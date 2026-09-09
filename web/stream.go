package web

import (
	"fmt"
	"mime"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/alplix/digitalis/metainfo"
	"github.com/alplix/digitalis/torrente"
)

// fileSpan maps a torrent file index to its byte span within the torrent.
type fileSpan struct {
	Start  int64
	Length int64
	Name   string
}

// fileSpans computes the absolute byte offset of every torrent file.
func fileSpans(m *metainfo.MetaInfo) []fileSpan {
	if m.IsSingleFile() {
		return []fileSpan{{Start: 0, Length: m.Info.Length, Name: m.Info.Name}}
	}
	var off int64
	spans := make([]fileSpan, 0, len(m.Info.Files))
	for _, f := range m.Info.Files {
		spans = append(spans, fileSpan{Start: off, Length: f.Length, Name: filepath.Join(f.Path...)})
		off += f.Length
	}
	return spans
}

// mediaExts lists extensions considered streamable by the built-in player.
var mediaExts = map[string]bool{
	".mp4": true, ".m4v": true, ".mkv": true, ".webm": true, ".mov": true,
	".avi": true, ".ogv": true, ".ts": true, ".3gp": true, ".mpeg": true, ".mpg": true,
	".mp3": true, ".m4a": true, ".aac": true, ".ogg": true, ".wav": true,
	".flac": true, ".opus": true,
}

// parseRange parses a single "bytes=start-end" header against [absStart, absEnd].
func parseRange(h string, absStart, absEnd int64) (int64, int64, bool) {
	if !strings.HasPrefix(h, "bytes=") {
		return 0, 0, false
	}
	parts := strings.SplitN(strings.TrimPrefix(h, "bytes="), ",", 2)
	part := strings.TrimSpace(parts[0])
	sep := strings.Index(part, "-")
	if sep < 0 {
		return 0, 0, false
	}
	lo := strings.TrimSpace(part[:sep])
	hi := strings.TrimSpace(part[sep+1:])
	size := absEnd - absStart + 1
	var s, e int64

	if lo == "" {
		n, err := strconv.ParseInt(hi, 10, 64)
		if err != nil || n <= 0 {
			return 0, 0, false
		}
		if n > size {
			n = size
		}
		s = absEnd - n + 1
		e = absEnd
		return s, e, true
	}

	start, err := strconv.ParseInt(lo, 10, 64)
	if err != nil {
		return 0, 0, false
	}
	s = absStart + start
	if hi == "" {
		e = absEnd
	} else {
		end, err := strconv.ParseInt(hi, 10, 64)
		if err != nil {
			return 0, 0, false
		}
		e = absStart + end
	}
	if s > e || s > absEnd {
		return 0, 0, false
	}
	if e > absEnd {
		e = absEnd
	}
	return s, e, true
}

// streamFile serves a torrent file over HTTP with byte-range support. Pieces
// not yet verified are served as zero bytes (they are downloaded with priority),
// which lets media players start playback while the download is still running.
func (s *Server) streamFile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	idx, err := strconv.Atoi(r.PathValue("idx"))
	if err != nil || idx < 0 {
		writeError(w, http.StatusBadRequest, "invalid file index")
		return
	}
	t, ok := s.engine.GetTorrent(id)
	if !ok {
		writeError(w, http.StatusNotFound, "torrent not found")
		return
	}
	if t.MetaInfo == nil || !t.StorageAvailable() {
		writeError(w, http.StatusServiceUnavailable, "metadata not resolved yet")
		return
	}
	spans := fileSpans(t.MetaInfo)
	if idx >= len(spans) {
		writeError(w, http.StatusNotFound, "file not found")
		return
	}
	span := spans[idx]
	if span.Length <= 0 {
		writeError(w, http.StatusNotFound, "file is empty")
		return
	}

	pieceLen := t.MetaInfo.Info.PieceLength
	pieceCount := t.StoragePieceCount()

	setPriority := func(from, to int64) {
		if pieceLen <= 0 || pieceCount <= 0 {
			return
		}
		lo := from / pieceLen
		hi := to / pieceLen
		if hi >= int64(pieceCount) {
			hi = int64(pieceCount) - 1
		}
		if lo <= hi {
			_ = s.engine.SetPriorityPieces(id, int(lo), int(hi))
		}
	}

	ctype := mime.TypeByExtension(strings.ToLower(filepath.Ext(span.Name)))
	if ctype == "" {
		ctype = "application/octet-stream"
	}

	absStart := span.Start
	absEnd := span.Start + span.Length - 1

	if rangeHdr := r.Header.Get("Range"); rangeHdr != "" {
		start, end, ok := parseRange(rangeHdr, absStart, absEnd)
		if !ok {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", span.Length))
			writeError(w, http.StatusRequestedRangeNotSatisfiable, "range not satisfiable")
			return
		}
		w.Header().Set("Content-Type", ctype)
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, span.Length))
		w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
		w.WriteHeader(http.StatusPartialContent)
		setPriority(start, end)
		s.writeFileRange(w, t, start, end-start+1, pieceLen)
		return
	}

	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Content-Length", strconv.FormatInt(span.Length, 10))
	w.WriteHeader(http.StatusOK)
	setPriority(absStart, absEnd)
	s.writeFileRange(w, t, absStart, span.Length, pieceLen)
}

// writeFileRange streams bytes [off, off+length) split across torrent pieces.
func (s *Server) writeFileRange(w http.ResponseWriter, t *torrente.Torrent, off, length, pieceLen int64) {
	buf := make([]byte, 256*1024)
	for length > 0 {
		n := int64(len(buf))
		if length < n {
			n = length
		}
		piece := off / pieceLen
		begin := off - piece*pieceLen
		data, err := t.ReadPieceRange(int(piece), begin, int(n))
		if err != nil {
			return // client went away or storage read failed
		}
		if _, werr := w.Write(data); werr != nil {
			return
		}
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
		off += n
		length -= n
	}
}
