//go:build cgo

package history

/*
#cgo darwin LDFLAGS: -lsqlite3
#cgo linux LDFLAGS: -lsqlite3
#include <stdlib.h>
#include <sqlite3.h>

static int mi_bind_text(sqlite3_stmt *stmt, int index, const char *value, int size) {
	return sqlite3_bind_text(stmt, index, value, size, SQLITE_TRANSIENT);
}

static int mi_bind_blob(sqlite3_stmt *stmt, int index, const void *value, int size) {
	return sqlite3_bind_blob(stmt, index, value, size, SQLITE_TRANSIENT);
}
*/
import "C"

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"strings"
	"time"
	"unsafe"
)

const analyticsVersion = 1

const analyticsSchema = "CREATE TABLE IF NOT EXISTS decisions (id TEXT PRIMARY KEY NOT NULL, at_ns INTEGER NOT NULL, document BLOB NOT NULL);" +
	"CREATE INDEX IF NOT EXISTS decisions_at ON decisions(at_ns DESC);" +
	"CREATE TABLE IF NOT EXISTS route_ingest (source_id TEXT PRIMARY KEY NOT NULL, at_ns INTEGER NOT NULL, digest BLOB NOT NULL);" +
	"CREATE INDEX IF NOT EXISTS route_ingest_at ON route_ingest(at_ns);" +
	"CREATE TABLE IF NOT EXISTS route_buckets (" +
	"start_ns INTEGER NOT NULL, entry TEXT NOT NULL, exit TEXT NOT NULL, region_source TEXT NOT NULL, target TEXT NOT NULL, chain TEXT NOT NULL," +
	"download_bytes INTEGER NOT NULL CHECK(download_bytes >= 0)," +
	"upload_bytes INTEGER NOT NULL CHECK(upload_bytes >= 0)," +
	"connections INTEGER NOT NULL CHECK(connections >= 0)," +
	"PRIMARY KEY(start_ns, entry, exit, region_source, target, chain));" +
	"CREATE INDEX IF NOT EXISTS route_buckets_start ON route_buckets(start_ns);" +
	"PRAGMA user_version = 1;"

