package flight

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
)

// The fixture encoders below implement only the fields used by these tests.
// Layout and pointer arithmetic follow the MaxMind DB v2 format specification:
// https://maxmind.github.io/MaxMind-DB/
func TestCountryDBRecordWidthsAndIPv4InIPv6(t *testing.T) {
	records := map[string][]byte{
		"sing string": mmdbString("JP"),
		"Meta array":  mmdbArray(mmdbString("US"), mmdbString("US")),
		"GeoIP2 map": mmdbMap(
			"country", mmdbMap("iso_code", mmdbString("GB")),
		),
		"data pointer": append(mmdbPointer(2), mmdbString("DE")...),
	}
	for _, bits := range []int{24, 28, 32} {
		for _, version := range []int{4, 6} {
			for name, record := range records {
				t.Run(fmt.Sprintf("%s/%d/IPv%d", name, bits, version), func(t *testing.T) {
					path := writeMMDB(t, bits, version, record, true)
					db, err := openCountry(path)
					if err != nil {
						t.Fatal(err)
					}
					defer db.close()
					want := map[string]string{"sing string": "JP", "Meta array": "US", "GeoIP2 map": "GB", "data pointer": "DE"}[name]
					if got := db.country(netip.MustParseAddr("1.1.1.1")); got != want {
						t.Fatalf("IPv4 country = %q, want %q", got, want)
					}
					if got := db.country(netip.MustParseAddr("::ffff:1.1.1.1")); got != want {
						t.Fatalf("mapped IPv4 country = %q, want %q", got, want)
					}
					if version == 6 {
						if got := db.country(netip.MustParseAddr("2001:4860:4860::8888")); got != want {
							t.Fatalf("IPv6 country = %q, want %q", got, want)
						}
					} else if got := db.country(netip.MustParseAddr("2001:4860:4860::8888")); got != "" {
						t.Fatalf("IPv4 database returned IPv6 country %q", got)
					}
				})
			}
		}
	}
}

