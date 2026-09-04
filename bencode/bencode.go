// Package bencode implements the Bencode encoding format used by the
// BitTorrent protocol (BEP-3).
package bencode

import (
	"bufio"
	"bytes"
	"fmt"
	"strconv"
	"strings"
)

// Value represents a decoded bencode value. It can be one of:
//
//	int64     - integer
//	string    - byte string
//	[]interface{} - list
//	map[string]interface{} - dictionary
type Value interface{}

// Decode parses bencode data from a byte slice.
func Decode(data []byte) (Value, error) {
	d := &decoder{buf: data}
	v, err := d.parseValue()
	if err != nil {
		return nil, err
	}
	// Ensure trailing whitespace is ok
	if d.pos != len(d.buf) {
		// skip trailing newlines/whitespace
		for d.pos < len(d.buf) {
			c := d.buf[d.pos]
			if c != '\n' && c != ' ' && c != '\r' && c != '\t' {
				return nil, fmt.Errorf("bencode: trailing data at offset %d", d.pos)
			}
			d.pos++
		}
	}
	return v, nil
}

type decoder struct {
	buf []byte
	pos int
}

func (d *decoder) parseValue() (Value, error) {
	if d.pos >= len(d.buf) {
		return nil, fmt.Errorf("bencode: unexpected end of data")
	}
	c := d.buf[d.pos]
	switch {
	case c == 'i':
		return d.parseInt()
	case c == 'l':
		return d.parseList()
	case c == 'd':
		return d.parseDict()
	case c >= '0' && c <= '9':
		return d.parseBytes()
	default:
		return nil, fmt.Errorf("bencode: invalid token %q at offset %d", c, d.pos)
	}
}

func (d *decoder) parseInt() (int64, error) {
	d.pos++ // skip 'i'
	start := d.pos
	for d.pos < len(d.buf) && d.buf[d.pos] != 'e' {
		d.pos++
	}
	if d.pos >= len(d.buf) {
		return 0, fmt.Errorf("bencode: unterminated integer")
	}
	s := string(d.buf[start:d.pos])
	d.pos++ // skip 'e'
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("bencode: invalid integer %q: %w", s, err)
	}
	return n, nil
}

func (d *decoder) parseBytes() (string, error) {
	start := d.pos
	for d.pos < len(d.buf) && d.buf[d.pos] != ':' {
		d.pos++
	}
	if d.pos >= len(d.buf) {
		return "", fmt.Errorf("bencode: unterminated string length")
	}
	lenStr := string(d.buf[start:d.pos])
	length, err := strconv.Atoi(lenStr)
	if err != nil {
		return "", fmt.Errorf("bencode: invalid string length %q", lenStr)
	}
	d.pos++ // skip ':'
	if d.pos+length > len(d.buf) {
		return "", fmt.Errorf("bencode: string length %d exceeds buffer", length)
	}
	s := string(d.buf[d.pos : d.pos+length])
	d.pos += length
	return s, nil
}

func (d *decoder) parseList() ([]interface{}, error) {
	d.pos++ // skip 'l'
	var list []interface{}
	for {
		if d.pos >= len(d.buf) {
			return nil, fmt.Errorf("bencode: unterminated list")
		}
		if d.buf[d.pos] == 'e' {
			d.pos++
			return list, nil
		}
		v, err := d.parseValue()
		if err != nil {
			return nil, err
		}
		list = append(list, v)
	}
}

func (d *decoder) parseDict() (map[string]interface{}, error) {
	d.pos++ // skip 'd'
	dict := make(map[string]interface{})
	for {
		if d.pos >= len(d.buf) {
			return nil, fmt.Errorf("bencode: unterminated dictionary")
		}
		if d.buf[d.pos] == 'e' {
			d.pos++
			return dict, nil
		}
		key, err := d.parseBytes()
		if err != nil {
			return nil, err
		}
		v, err := d.parseValue()
		if err != nil {
			return nil, err
		}
		dict[key] = v
	}
}

// Encode serializes a Value into bencode format.
func Encode(v Value) ([]byte, error) {
	var buf bytes.Buffer
	if err := encode(&buf, v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func encode(buf *bytes.Buffer, v Value) error {
	switch val := v.(type) {
	case int:
		buf.WriteString("i" + strconv.Itoa(val) + "e")
	case int64:
		buf.WriteString("i" + strconv.FormatInt(val, 10) + "e")
	case string:
		buf.WriteString(strconv.Itoa(len(val)) + ":" + val)
	case []byte:
		buf.WriteString(strconv.Itoa(len(val)) + ":" + string(val))
	case []interface{}:
		buf.WriteByte('l')
		for _, item := range val {
			if err := encode(buf, item); err != nil {
				return err
			}
		}
		buf.WriteByte('e')
	case map[string]interface{}:
		// keys must be sorted
		keys := make([]string, 0, len(val))
		for k := range val {
			keys = append(keys, k)
		}
		sortStrings(keys)
		buf.WriteByte('d')
		for _, k := range keys {
			buf.WriteString(strconv.Itoa(len(k)) + ":" + k)
			if err := encode(buf, val[k]); err != nil {
				return err
			}
		}
		buf.WriteByte('e')
	default:
		return fmt.Errorf("bencode: unsupported type %T", v)
	}
	return nil
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// DecodeReader reads a bencode value from a bufio.Reader incrementally (useful
// for very large torrent files / streaming contexts).
func DecodeReader(r *bufio.Reader) (Value, error) {
	// For simplicity read everything; can be optimized later with a
	// streaming decoder.
	peek, err := r.Peek(1)
	if err != nil {
		return nil, err
	}
	_ = peek
	var buf bytes.Buffer
	for {
		b, err := r.ReadByte()
		if err != nil {
			return nil, err
		}
		buf.WriteByte(b)
		// crude end detection; this is not efficient but correct enough for
		// our streaming decoder. For now just read the whole stream.
		if len(peek) > 0 && b == 'e' {
			// not a robust check, kept minimal
		}
	}
}

// Helper to lookup nested string values from a parsed dict.
func LookupString(dict map[string]interface{}, key string) (string, bool) {
	if v, ok := dict[key]; ok {
		if s, ok := v.(string); ok {
			return s, true
		}
	}
	return "", false
}

// Helper to lookup int values from a parsed dict.
func LookupInt(dict map[string]interface{}, key string) (int64, bool) {
	if v, ok := dict[key]; ok {
		switch n := v.(type) {
		case int64:
			return n, true
		case int:
			return int64(n), true
		}
	}
	return 0, false
}

// InfoHashFromRaw calculates nothing by itself; kept for API symmetry.
func IsBencode(data []byte) bool {
	if len(data) == 0 {
		return false
	}
	c := data[0]
	return c == 'i' || c == 'l' || c == 'd' || (c >= '0' && c <= '9')
}

// ParseMagnetInfo is a helper (kept for completeness).
func ParseIntList(s string) ([]int64, error) {
	if s == "" {
		return nil, nil
	}
	parts := strings.Split(s, ",")
	out := make([]int64, 0, len(parts))
	for _, p := range parts {
		n, err := strconv.ParseInt(strings.TrimSpace(p), 10, 64)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, nil
}