func (s *Store) RecordDecision(ctx context.Context, decision Decision) error {
	if decision.ID = strings.TrimSpace(decision.ID); decision.ID == "" {
		return errors.New("decision event id is missing")
	}
	if len(decision.ID) > 256 || strings.ContainsAny(decision.ID, "\r\n\x00") {
		return errors.New("decision event id is invalid")
	}
	if decision.At.IsZero() {
		return errors.New("decision event time is missing")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	decision.At = decision.At.UTC()
	document, err := json.Marshal(decision)
	if err != nil {
		return fmt.Errorf("encode decision event: %w", err)
	}
	db, _, err := s.openAnalytics(true)
	if err != nil {
		return err
	}
	defer C.sqlite3_close(db)
	return withAnalyticsTransaction(ctx, db, func() error {
		inserted, err := execChanged(db,
			"INSERT OR IGNORE INTO decisions(id, at_ns, document) VALUES(?, ?, ?)",
			decision.ID, decision.At.UnixNano(), document)
		if err != nil {
			return err
		}
		if !inserted {
			existing, found, err := decisionDocument(db, decision.ID)
			if err != nil {
				return err
			}
			if !found || !bytes.Equal(existing, document) {
				return errors.New("decision event id conflicts with different content")
			}
		}
		_, err = execChanged(db, "DELETE FROM decisions WHERE id IN (SELECT id FROM decisions ORDER BY at_ns DESC, id DESC LIMIT -1 OFFSET ?)", int64(decisionRetention))
		return err
	})
}

func (s *Store) RecentDecisions(ctx context.Context, limit int) ([]Decision, error) {
	if limit < 0 {
		return nil, errors.New("decision limit cannot be negative")
	}
	if limit == 0 || limit > decisionRetention {
		limit = decisionRetention
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	db, exists, err := s.openAnalytics(false)
	if err != nil || !exists {
		return nil, err
	}
	defer C.sqlite3_close(db)
	stmt, err := prepare(db, "SELECT document FROM decisions ORDER BY at_ns DESC, id DESC LIMIT ?")
	if err != nil {
		return nil, err
	}
	defer C.sqlite3_finalize(stmt)
	if err := bind(stmt, int64(limit)); err != nil {
		return nil, err
	}
	result := make([]Decision, 0, min(limit, 32))
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		code := C.sqlite3_step(stmt)
		if code == C.SQLITE_DONE {
			return result, nil
		}
		if code != C.SQLITE_ROW {
			return nil, sqliteError(db, "read decisions", code)
		}
		decision, err := decodeDecision(columnBytes(stmt, 0))
		if err != nil {
			return nil, err
		}
		result = append(result, decision)
	}
}

func (s *Store) DecisionByID(ctx context.Context, id string) (Decision, error) {
	if id = strings.TrimSpace(id); id == "" || len(id) > 256 || strings.ContainsAny(id, "\r\n\x00") {
		return Decision{}, ErrDecisionNotFound
	}
	if err := ctx.Err(); err != nil {
		return Decision{}, err
	}
	db, exists, err := s.openAnalytics(false)
	if err != nil {
		return Decision{}, err
	}
	if !exists {
		return Decision{}, ErrDecisionNotFound
	}
	defer C.sqlite3_close(db)
	document, found, err := decisionDocument(db, id)
	if err != nil {
		return Decision{}, err
	}
	if !found {
		return Decision{}, ErrDecisionNotFound
	}
	return decodeDecision(document)
}

func (s *Store) addRouteBuckets(ctx context.Context, buckets []RouteBucket, sourceID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	db, _, err := s.openAnalytics(true)
	if err != nil {
		return err
	}
	defer C.sqlite3_close(db)
	document, err := json.Marshal(buckets)
	if err != nil {
		return fmt.Errorf("encode route batch: %w", err)
	}
	digest := sha256.Sum256(document)
	return withAnalyticsTransaction(ctx, db, func() error {
		at := s.now().UTC()
		for _, bucket := range buckets {
			if bucket.Start.After(at) {
				at = bucket.Start
			}
		}
		inserted, err := execChanged(db, "INSERT OR IGNORE INTO route_ingest(source_id, at_ns, digest) VALUES(?, ?, ?)", sourceID, at.UnixNano(), digest[:])
		if err != nil {
			return err
		}
		if !inserted {
			existing, found, err := routeIngestDigest(db, sourceID)
			if err != nil {
				return err
			}
			if !found || !bytes.Equal(existing, digest[:]) {
				return errors.New("route bucket source id conflicts with different content")
			}
			return nil
		}
		for _, bucket := range buckets {
			entry := routeDimension(bucket.Entry)
			current, found, err := routeBucketByKey(db, bucket)
			if err != nil {
				return err
			}
			if found {
				if _, ok := safeCounterAdd(current.DownloadBytes, bucket.DownloadBytes); !ok {
					return errors.New("route download counter overflow")
				}
				if _, ok := safeCounterAdd(current.UploadBytes, bucket.UploadBytes); !ok {
					return errors.New("route upload counter overflow")
				}
				if _, ok := safeCounterAdd(current.Connections, bucket.Connections); !ok {
					return errors.New("route connection counter overflow")
				}
			}
			query := "INSERT INTO route_buckets(start_ns, entry, exit, region_source, target, chain, download_bytes, upload_bytes, connections) " +
				"VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?) " +
				"ON CONFLICT(start_ns, entry, exit, region_source, target, chain) DO UPDATE SET " +
				"download_bytes = download_bytes + excluded.download_bytes, " +
				"upload_bytes = upload_bytes + excluded.upload_bytes, " +
				"connections = connections + excluded.connections"
			if _, err := execChanged(db, query,
				bucket.Start.UnixNano(), entry, bucket.Exit, bucket.RegionSource, bucket.Target, bucket.Chain,
				bucket.DownloadBytes, bucket.UploadBytes, bucket.Connections); err != nil {
				return err
			}
		}
		cutoff := s.now().UTC().Add(-routeRetention).UnixNano()
		if _, err := execChanged(db, "DELETE FROM route_buckets WHERE start_ns < ?", cutoff); err != nil {
			return err
		}
		_, err = execChanged(db, "DELETE FROM route_ingest WHERE at_ns < ?", cutoff)
		return err
	})
}

func (s *Store) routeBuckets(ctx context.Context, since, until time.Time, interval RouteInterval) ([]RouteBucket, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	db, exists, err := s.openAnalytics(false)
	if err != nil || !exists {
		return nil, err
	}
	defer C.sqlite3_close(db)
	width := int64(time.Minute)
	switch interval {
	case RouteHour:
		width = int64(time.Hour)
	case RouteDay:
		width = int64(24 * time.Hour)
	}
	timeExpression := "(start_ns / ?) * ?"
	args := []any{width, width}
	if interval == RoutePeriod {
		timeExpression = "?"
		args = []any{since.UnixNano()}
	}
	query := "SELECT " + timeExpression + ", entry, exit, region_source, target, chain, " +
		"SUM(download_bytes), SUM(upload_bytes), SUM(connections) FROM route_buckets"
	var predicates []string
	if !since.IsZero() {
		predicates = append(predicates, "start_ns >= ?")
		args = append(args, since.UnixNano())
	}
	if !until.IsZero() {
		predicates = append(predicates, "start_ns < ?")
		args = append(args, until.UnixNano())
	}
	if len(predicates) > 0 {
		query += " WHERE " + strings.Join(predicates, " AND ")
	}
	query += " GROUP BY 1, 2, exit, region_source, target, chain ORDER BY 1, 2, exit, region_source, target, chain"
	stmt, err := prepare(db, query)
	if err != nil {
		return nil, err
	}
	defer C.sqlite3_finalize(stmt)
	if err := bind(stmt, args...); err != nil {
		return nil, err
	}
	var result []RouteBucket
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		code := C.sqlite3_step(stmt)
		if code == C.SQLITE_DONE {
			return result, nil
		}
		if code != C.SQLITE_ROW {
			return nil, sqliteError(db, "read route buckets", code)
		}
		result = append(result, RouteBucket{
			Start:         time.Unix(0, int64(C.sqlite3_column_int64(stmt, 0))).UTC(),
			Entry:         columnString(stmt, 1),
			Exit:          columnString(stmt, 2),
			RegionSource:  columnString(stmt, 3),
			Target:        columnString(stmt, 4),
			Chain:         columnString(stmt, 5),
			DownloadBytes: int64(C.sqlite3_column_int64(stmt, 6)),
			UploadBytes:   int64(C.sqlite3_column_int64(stmt, 7)),
			Connections:   int64(C.sqlite3_column_int64(stmt, 8)),
		})
	}
}

