// Package metainfo parses .torrent files, magnet URIs and computes
// info hashes per BEP-3 / BEP-9 / BEP-53.
package metainfo

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"

	"golang.org/x/net/html/charset"

	"github.com/alplix/digitalis/bencode"
)

// MetaInfo represents a parsed BitTorrent metainfo file (BEP-3).
type MetaInfo struct {
	Info         InfoDict
	Announce     string
	AnnounceList [][]string
	Comment      string
	CreatedBy    string
	CreationDate int64
	HTTPSeeds    []string // BEP-17
	URLList      []string // BEP-19 WebSeed
}

// InfoDict is the "info" dictionary.
type InfoDict struct {
	Name        string
	PieceLength int64
	Pieces      string // concatenated 20-byte SHA-1 hashes
	Length      int64  // for single-file
	Files       []File // for multi-file

	// Raw is the bencoded bytes of the info dict (needed for info hash).
	Raw []byte
}

// File is a single file within a torrent.
type File struct {
	Length int64
	Path   []string
}

// InfoHash returns the hex-encoded SHA-1 hash of the info dictionary.
func (m *MetaInfo) InfoHash() string {
	h := sha1.Sum(m.Info.Raw)
	return hex.EncodeToString(h[:])
}

// InfoHashBytes returns the raw 20-byte info hash.
func (m *MetaInfo) InfoHashBytes() []byte {
	h := sha1.Sum(m.Info.Raw)
	return h[:]
}

// IsSingleFile reports whether the torrent contains a single file.
func (m *MetaInfo) IsSingleFile() bool {
	return len(m.Info.Files) == 0
}

// TotalLength returns the total size of all files in bytes, or -1 if it could
// not be determined (magnet without metadata).
func (m *MetaInfo) TotalLength() int64 {
	if m.IsSingleFile() {
		return m.Info.Length
	}
	var total int64
	for _, f := range m.Info.Files {
		total += f.Length
	}
	return total
}

// PieceCount returns the number of pieces.
func (m *MetaInfo) PieceCount() int {
	return len(m.Info.Pieces) / 20
}

// PieceHash returns the 20-byte hash of piece index i.
func (m *MetaInfo) PieceHash(i int) []byte {
	start := i * 20
	return []byte(m.Info.Pieces[start : start+20])
}

// Parse parses a .torrent file from raw bytes.
func Parse(data []byte) (*MetaInfo, error) {
	raw, err := bencode.Decode(data)
	if err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	root, ok := raw.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("parse: root is not a dictionary")
	}

	m := &MetaInfo{}

	if v, ok := root["announce"]; ok {
		m.Announce = v.(string)
	}
	if v, ok := root["comment"]; ok {
		m.Comment = v.(string)
	}
	if v, ok := root["created by"]; ok {
		m.CreatedBy = v.(string)
	}
	if v, ok := root["creation date"]; ok {
		m.CreationDate = toInt64(v)
	}
	if v, ok := root["httpseeds"]; ok {
		if l, ok := v.([]interface{}); ok {
			for _, item := range l {
				m.HTTPSeeds = append(m.HTTPSeeds, item.(string))
			}
		}
	}
	if v, ok := root["url-list"]; ok {
		switch l := v.(type) {
		case []interface{}:
			for _, item := range l {
				m.URLList = append(m.URLList, item.(string))
			}
		case string:
			m.URLList = append(m.URLList, l)
		}
	}
	if v, ok := root["announce-list"]; ok {
		if l, ok := v.([]interface{}); ok {
			for _, tier := range l {
				if inner, ok := tier.([]interface{}); ok {
					t := make([]string, 0, len(inner))
					for _, tr := range inner {
						t = append(t, tr.(string))
					}
					m.AnnounceList = append(m.AnnounceList, t)
				}
			}
		}
	}

	infoRaw, ok := root["info"]
	if !ok {
		return nil, fmt.Errorf("parse: missing info dictionary")
	}
	infoDict, ok := infoRaw.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("parse: info is not a dictionary")
	}

	// Re-encode info dict for hash computation.
	infoBencoded, err := bencode.Encode(infoRaw)
	if err != nil {
		return nil, fmt.Errorf("parse: re-encode info: %w", err)
	}
	m.Info.Raw = infoBencoded

	if v, ok := infoDict["name"]; ok {
		m.Info.Name = decodeString(v.(string))
	}
	if v, ok := infoDict["piece length"]; ok {
		m.Info.PieceLength = toInt64(v)
	}
	if v, ok := infoDict["pieces"]; ok {
		m.Info.Pieces = v.(string)
	}
	if v, ok := infoDict["length"]; ok {
		m.Info.Length = toInt64(v)
	}
	if v, ok := infoDict["files"]; ok {
		if l, ok := v.([]interface{}); ok {
			for _, item := range l {
				fm, ok := item.(map[string]interface{})
				if !ok {
					continue
				}
				f := File{}
				if fv, ok := fm["length"]; ok {
					f.Length = toInt64(fv)
				}
				if pv, ok := fm["path"]; ok {
					if pl, ok := pv.([]interface{}); ok {
						for _, pe := range pl {
							f.Path = append(f.Path, decodeString(pe.(string)))
						}
					}
				}
				m.Info.Files = append(m.Info.Files, f)
			}
		}
	}

	return m, nil
}

