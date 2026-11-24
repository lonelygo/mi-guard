package flight

import (
	"bytes"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unicode/utf8"
)

// CountryDB reads only the country code from local MaxMind DB v2 records. It
// supports the country map, sing string and Meta-geoip0 list used by Mihomo.
// File-backed page reads keep a large optional city database off the Go heap.
type CountryDB struct {
	file                                          *os.File
	size, nodes, width, version, treeEnd, dataEnd int
	mu                                            sync.Mutex
	pages                                         map[int][]byte
}

var errCountryDB = errors.New("invalid or unsupported country database")

func openCountry(path string) (*CountryDB, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	d := &CountryDB{file: f, pages: make(map[int][]byte)}
	if err = d.header(); err != nil {
		f.Close()
		return nil, err
	}
	return d, nil
}

func (d *CountryDB) header() error {
	stat, err := d.file.Stat()
	if err != nil {
		return err
	}
	if !stat.Mode().IsRegular() || stat.Size() < 32 || stat.Size() > 1<<32 {
		return errCountryDB
	}
	d.size = int(stat.Size())
	tail := make([]byte, min(d.size, 128<<10))
	if _, err := d.file.ReadAt(tail, stat.Size()-int64(len(tail))); err != nil {
		return err
	}
	marker := []byte("\xab\xcd\xefMaxMind.com")
	p := bytes.LastIndex(tail, marker)
	markerOffset := d.size - len(tail) + p
	if p < 0 || markerOffset < 16 {
		return errCountryDB
	}
	d.dataEnd = markerOffset
	start := d.dataEnd + len(marker)
	m, ok := d.decode(start, start, d.size).(map[string]any)
	if !ok || m["binary_format_major_version"] != 2 {
		return errCountryDB
	}
	d.nodes, _ = m["node_count"].(int)
	bits, _ := m["record_size"].(int)
	d.version, _ = m["ip_version"].(int)
	if d.nodes < 1 || (bits != 24 && bits != 28 && bits != 32) || (d.version != 4 && d.version != 6) {
		return errCountryDB
	}
	d.width = bits / 4
	d.treeEnd = d.width * d.nodes
	if d.treeEnd+16 > d.dataEnd {
		return errCountryDB
	}
	return nil
}

func (d *CountryDB) close() {
	if d != nil {
		d.file.Close()
	}
}

func discoverCountry(explicit, core string) *CountryDB {
	var paths []string
	if explicit != "" {
		paths = []string{explicit}
	} else {
		for _, name := range []string{"country.mmdb", "Country.mmdb", "geoip.metadb", "GeoLite2-Country.mmdb"} {
			paths = append(paths, filepath.Join(filepath.Dir(core), name))
		}
	}
	for _, p := range paths {
		if db, err := openCountry(p); err == nil {
			return db
		}
	}
	return nil
}

func (d *CountryDB) at(i int) (byte, bool) {
	if i < 0 || i >= d.size {
		return 0, false
	}
	const block = 4096
	page := i / block
	d.mu.Lock()
	defer d.mu.Unlock()
	b := d.pages[page]
	if b == nil {
		b = make([]byte, min(block, d.size-page*block))
		if _, err := d.file.ReadAt(b, int64(page*block)); err != nil {
			return 0, false
		}
		if len(d.pages) >= 512 {
			clear(d.pages)
		}
		d.pages[page] = b
	}
	return b[i%block], true
}

func (d *CountryDB) child(node, side int) (int, bool) {
	if node < 0 || node >= d.nodes || side < 0 || side > 1 {
		return 0, false
	}
	p := node * d.width
	if d.width == 7 {
		lo, lowOK := d.number(p+side*4, 3)
		mid, midOK := d.at(p + 3)
		if !lowOK || !midOK {
			return 0, false
		}
		high := int(mid >> 4)
		if side == 1 {
			high = int(mid & 15)
		}
		return high<<24 | lo, true
	}
	w := d.width / 2
	return d.number(p+side*w, w)
}

var benchmarkPrefix = netip.MustParsePrefix("198.18.0.0/15")

func (d *CountryDB) number(p, n int) (int, bool) {
	v := 0
	for i := 0; i < n; i++ {
		b, ok := d.at(p + i)
		if !ok {
			return 0, false
		}
		v = v<<8 | int(b)
	}
	return v, true
}

