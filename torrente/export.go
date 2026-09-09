package torrente

import (
	"bytes"
	"fmt"
)

// ExportTorrent builds a .torrent file for the torrent. It works for magnets
// too once their metadata has arrived: the raw bencoded info dict is copied
// verbatim, which keeps the info hash identical. When no announce URL is
// known the file contains only the info dict — clients fall back to DHT.
func (e *Engine) ExportTorrent(id string) ([]byte, error) {
	e.mu.Lock()
	t, ok := e.torrents[id]
	e.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("torrent not found")
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.MetaInfo == nil || len(t.MetaInfo.Info.Raw) == 0 {
		return nil, fmt.Errorf("metadata not ready")
	}

	var buf bytes.Buffer
	buf.WriteString("d")
	if a := t.MetaInfo.Announce; a != "" {
		writeBString(&buf, "announce")
		writeBString(&buf, a)
	}
	if len(t.MetaInfo.AnnounceList) > 0 {
		writeBString(&buf, "announce-list")
		buf.WriteString("l")
		for _, tier := range t.MetaInfo.AnnounceList {
			buf.WriteString("l")
			for _, u := range tier {
				writeBString(&buf, u)
			}
			buf.WriteString("e")
		}
		buf.WriteString("e")
	}
	writeBString(&buf, "info")
	buf.Write(t.MetaInfo.Info.Raw)
	buf.WriteString("e")
	return buf.Bytes(), nil
}

// writeBString appends a bencoded string: "<len>:<bytes>".
func writeBString(buf *bytes.Buffer, s string) {
	fmt.Fprintf(buf, "%d:", len(s))
	buf.WriteString(s)
}
