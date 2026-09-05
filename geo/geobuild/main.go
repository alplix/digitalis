// Command geobuild generates geo/countries.bin from a DB-IP style CSV with
// lines of the form: <start_ip>,<end_ip>,<country_code>. Output layout matches
// what geo.Lookup expects:
//
//	[0:4]  "DLGI"
//	[4:8]  uint32 LE row count N
//	[8:]   N uint32 LE range starts
//	[N*4:] N uint32 LE range ends
//	[...]  N uint8 code indices
//	[...]  null-terminated country code table
package main

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"sort"
	"strings"
)

type row struct {
	start, end uint32
	cc         string
}

func ipToU32(s string) (uint32, bool) {
	ip := net.ParseIP(s)
	if ip == nil {
		return 0, false
	}
	v4 := ip.To4()
	if v4 == nil {
		return 0, false
	}
	return binary.BigEndian.Uint32(v4), true
}

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: geobuild <input.csv> <output.bin>")
		os.Exit(1)
	}
	inPath, outPath := os.Args[1], os.Args[2]

	f, err := os.Open(inPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open:", err)
		os.Exit(1)
	}
	defer f.Close()

	var rows []row
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		parts := strings.Split(line, ",")
		if len(parts) < 3 {
			continue
		}
		a := strings.TrimSpace(parts[0])
		b := strings.TrimSpace(parts[1])
		cc := strings.TrimSpace(parts[2])
		if len(cc) != 2 {
			continue
		}
		s0, ok1 := ipToU32(a)
		e0, ok2 := ipToU32(b)
		if !ok1 || !ok2 {
			continue
		}
		rows = append(rows, row{start: s0, end: e0, cc: cc})
	}
	if err := sc.Err(); err != nil {
		fmt.Fprintln(os.Stderr, "scan:", err)
		os.Exit(1)
	}

	sort.Slice(rows, func(i, j int) bool { return rows[i].start < rows[j].start })

	merged := rows[:0]
	for _, r := range rows {
		n := len(merged)
		if n > 0 && r.cc == merged[n-1].cc && r.start <= merged[n-1].end+1 {
			if r.end > merged[n-1].end {
				merged[n-1].end = r.end
			}
			continue
		}
		merged = append(merged, r)
	}
	rows = merged

	var table []string
	idxFor := func(cc string) byte {
		for i, c := range table {
			if c == cc {
				return byte(i)
			}
		}
		table = append(table, cc)
		return byte(len(table) - 1)
	}
	if len(table) > 256 {
		fmt.Fprintln(os.Stderr, "too many distinct country codes:", len(table))
		os.Exit(1)
	}

	out, err := os.Create(outPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "create:", err)
		os.Exit(1)
	}
	defer out.Close()

	w := bufio.NewWriter(out)
	var hdr [8]byte
	copy(hdr[:4], "DLGI")
	binary.LittleEndian.PutUint32(hdr[4:], uint32(len(rows)))
	w.Write(hdr[:])
	for _, r := range rows {
		var b [4]byte
		binary.LittleEndian.PutUint32(b[:], r.start)
		w.Write(b[:])
	}
	for _, r := range rows {
		var b [4]byte
		binary.LittleEndian.PutUint32(b[:], r.end)
		w.Write(b[:])
	}
	buf := make([]byte, len(rows))
	for i, r := range rows {
		buf[i] = idxFor(r.cc)
	}
	w.Write(buf)
	for _, c := range table {
		w.WriteString(c)
		w.WriteByte(0)
	}
	if err := w.Flush(); err != nil {
		fmt.Fprintln(os.Stderr, "flush:", err)
		os.Exit(1)
	}
	fmt.Printf("wrote %s: %d rows, %d country codes, %d bytes\n", outPath, len(rows), len(table), outSize(outPath))
}

func outSize(p string) int64 {
	fi, err := os.Stat(p)
	if err != nil {
		return 0
	}
	return fi.Size()
}