func (s *Store) openAnalytics(write bool) (*C.sqlite3, bool, error) {
	path := s.analyticsPath()
	if !write {
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		} else if err != nil {
			return nil, false, fmt.Errorf("stat analytics database: %w", err)
		}
	} else {
		if err := s.ensureDir(); err != nil {
			return nil, false, err
		}
		created, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
		if err == nil {
			if err := created.Close(); err != nil {
				return nil, false, fmt.Errorf("close new analytics database: %w", err)
			}
		} else if !errors.Is(err, os.ErrExist) {
			return nil, false, fmt.Errorf("create analytics database: %w", err)
		}
	}
	cpath := C.CString(path)
	defer C.free(unsafe.Pointer(cpath))
	flags := C.int(C.SQLITE_OPEN_READONLY | C.SQLITE_OPEN_FULLMUTEX)
	if write {
		flags = C.SQLITE_OPEN_READWRITE | C.SQLITE_OPEN_CREATE | C.SQLITE_OPEN_FULLMUTEX
	}
	var db *C.sqlite3
	if code := C.sqlite3_open_v2(cpath, &db, flags, nil); code != C.SQLITE_OK {
		err := sqliteError(db, "open analytics database", code)
		if db != nil {
			C.sqlite3_close(db)
		}
		return nil, false, err
	}
	C.sqlite3_extended_result_codes(db, 1)
	C.sqlite3_busy_timeout(db, 2500)
	if write {
		if err := execSQL(db, "PRAGMA synchronous=FULL;"); err != nil {
			C.sqlite3_close(db)
			return nil, false, err
		}
	}
	version, err := queryInteger(db, "PRAGMA user_version")
	if err != nil {
		C.sqlite3_close(db)
		return nil, false, err
	}
	if version == 0 && write {
		if err := withAnalyticsTransaction(context.Background(), db, func() error {
			current, err := queryInteger(db, "PRAGMA user_version")
			if err != nil {
				return err
			}
			if current == analyticsVersion {
				return nil
			}
			if current != 0 {
				return fmt.Errorf("unsupported analytics database version %d", current)
			}
			tables, err := queryInteger(db, "SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'")
			if err != nil {
				return err
			}
			if tables != 0 {
				return errors.New("analytics database has an unversioned schema")
			}
			return execSQL(db, analyticsSchema)
		}); err != nil {
			C.sqlite3_close(db)
			return nil, false, err
		}
		version = analyticsVersion
	}
	if version != analyticsVersion {
		C.sqlite3_close(db)
		return nil, false, fmt.Errorf("unsupported analytics database version %d", version)
	}
	return db, true, nil
}

