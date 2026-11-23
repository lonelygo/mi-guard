// Package history persists bounded measurements and enforces the download
// budget across daemon and command-line processes.
package history

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/lonelygo/mi-guard/internal/model"
)

var ErrBudget = errors.New("speed-test download budget exhausted")

const stateVersion = 1

type Store struct {
	file       string
	window     int
	billingDay int
	now        func() time.Time
}

type Snapshot struct {
	Samples      []model.Sample       `json:"samples"`
	Heartbeats   []HeartbeatAggregate `json:"heartbeats,omitempty"`
	UsedBytes    int64                `json:"used_bytes"`
	PendingBytes int64                `json:"pending_bytes"`
	CycleStart   time.Time            `json:"cycle_start"`
	LastSweep    time.Time            `json:"last_sweep,omitempty"`
}

type HeartbeatAggregate struct {
	Scope       string    `json:"scope"`
	Attempts    int64     `json:"attempts"`
	Failures    int64     `json:"failures"`
	LastAt      time.Time `json:"last_time"`
	LastLatency int       `json:"last_latency_ms"`
	LastError   string    `json:"last_error,omitempty"`
}

type Reservation struct {
	ID         string    `json:"id"`
	Bytes      int64     `json:"bytes"`
	CycleStart time.Time `json:"cycle_start"`
}

type diskState struct {
	Version      int                           `json:"version"`
	CycleStart   time.Time                     `json:"cycle_start"`
	UsedBytes    int64                         `json:"used_bytes"`
	Reservations map[string]diskReservation    `json:"reservations,omitempty"`
	Finalized    map[string]time.Time          `json:"finalized,omitempty"`
	Previous     *cycleLedger                  `json:"previous_cycle,omitempty"`
	LastSweep    time.Time                     `json:"last_sweep,omitempty"`
	Samples      []model.Sample                `json:"samples,omitempty"`
	Heartbeats   map[string]HeartbeatAggregate `json:"heartbeats,omitempty"`
}

type cycleLedger struct {
	CycleStart   time.Time                  `json:"cycle_start"`
	UsedBytes    int64                      `json:"used_bytes"`
	Reservations map[string]diskReservation `json:"reservations,omitempty"`
	Finalized    map[string]time.Time       `json:"finalized,omitempty"`
}

type diskReservation struct {
	Bytes     int64     `json:"bytes"`
	CreatedAt time.Time `json:"created_at"`
}

func New(file string, window, billingDay int) *Store {
	if window < 1 {
		window = 1
	}
	if billingDay < 1 || billingDay > 28 {
		billingDay = 1
	}
	return &Store{
		file:       file,
		window:     window,
		billingDay: billingDay,
		now:        time.Now,
	}
}

// WithLock obtains a named cross-process lock. Callers can use separate
// "measure" and "apply" locks for long operations; state mutations use the
// short-lived internal "state" lock and therefore do not hold disk state open.
func (s *Store) WithLock(ctx context.Context, name string, fn func() error) error {
	if err := s.ensureDir(); err != nil {
		return err
	}
	lock, err := os.OpenFile(s.lockPath(name), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("open history lock: %w", err)
	}
	defer lock.Close()

	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			return fmt.Errorf("lock history: %w", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN) //nolint:errcheck

	if err := ctx.Err(); err != nil {
		return err
	}
	return fn()
}

func (s *Store) Snapshot(ctx context.Context) (Snapshot, error) {
	var result Snapshot
	err := s.WithLock(ctx, "state", func() error {
		state, changed, err := s.loadCurrent()
		if err != nil {
			return err
		}
		if changed {
			if err := s.write(state); err != nil {
				return err
			}
		}
		result = snapshotOf(state)
		return nil
	})
	return result, err
}

// Record stores a non-budgeted observation. Heartbeats are aggregated by
// scope; other samples join the bounded per-node bandwidth history.
func (s *Store) Record(ctx context.Context, sample model.Sample) error {
	return s.WithLock(ctx, "state", func() error {
		state, _, err := s.loadCurrent()
		if err != nil {
			return err
		}
		s.record(&state, sample)
		return s.write(state)
	})
}