func (d *CountryDB) country(addr netip.Addr) string {
	if d == nil || !addr.IsValid() {
		return ""
	}
	addr = addr.Unmap()
	if !addr.IsGlobalUnicast() || addr.IsPrivate() {
		return ""
	}
	if addr.Is4() && benchmarkPrefix.Contains(addr) {
		return ""
	}
	var ip []byte
	if addr.Is4() {
		v := addr.As4()
		if d.version == 6 {
			ip = make([]byte, 16)
			copy(ip[12:], v[:])
		} else {
			ip = v[:]
		}
	} else if d.version == 6 {
		ip = addr.AsSlice()
	} else {
		return ""
	}
	node := 0
	for n := 0; n < len(ip)*8 && node < d.nodes; n++ {
		side := int(ip[n/8] >> (7 - n%8) & 1)
		var ok bool
		node, ok = d.child(node, side)
		if !ok {
			return ""
		}
	}
	if node < d.nodes+16 {
		return ""
	}
	base := d.treeEnd + 16
	pos := d.treeEnd + (node - d.nodes)
	v := d.decode(pos, base, d.dataEnd)
	switch r := v.(type) {
	case string:
		return region(r)
	case []any:
		return uniqueRegion(r)
	case map[string]any:
		if s, ok := r["country"].(string); ok {
			return region(s)
		}
		if c, ok := r["country"].(map[string]any); ok {
			s, _ := c["iso_code"].(string)
			return region(s)
		}
	}
	return ""
}

func region(s string) string {
	s = strings.ToUpper(s)
	if len(s) != 2 || s[0] < 'A' || s[0] > 'Z' || s[1] < 'A' || s[1] > 'Z' || s == "ZZ" {
		return ""
	}
	return s
}
func uniqueRegion(list []any) string {
	var code string
	for _, e := range list {
		v, _ := e.(string)
		if c := region(v); c != "" {
			if code != "" && c != code {
				return ""
			}
			code = c
		}
	}
	return code
}

// A record has bounded traversal and allocations, including across pointers.
type recordReader struct {
	db                *CountryDB
	base, end, budget int
}

func (d *CountryDB) decode(pos, base, end int) any {
	r := recordReader{d, base, end, 4096}
	v, _, ok := r.value(pos, 0)
	if !ok {
		return nil
	}
	return v
}
func (r *recordReader) value(pos, depth int) (any, int, bool) {
	if pos < r.base || pos >= r.end || depth > 24 || r.budget <= 0 {
		return nil, 0, false
	}
	r.budget--
	d := r.db
	ctrl, ok := d.at(pos)
	if !ok {
		return nil, 0, false
	}
	pos++
	kind, size := int(ctrl>>5), int(ctrl&31)
	if kind == 1 {
		n := (size >> 3) + 1
		if pos+n > r.end {
			return nil, 0, false
		}
		v, ok := d.number(pos, n)
		if !ok {
			return nil, 0, false
		}
		if n < 4 {
			v |= (size & 7) << (n * 8)
		}
		v += []int{0, 0, 2048, 526336, 0}[n]
		data, _, ok := r.value(r.base+v, depth+1)
		return data, pos + n, ok
	}
	if kind == 0 {
		k, ok := d.at(pos)
		if !ok || pos >= r.end {
			return nil, 0, false
		}
		kind = int(k) + 7
		pos++
	}
	if size >= 29 {
		n := size - 28
		if pos+n > r.end {
			return nil, 0, false
		}
		v, ok := d.number(pos, n)
		if !ok {
			return nil, 0, false
		}
		size = []int{0, 29, 285, 65821}[n] + v
		pos += n
	}
	if kind == 7 || kind == 11 {
		if size > 256 {
			return nil, 0, false
		}
		m := make(map[string]any)
		var a []any
		for i := 0; i < size; i++ {
			var key string
			if kind == 7 {
				k, next, ok := r.value(pos, depth+1)
				if !ok {
					return nil, 0, false
				}
				key, ok = k.(string)
				if !ok {
					return nil, 0, false
				}
				if _, duplicate := m[key]; duplicate {
					return nil, 0, false
				}
				pos = next
			}
			v, next, ok := r.value(pos, depth+1)
			if !ok {
				return nil, 0, false
			}
			pos = next
			if kind == 7 {
				m[key] = v
			} else {
				a = append(a, v)
			}
		}
		if kind == 7 {
			return m, pos, true
		}
		return a, pos, true
	}
	if kind == 14 {
		return size != 0, pos, size <= 1
	}
	if size > r.end-pos {
		return nil, 0, false
	}
	end := pos + size
	switch kind {
	case 2:
		if size > 512 || size > r.budget {
			return nil, 0, false
		}
		r.budget -= size
		b := make([]byte, size)
		for i := pos; i < end; i++ {
			c, ok := d.at(i)
			if !ok {
				return nil, 0, false
			}
			b[i-pos] = c
		}
		if !utf8.Valid(b) {
			return nil, 0, false
		}
		return string(b), end, true
	case 5, 6, 9:
		limit := 8
		if kind == 5 {
			limit = 2
		} else if kind == 6 {
			limit = 4
		}
		if size > limit {
			return nil, 0, false
		}
		n, ok := d.number(pos, size)
		return n, end, ok && n >= 0
	default:
		return nil, end, true
	}
}