func withAnalyticsTransaction(ctx context.Context, db *C.sqlite3, fn func() error) (err error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := execSQL(db, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = execSQL(db, "ROLLBACK")
		}
	}()
	if err := fn(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := execSQL(db, "COMMIT"); err != nil {
		return err
	}
	committed = true
	return nil
}

func decisionDocument(db *C.sqlite3, id string) ([]byte, bool, error) {
	stmt, err := prepare(db, "SELECT document FROM decisions WHERE id = ?")
	if err != nil {
		return nil, false, err
	}
	defer C.sqlite3_finalize(stmt)
	if err := bind(stmt, id); err != nil {
		return nil, false, err
	}
	switch code := C.sqlite3_step(stmt); code {
	case C.SQLITE_ROW:
		return columnBytes(stmt, 0), true, nil
	case C.SQLITE_DONE:
		return nil, false, nil
	default:
		return nil, false, sqliteError(db, "read decision event", code)
	}
}

func routeBucketByKey(db *C.sqlite3, bucket RouteBucket) (RouteBucket, bool, error) {
	query := "SELECT download_bytes, upload_bytes, connections FROM route_buckets " +
		"WHERE start_ns = ? AND entry = ? AND exit = ? AND region_source = ? AND target = ? AND chain = ?"
	stmt, err := prepare(db, query)
	if err != nil {
		return RouteBucket{}, false, err
	}
	defer C.sqlite3_finalize(stmt)
	if err := bind(stmt, bucket.Start.UnixNano(), routeDimension(bucket.Entry), bucket.Exit, bucket.RegionSource, bucket.Target, bucket.Chain); err != nil {
		return RouteBucket{}, false, err
	}
	switch code := C.sqlite3_step(stmt); code {
	case C.SQLITE_ROW:
		return RouteBucket{
			DownloadBytes: int64(C.sqlite3_column_int64(stmt, 0)),
			UploadBytes:   int64(C.sqlite3_column_int64(stmt, 1)),
			Connections:   int64(C.sqlite3_column_int64(stmt, 2)),
		}, true, nil
	case C.SQLITE_DONE:
		return RouteBucket{}, false, nil
	default:
		return RouteBucket{}, false, sqliteError(db, "read route bucket", code)
	}
}

func routeIngestDigest(db *C.sqlite3, sourceID string) ([]byte, bool, error) {
	stmt, err := prepare(db, "SELECT digest FROM route_ingest WHERE source_id = ?")
	if err != nil {
		return nil, false, err
	}
	defer C.sqlite3_finalize(stmt)
	if err := bind(stmt, sourceID); err != nil {
		return nil, false, err
	}
	switch code := C.sqlite3_step(stmt); code {
	case C.SQLITE_ROW:
		return columnBytes(stmt, 0), true, nil
	case C.SQLITE_DONE:
		return nil, false, nil
	default:
		return nil, false, sqliteError(db, "read route ingest", code)
	}
}

func decodeDecision(document []byte) (Decision, error) {
	var decision Decision
	decoder := json.NewDecoder(bytes.NewReader(document))
	if err := decoder.Decode(&decision); err != nil {
		return Decision{}, fmt.Errorf("decode decision event: %w", err)
	}
	if decision.ID == "" || decision.At.IsZero() {
		return Decision{}, errors.New("decode decision event: incomplete record")
	}
	return decision, nil
}