func TestCountryDBTwentyEightBitChildLayout(t *testing.T) {
	left, right := 0x0a123456, 0x0b654321
	tree := mmdbNode(28, left, right)
	path := filepath.Join(t.TempDir(), "tree")
	if err := os.WriteFile(path, tree, 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	db := &CountryDB{file: file, size: len(tree), nodes: 1, width: 7, pages: make(map[int][]byte)}
	if got, ok := db.child(0, 0); !ok || got != left {
		t.Fatalf("left child = %#x, %v", got, ok)
	}
	if got, ok := db.child(0, 1); !ok || got != right {
		t.Fatalf("right child = %#x, %v", got, ok)
	}
}

func TestCountryDBPointerSizes(t *testing.T) {
	for size, offset := range map[int]int{1: 10, 2: 2048, 3: 526336, 4: 256} {
		t.Run(fmt.Sprintf("%d-byte", size), func(t *testing.T) {
			pointer := mmdbPointerSized(offset, size)
			data := append([]byte{}, pointer...)
			data = append(data, make([]byte, offset-len(data))...)
			data = append(data, mmdbString("CA")...)
			db, err := openCountry(writeMMDB(t, 24, 4, data, true))
			if err != nil {
				t.Fatal(err)
			}
			defer db.close()
			if got := db.country(netip.MustParseAddr("1.1.1.1")); got != "CA" {
				t.Fatalf("country through %d-byte pointer = %q", size, got)
			}
		})
	}
}

func TestCountryDBRejectsPrivateBenchmarkMissingAndAmbiguousRecords(t *testing.T) {
	db, err := openCountry(writeMMDB(t, 24, 6, mmdbString("US"), true))
	if err != nil {
		t.Fatal(err)
	}
	defer db.close()
	for _, raw := range []string{"10.0.0.1", "::ffff:10.0.0.1", "192.168.1.1", "127.0.0.1", "198.18.0.1", "198.19.255.254", "fc00::1", "fe80::1"} {
		if got := db.country(netip.MustParseAddr(raw)); got != "" {
			t.Errorf("non-public %s returned %q", raw, got)
		}
	}
	missing, err := openCountry(writeMMDB(t, 24, 4, nil, false))
	if err != nil {
		t.Fatal(err)
	}
	defer missing.close()
	if got := missing.country(netip.MustParseAddr("1.1.1.1")); got != "" {
		t.Fatalf("missing record returned %q", got)
	}
	ambiguous, err := openCountry(writeMMDB(t, 24, 4, mmdbArray(mmdbString("US"), mmdbString("DE")), true))
	if err != nil {
		t.Fatal(err)
	}
	defer ambiguous.close()
	if got := ambiguous.country(netip.MustParseAddr("1.1.1.1")); got != "" {
		t.Fatalf("ambiguous Meta record returned %q", got)
	}
	for _, field := range []string{"registered_country", "represented_country"} {
		db, err := openCountry(writeMMDB(t, 24, 4, mmdbMap(field, mmdbMap("iso_code", mmdbString("US"))), true))
		if err != nil {
			t.Fatal(err)
		}
		if got := db.country(netip.MustParseAddr("1.1.1.1")); got != "" {
			db.close()
			t.Fatalf("%s was treated as physical country %q", field, got)
		}
		db.close()
	}
}

func TestCountryDBMetadataMarkerUsesAbsoluteOffset(t *testing.T) {
	marker := []byte("\xab\xcd\xefMaxMind.com")
	padding := 120000
	var contents []byte
	for range 3 {
		contents = mmdbContentsWithPadding(24, 4, mmdbString("US"), true, padding)
		markerAt := bytes.LastIndex(contents, marker)
		afterMarker := len(contents) - markerAt
		padding += 128<<10 - 8 - afterMarker
	}
	contents = mmdbContentsWithPadding(24, 4, mmdbString("US"), true, padding)
	markerAt := bytes.LastIndex(contents, marker)
	tailStart := len(contents) - (128 << 10)
	if markerAt-tailStart != 8 {
		t.Fatalf("marker tail offset = %d, want 8", markerAt-tailStart)
	}
	path := filepath.Join(t.TempDir(), "marker-near-tail-start.mmdb")
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := openCountry(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.close()
	if got := db.country(netip.MustParseAddr("1.1.1.1")); got != "US" {
		t.Fatalf("country with early tail marker = %q", got)
	}
}

func TestCountryDBBoundsPointersCorruptionPermissionsAndConcurrency(t *testing.T) {
	cycle := mmdbPointer(0)
	db, err := openCountry(writeMMDB(t, 24, 4, cycle, true))
	if err != nil {
		t.Fatal(err)
	}
	if got := db.country(netip.MustParseAddr("1.1.1.1")); got != "" {
		t.Fatalf("cyclic pointer returned %q", got)
	}
	db.close()

	corrupt := filepath.Join(t.TempDir(), "corrupt.mmdb")
	if err := os.WriteFile(corrupt, []byte("not a maxmind database"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := openCountry(corrupt); err == nil {
		t.Fatal("corrupt database was accepted")
	}

	if runtime.GOOS != "windows" && os.Geteuid() != 0 {
		blocked := writeMMDB(t, 24, 4, mmdbString("US"), true)
		if err := os.Chmod(blocked, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(blocked, 0o600) })
		if _, err := openCountry(blocked); err == nil {
			t.Fatal("unreadable database was accepted")
		}
	}

	concurrent, err := openCountry(writeMMDB(t, 32, 6, mmdbMap("country", mmdbMap("iso_code", mmdbString("GB"))), true))
	if err != nil {
		t.Fatal(err)
	}
	defer concurrent.close()
	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 50 {
				if got := concurrent.country(netip.MustParseAddr("81.2.69.160")); got != "GB" {
					t.Errorf("concurrent lookup = %q", got)
					return
				}
			}
		}()
	}
	wg.Wait()
}

func TestOfficialGeoIP2CountryFixture(t *testing.T) {
	path := os.Getenv("MI_GUARD_OFFICIAL_COUNTRY_DB")
	if path == "" {
		t.Skip("set MI_GUARD_OFFICIAL_COUNTRY_DB to MaxMind's GeoIP2-Country-Test.mmdb")
	}
	db, err := openCountry(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.close()
	// MaxMind's own GeoIP2 integration tests use this public fixture address
	// and expect the United Kingdom.
	if got := db.country(netip.MustParseAddr("81.2.69.160")); got != "GB" {
		t.Fatalf("official fixture country = %q, want GB", got)
	}
}

func TestLocalCountryDatabasePublicIPs(t *testing.T) {
	path := os.Getenv("MI_GUARD_LOCAL_COUNTRY_DB")
	if path == "" {
		t.Skip("set MI_GUARD_LOCAL_COUNTRY_DB for a read-only local compatibility check")
	}
	db, err := openCountry(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.close()
	resolved := 0
	for _, raw := range []string{"1.1.1.1", "8.8.8.8", "9.9.9.9", "2606:4700:4700::1111", "2001:4860:4860::8888"} {
		if db.country(netip.MustParseAddr(raw)) != "" {
			resolved++
		}
	}
	t.Logf("resolved %d/5 public test addresses", resolved)
}

func writeMMDB(t *testing.T, bits, version int, record []byte, present bool) string {
	t.Helper()
	contents := mmdbContents(bits, version, record, present)
	path := filepath.Join(t.TempDir(), fmt.Sprintf("country-%d-%d.mmdb", bits, version))
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func mmdbContents(bits, version int, record []byte, present bool) []byte {
	return mmdbContentsWithPadding(bits, version, record, present, 0)
}

func mmdbContentsWithPadding(bits, version int, record []byte, present bool, padding int) []byte {
	nodes := 1
	value := nodes
	if present {
		value = nodes + 16
	}
	data := record
	tree := mmdbNode(bits, value, value)
	metadataItems := []any{
		"node_count", mmdbUint(6, uint64(nodes)),
		"record_size", mmdbUint(5, uint64(bits)),
		"ip_version", mmdbUint(5, uint64(version)),
		"database_type", mmdbString("mi-guard synthetic country test"),
		"languages", mmdbArray(mmdbString("en")),
		"binary_format_major_version", mmdbUint(5, 2),
		"binary_format_minor_version", mmdbUint(5, 0),
		"build_epoch", mmdbUint(9, 1),
		"description", mmdbMap("en", mmdbString("synthetic")),
	}
	if padding > 0 {
		metadataItems = append(metadataItems, "padding", mmdbBytes(padding))
	}
	metadata := mmdbMap(metadataItems...)
	contents := append([]byte{}, tree...)
	contents = append(contents, make([]byte, 16)...)
	contents = append(contents, data...)
	contents = append(contents, []byte("\xab\xcd\xefMaxMind.com")...)
	contents = append(contents, metadata...)
	return contents
}

func mmdbNode(bits, left, right int) []byte {
	put := func(value, size int) []byte {
		out := make([]byte, size)
		for index := size - 1; index >= 0; index-- {
			out[index] = byte(value)
			value >>= 8
		}
		return out
	}
	switch bits {
	case 24:
		return append(put(left, 3), put(right, 3)...)
	case 28:
		out := append([]byte{}, put(left, 3)...)
		out = append(out, byte(left>>24)<<4|byte(right>>24)&15)
		return append(out, put(right, 3)...)
	case 32:
		return append(put(left, 4), put(right, 4)...)
	default:
		panic("unsupported synthetic record size")
	}
}

func mmdbString(value string) []byte {
	if len(value) > 284 {
		panic("synthetic string is too long")
	}
	if len(value) < 29 {
		return append([]byte{byte(2<<5 | len(value))}, value...)
	}
	return append([]byte{byte(2<<5 | 29), byte(len(value) - 29)}, value...)
}

func mmdbBytes(size int) []byte {
	return append(mmdbSizedControl(4, size), make([]byte, size)...)
}

func mmdbSizedControl(kind, size int) []byte {
	switch {
	case size < 29:
		return mmdbControl(kind, size)
	case size < 285:
		return append(mmdbControl(kind, 29), byte(size-29))
	case size < 65821:
		value := size - 285
		return append(mmdbControl(kind, 30), byte(value>>8), byte(value))
	case size <= 16843036:
		value := size - 65821
		return append(mmdbControl(kind, 31), byte(value>>16), byte(value>>8), byte(value))
	default:
		panic("synthetic payload is too large")
	}
}

func mmdbUint(kind int, value uint64) []byte {
	if value == 0 {
		return mmdbControl(kind, 0)
	}
	var raw [8]byte
	binary.BigEndian.PutUint64(raw[:], value)
	start := 0
	for start < len(raw)-1 && raw[start] == 0 {
		start++
	}
	payload := raw[start:]
	return append(mmdbControl(kind, len(payload)), payload...)
}

func mmdbControl(kind, size int) []byte {
	if kind <= 7 {
		return []byte{byte(kind<<5 | size)}
	}
	return []byte{byte(size), byte(kind - 7)}
}

func mmdbMap(items ...any) []byte {
	if len(items)%2 != 0 || len(items)/2 >= 29 {
		panic("invalid synthetic map")
	}
	out := []byte{byte(7<<5 | len(items)/2)}
	for index := 0; index < len(items); index += 2 {
		key, ok := items[index].(string)
		if !ok {
			panic("synthetic map key is not a string")
		}
		value, ok := items[index+1].([]byte)
		if !ok {
			panic("synthetic map value is not encoded")
		}
		out = append(out, mmdbString(key)...)
		out = append(out, value...)
	}
	return out
}

func mmdbArray(items ...[]byte) []byte {
	if len(items) >= 29 {
		panic("synthetic array is too long")
	}
	out := []byte{byte(len(items)), 4} // extended type 11 is encoded as 11 - 7
	for _, item := range items {
		out = append(out, item...)
	}
	return out
}

func mmdbPointer(offset int) []byte {
	return mmdbPointerSized(offset, 1)
}

func mmdbPointerSized(offset, size int) []byte {
	bases := [...]int{0, 0, 2048, 526336, 0}
	if size < 1 || size > 4 || offset < bases[size] {
		panic("synthetic pointer offset is out of range")
	}
	value := offset - bases[size]
	if size == 4 {
		value = offset
	}
	max := uint64(1) << (size * 8)
	if size < 4 {
		max <<= 3
	}
	if uint64(value) >= max {
		panic("synthetic pointer value does not fit")
	}
	control := byte(1<<5 | (size-1)<<3)
	if size < 4 {
		control |= byte(value >> (size * 8))
	}
	payload := make([]byte, size)
	for index := size - 1; index >= 0; index-- {
		payload[index] = byte(value)
		value >>= 8
	}
	return append([]byte{control}, payload...)
}

func TestDiscoverCountryReturnsNilForUnavailableDatabase(t *testing.T) {
	if db := discoverCountry(filepath.Join(t.TempDir(), "missing.mmdb"), ""); db != nil {
		db.close()
		t.Fatal("missing explicit database was discovered")
	}
	if _, err := openCountry(filepath.Join(t.TempDir(), "missing.mmdb")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing database error = %v", err)
	}
}