// MarkSweep records the start of a real scheduled full scan. Cadence metadata
// is independent of measurements and remains intact across billing rollover.
func (s *Store) MarkSweep(ctx context.Context, at time.Time) error {
	if at.IsZero() {
		return errors.New("history sweep time is missing")
	}
	return s.WithLock(ctx, "state", func() error {
		state, _, err := s.loadCurrent()
		if err != nil {
			return err
		}
		state.LastSweep = at
		return s.write(state)
	})
}

// Reserve atomically pre-charges up to limit bytes. A smaller grant may be
// returned near the monthly limit. Pending reservations survive crashes and
// remain charged to their starting cycle until Finish. One prior cycle is
// retained so a measurement crossing the boundary cannot debit the new cycle.
func (s *Store) Reserve(ctx context.Context, limit, monthlyMax int64) (Reservation, error) {
	if limit <= 0 || monthlyMax <= 0 {
		return Reservation{}, ErrBudget
	}
	var result Reservation
	err := s.WithLock(ctx, "state", func() error {
		state, _, err := s.loadCurrent()
		if err != nil {
			return err
		}
		remaining := monthlyMax - state.UsedBytes
		if remaining <= 0 {
			return ErrBudget
		}
		grant := min(limit, remaining)
		id, err := reservationID()
		if err != nil {
			return err
		}
		state.UsedBytes += grant
		state.Reservations[id] = diskReservation{Bytes: grant, CreatedAt: s.now()}
		if err := s.write(state); err != nil {
			return err
		}
		result = Reservation{ID: id, Bytes: grant, CycleStart: state.CycleStart}
		return nil
	})
	return result, err
}

// Finish settles a reservation once and stores its result. Duplicate finishes
// are harmless. Actual bytes are always accounted, even if a buggy downloader
// exceeds its grant.
func (s *Store) Finish(ctx context.Context, reservation Reservation, sample model.Sample) error {
	return s.settle(ctx, reservation, sample.Bytes, &sample)
}

// Discard accounts an interrupted or unattributable transfer without treating
// operator cancellation or a wiring failure as evidence about any candidate.
func (s *Store) Discard(ctx context.Context, reservation Reservation, bytes int64) error {
	return s.settle(ctx, reservation, bytes, nil)
}

func (s *Store) settle(ctx context.Context, reservation Reservation, bytes int64, sample *model.Sample) error {
	if reservation.ID == "" {
		return errors.New("history reservation has no id")
	}
	return s.WithLock(ctx, "state", func() error {
		state, _, err := s.loadCurrent()
		if err != nil {
			return err
		}
		ledger, err := ledgerForReservation(&state, reservation.CycleStart)
		if err != nil {
			return err
		}
		if _, done := ledger.finalized[reservation.ID]; done {
			return nil
		}
		pending, ok := ledger.reservations[reservation.ID]
		if !ok {
			return errors.New("history reservation is unknown")
		}
		actual := max(bytes, int64(0))
		*ledger.usedBytes += actual - pending.Bytes
		if *ledger.usedBytes < 0 {
			return errors.New("history budget settlement underflow")
		}
		delete(ledger.reservations, reservation.ID)
		ledger.finalized[reservation.ID] = s.now()
		if sample != nil {
			sample.Bytes = actual
			s.record(&state, *sample)
		}
		return s.write(state)
	})
}

func (s *Store) record(state *diskState, sample model.Sample) {
	if sample.At.IsZero() {
		sample.At = s.now()
	}
	if sample.Kind == "heartbeat" {
		key := sample.Scope
		if key == "" {
			key = sample.Node
		}
		agg := state.Heartbeats[key]
		agg.Scope = key
		agg.Attempts += int64(max(sample.Attempts, 1))
		agg.Failures += int64(max(sample.Failures, boolInt(sample.Error != "")))
		agg.LastAt = sample.At
		agg.LastLatency = sample.LatencyMS
		agg.LastError = sample.Error
		state.Heartbeats[key] = agg
		return
	}
	state.Samples = append(state.Samples, sample)
	state.Samples = retainSamples(state.Samples, s.window)
}