func toInt64(v interface{}) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case int:
		return int64(n)
	}
	return 0
}

// decodeString decodes UTF-8 percent-encoded strings back to usable names.
func decodeString(s string) string {
	// If it's not valid UTF-8, fall back to latin1 detection via charset reader.
	if !isValidUTF8([]byte(s)) {
		// Try to convert from kept raw bytes
		if r, err := charset.NewReaderLabel("latin1", strings.NewReader(s)); err == nil {
			b, _ := io.ReadAll(r)
			return string(b)
		}
	}
	return s
}

func isValidUTF8(b []byte) bool {
	// minimal UTF-8 check
	u := string(b)
	_, size := decodeRune([]byte(u))
	return size == len(b) || size < 0
}

func decodeRune(b []byte) (rune, int) {
	// first byte
	if len(b) == 0 {
		return '\uFFFD', -1
	}
	c := b[0]
	switch {
	case c < 0x80:
		return rune(c), 1
	case c&0xE0 == 0xC0:
		return '\uFFFD', 2
	case c&0xF0 == 0xE0:
		return '\uFFFD', 3
	case c&0xF8 == 0xF0:
		return '\uFFFD', 4
	}
	return '\uFFFD', -1
}

// FromTorrentFile reads and parses a .torrent file from disk.
func FromTorrentFile(path string) (*MetaInfo, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(data)
}

// Magnet represents a parsed magnet URI (BEP-9 / BEP-53).
type Magnet struct {
	InfoHash   string   // hex
	DisplayName string  // dn
	Trackers   []string // tr
	Xs         string   // exact source / webseed
	Sources    []string
	UIDs       []string
	AcceptedLanguages []string
	PeerAddresses []string // x.pe
	ExactLength int64    // xl
}

// ParseMagnet parses a magnet: URI.
func ParseMagnet(uri string) (*Magnet, error) {
	if !strings.HasPrefix(uri, "magnet:?") {
		return nil, fmt.Errorf("not a magnet URI")
	}
	u, err := url.Parse(uri)
	if err != nil {
		return nil, fmt.Errorf("bad magnet: %w", err)
	}
	q := u.Query()
	m := &Magnet{}
	xt := q.Get("xt")
	if strings.HasPrefix(xt, "urn:btih:") {
		m.InfoHash = strings.TrimPrefix(xt, "urn:btih:")
	} else if strings.HasPrefix(xt, "urn:btmh:") {
		// v2 torrent, not handled yet
		m.InfoHash = strings.TrimPrefix(xt, "urn:btmh:")
	}
	m.DisplayName = q.Get("dn")
	m.Trackers = q["tr"]
	m.Xs = q.Get("xs")
	m.ExactLength = 0
	if v := q.Get("xl"); v != "" {
		fmt.Sscanf(v, "%d", &m.ExactLength)
	}
	m.Sources = q["as"]
	m.UIDs = q["x.u"]
	m.AcceptedLanguages = q["accept-language"]
	m.PeerAddresses = q["x.pe"]
	return m, nil
}
