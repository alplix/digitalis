// Package geo maps IPv4 addresses to ISO 3166-1 alpha-2 country codes using a
// compact, embedded range table generated from the DB-IP country database
// (CC BY 4.0, https://db-ip.com).
package geo

import (
	"embed"
	"encoding/binary"
	"net"
	"sort"
	"sync"
)

//go:embed countries.bin
var dbFS embed.FS

const magic = "DLGI"

var (
	once   sync.Once
	starts []uint32
	ends   []uint32
	codes  []string
)

func load() {
	once.Do(func() {
		data, err := dbFS.ReadFile("countries.bin")
		if err != nil || len(data) < 8 || string(data[:4]) != magic {
			return
		}
		n := int(binary.LittleEndian.Uint32(data[4:8]))
		if n <= 0 || n > 4_000_000 {
			return
		}
		starts = make([]uint32, n)
		ends = make([]uint32, n)
		codes = make([]string, n)
		off := 8
		if len(data) < off+n*9 {
			return
		}
		for i := 0; i < n; i++ {
			starts[i] = binary.LittleEndian.Uint32(data[off+i*4:])
		}
		off += n * 4
		for i := 0; i < n; i++ {
			ends[i] = binary.LittleEndian.Uint32(data[off+i*4:])
		}
		off += n * 4
		idx := data[off : off+n]
		off += n
		var table []string
		for off < len(data) {
			j := off
			for j < len(data) && data[j] != 0 {
				j++
			}
			if j >= len(data) {
				break
			}
			table = append(table, string(data[off:j]))
			off = j + 1
		}
		for i := range codes {
			if int(idx[i]) < len(table) {
				codes[i] = table[idx[i]]
			}
		}
	})
}

// Lookup returns the ISO 3166-1 alpha-2 country code for an IP address given
// as either a bare IP or a host:port string. Empty string if unknown.
func Lookup(hostport string) string {
	load()
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return ""
	}
	v4 := ip.To4()
	if v4 == nil {
		return ""
	}
	x := binary.BigEndian.Uint32(v4)
	if len(starts) == 0 {
		return ""
	}
	i := sort.Search(len(starts), func(i int) bool { return starts[i] > x })
	if i == 0 {
		return ""
	}
	i--
	if x <= ends[i] {
		return codes[i]
	}
	return ""
}

// LookupIP is Lookup for a parsed net.IP.
func LookupIP(ip net.IP) string {
	if ip == nil {
		return ""
	}
	v4 := ip.To4()
	if v4 == nil {
		return ""
	}
	return Lookup(net.IP(v4).String())
}