func (s *Store) loadCurrent() (diskState, bool, error) {
	state, exists, err := s.load()
	if err != nil {
		return diskState{}, false, err
	}
	now := s.now()
	start := cycleStart(now, s.billingDay)
	if !exists {
		return newDiskState(start), true, nil
	}
	if state.Version != stateVersion {
		return diskState{}, false, fmt.Errorf("unsupported history version %d", state.Version)
	}
	normalizeState(&state)
	if err := validateState(state, s.billingDay); err != nil {
		return diskState{}, false, err
	}
	if state.CycleStart.After(start) {
		return diskState{}, false, errors.New("history billing cycle is in the future")
	}
	if !state.CycleStart.Equal(start) {
		state.Previous = &cycleLedger{
			CycleStart:   state.CycleStart,
			UsedBytes:    state.UsedBytes,
			Reservations: state.Reservations,
			Finalized:    state.Finalized,
		}
		state.CycleStart = start
		state.UsedBytes = 0
		state.Reservations = make(map[string]diskReservation)
		state.Finalized = make(map[string]time.Time)
		return state, true, nil
	}
	return state, false, nil
}

func (s *Store) load() (diskState, bool, error) {
	file, err := os.Open(s.file)
	if errors.Is(err, os.ErrNotExist) {
		return diskState{}, false, nil
	}
	if err != nil {
		return diskState{}, false, fmt.Errorf("open history: %w", err)
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	var state diskState
	if err := decoder.Decode(&state); err != nil {
		return diskState{}, false, fmt.Errorf("decode history: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("multiple JSON values")
		}
		return diskState{}, false, fmt.Errorf("decode history: %w", err)
	}
	return state, true, nil
}

func (s *Store) write(state diskState) error {
	if err := s.ensureDir(); err != nil {
		return err
	}
	dir := filepath.Dir(s.file)
	temp, err := os.CreateTemp(dir, ".speed-history-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary history: %w", err)
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return fmt.Errorf("protect temporary history: %w", err)
	}
	encoder := json.NewEncoder(temp)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(state); err != nil {
		temp.Close()
		return fmt.Errorf("encode history: %w", err)
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return fmt.Errorf("sync history: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close history: %w", err)
	}
	if err := os.Rename(tempName, s.file); err != nil {
		return fmt.Errorf("replace history: %w", err)
	}
	return nil
}

func (s *Store) ensureDir() error {
	dir := filepath.Dir(s.file)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create history directory: %w", err)
	}
	return nil
}

func (s *Store) lockPath(name string) string {
	var clean strings.Builder
	for _, char := range name {
		if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '-' || char == '_' {
			clean.WriteRune(char)
		} else {
			clean.WriteByte('_')
		}
	}
	if clean.Len() == 0 {
		clean.WriteString("default")
	}
	return s.file + "." + clean.String() + ".lock"
}

func snapshotOf(state diskState) Snapshot {
	pending := int64(0)
	for _, item := range state.Reservations {
		pending += item.Bytes
	}
	heartbeats := make([]HeartbeatAggregate, 0, len(state.Heartbeats))
	for _, heartbeat := range state.Heartbeats {
		heartbeats = append(heartbeats, heartbeat)
	}
	sort.Slice(heartbeats, func(i, j int) bool { return heartbeats[i].Scope < heartbeats[j].Scope })
	return Snapshot{
		Samples:      append([]model.Sample(nil), state.Samples...),
		Heartbeats:   heartbeats,
		UsedBytes:    state.UsedBytes,
		PendingBytes: pending,
		CycleStart:   state.CycleStart,
		LastSweep:    state.LastSweep,
	}
}

func newDiskState(start time.Time) diskState {
	return diskState{
		Version:      stateVersion,
		CycleStart:   start,
		Reservations: make(map[string]diskReservation),
		Finalized:    make(map[string]time.Time),
		Heartbeats:   make(map[string]HeartbeatAggregate),
	}
}

func normalizeState(state *diskState) {
	if state.Reservations == nil {
		state.Reservations = make(map[string]diskReservation)
	}
	if state.Finalized == nil {
		state.Finalized = make(map[string]time.Time)
	}
	if state.Heartbeats == nil {
		state.Heartbeats = make(map[string]HeartbeatAggregate)
	}
	if state.Previous != nil {
		if state.Previous.Reservations == nil {
			state.Previous.Reservations = make(map[string]diskReservation)
		}
		if state.Previous.Finalized == nil {
			state.Previous.Finalized = make(map[string]time.Time)
		}
	}
}