func queryInteger(db *C.sqlite3, query string) (int, error) {
	stmt, err := prepare(db, query)
	if err != nil {
		return 0, err
	}
	defer C.sqlite3_finalize(stmt)
	if code := C.sqlite3_step(stmt); code != C.SQLITE_ROW {
		return 0, sqliteError(db, "read analytics metadata", code)
	}
	return int(C.sqlite3_column_int(stmt, 0)), nil
}

func execSQL(db *C.sqlite3, query string) error {
	cquery := C.CString(query)
	defer C.free(unsafe.Pointer(cquery))
	var message *C.char
	if code := C.sqlite3_exec(db, cquery, nil, nil, &message); code != C.SQLITE_OK {
		var detail string
		if message != nil {
			detail = C.GoString(message)
			C.sqlite3_free(unsafe.Pointer(message))
		}
		if detail == "" {
			return sqliteError(db, "execute analytics query", code)
		}
		return fmt.Errorf("execute analytics query: %s", detail)
	}
	return nil
}

func execChanged(db *C.sqlite3, query string, args ...any) (bool, error) {
	stmt, err := prepare(db, query)
	if err != nil {
		return false, err
	}
	defer C.sqlite3_finalize(stmt)
	if err := bind(stmt, args...); err != nil {
		return false, err
	}
	if code := C.sqlite3_step(stmt); code != C.SQLITE_DONE {
		return false, sqliteError(db, "write analytics database", code)
	}
	return C.sqlite3_changes(db) > 0, nil
}

func prepare(db *C.sqlite3, query string) (*C.sqlite3_stmt, error) {
	cquery := C.CString(query)
	defer C.free(unsafe.Pointer(cquery))
	var stmt *C.sqlite3_stmt
	if code := C.sqlite3_prepare_v2(db, cquery, -1, &stmt, nil); code != C.SQLITE_OK {
		return nil, sqliteError(db, "prepare analytics query", code)
	}
	return stmt, nil
}

func bind(stmt *C.sqlite3_stmt, args ...any) error {
	for index, value := range args {
		position := C.int(index + 1)
		var code C.int
		switch typed := value.(type) {
		case string:
			cvalue := C.CString(typed)
			code = C.mi_bind_text(stmt, position, cvalue, C.int(len(typed)))
			C.free(unsafe.Pointer(cvalue))
		case int:
			code = C.sqlite3_bind_int64(stmt, position, C.sqlite3_int64(typed))
		case int64:
			code = C.sqlite3_bind_int64(stmt, position, C.sqlite3_int64(typed))
		case []byte:
			if len(typed) == 0 {
				code = C.mi_bind_blob(stmt, position, nil, 0)
			} else {
				code = C.mi_bind_blob(stmt, position, unsafe.Pointer(&typed[0]), C.int(len(typed)))
			}
		default:
			return fmt.Errorf("unsupported analytics argument %T", value)
		}
		if code != C.SQLITE_OK {
			return fmt.Errorf("bind analytics argument %d: sqlite code %d", index+1, int(code))
		}
	}
	return nil
}

func columnBytes(stmt *C.sqlite3_stmt, column C.int) []byte {
	size := C.sqlite3_column_bytes(stmt, column)
	if size == 0 {
		return []byte{}
	}
	return C.GoBytes(C.sqlite3_column_blob(stmt, column), size)
}

func columnString(stmt *C.sqlite3_stmt, column C.int) string {
	return string(columnBytes(stmt, column))
}

func sqliteError(db *C.sqlite3, operation string, code C.int) error {
	if db == nil {
		return fmt.Errorf("%s: sqlite code %d", operation, int(code))
	}
	return fmt.Errorf("%s: %s (sqlite code %d)", operation, C.GoString(C.sqlite3_errmsg(db)), int(code))
}

func safeCounterAdd(left, right int64) (int64, bool) {
	if right > 0 && left > math.MaxInt64-right {
		return 0, false
	}
	return left + right, true
}

func closeAnalytics(db *C.sqlite3) { C.sqlite3_close(db) }