type ledgerRefs struct {
	usedBytes    *int64
	reservations map[string]diskReservation
	finalized    map[string]time.Time
}

func ledgerForReservation(state *diskState, start time.Time) (ledgerRefs, error) {
	if start.IsZero() {
		return ledgerRefs{}, errors.New("history reservation has no billing cycle")
	}
	if start.Equal(state.CycleStart) {
		return ledgerRefs{&state.UsedBytes, state.Reservations, state.Finalized}, nil
	}
	if state.Previous != nil && start.Equal(state.Previous.CycleStart) {
		return ledgerRefs{&state.Previous.UsedBytes, state.Previous.Reservations, state.Previous.Finalized}, nil
	}
	return ledgerRefs{}, errors.New("history reservation belongs to an unsupported billing cycle")
}

func validateState(state diskState, billingDay int) error {
	if err := validateCycle("current", state.CycleStart, state.UsedBytes, state.Reservations, state.Finalized, billingDay); err != nil {
		return err
	}
	if state.Previous != nil {
		if !state.Previous.CycleStart.Before(state.CycleStart) {
			return errors.New("previous history billing cycle is not before current cycle")
		}
		if err := validateCycle("previous", state.Previous.CycleStart, state.Previous.UsedBytes, state.Previous.Reservations, state.Previous.Finalized, billingDay); err != nil {
			return err
		}
	}
	return nil
}

func validateCycle(name string, start time.Time, used int64, reservations map[string]diskReservation, finalized map[string]time.Time, billingDay int) error {
	if start.IsZero() {
		return fmt.Errorf("%s history billing cycle is missing", name)
	}
	if start.Day() != billingDay || start.Hour() != 0 || start.Minute() != 0 || start.Second() != 0 || start.Nanosecond() != 0 {
		return fmt.Errorf("%s history billing cycle is malformed", name)
	}
	if used < 0 {
		return fmt.Errorf("%s history used bytes is negative", name)
	}
	pending := int64(0)
	for id, reservation := range reservations {
		if id == "" || reservation.Bytes <= 0 || reservation.CreatedAt.IsZero() {
			return fmt.Errorf("%s history reservation is malformed", name)
		}
		if pending > int64(^uint64(0)>>1)-reservation.Bytes {
			return fmt.Errorf("%s history reservations overflow", name)
		}
		pending += reservation.Bytes
		if _, conflict := finalized[id]; conflict {
			return fmt.Errorf("%s history reservation is both pending and finalized", name)
		}
	}
	if pending > used {
		return fmt.Errorf("%s history pending bytes exceeds used bytes", name)
	}
	for id, finishedAt := range finalized {
		if id == "" || finishedAt.IsZero() {
			return fmt.Errorf("%s finalized history reservation is malformed", name)
		}
	}
	return nil
}

func retainSamples(samples []model.Sample, window int) []model.Sample {
	byNode := make(map[string][]model.Sample)
	for _, sample := range samples {
		key := sample.Provider + "\x00" + sample.Node
		byNode[key] = append(byNode[key], sample)
	}
	result := make([]model.Sample, 0, len(samples))
	for _, group := range byNode {
		sort.SliceStable(group, func(i, j int) bool { return group[i].At.Before(group[j].At) })
		if len(group) > window {
			group = group[len(group)-window:]
		}
		result = append(result, group...)
	}
	sort.SliceStable(result, func(i, j int) bool { return result[i].At.Before(result[j].At) })
	return result
}

func cycleStart(now time.Time, billingDay int) time.Time {
	year, month := now.Year(), now.Month()
	if now.Day() < billingDay {
		month--
		if month < time.January {
			month = time.December
			year--
		}
	}
	return time.Date(year, month, billingDay, 0, 0, 0, 0, now.Location())
}

func reservationID() (string, error) {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", fmt.Errorf("create reservation id: %w", err)
	}
	return hex.EncodeToString(bytes[:]), nil
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
