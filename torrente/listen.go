package torrente

import (
	"fmt"
	"net"
)

// StoredBytes returns the number of verified bytes on disk.
func (t *Torrent) StoredBytes() int64 {
	if t.storage == nil {
		return 0
	}
	var sum int64
	for i := 0; i < t.storage.PieceCount(); i++ {
		if t.storage.PiecePresent(i) {
			sum += t.storage.PieceLength(i)
		}
	}
	return sum
}

// Listen starts a TCP listener on the engine port for incoming peers.
func (e *Engine) Listen() (net.Listener, error) {
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", e.port))
	if err != nil {
		return nil, fmt.Errorf("listen on :%d: %w", e.port, err)
	}
	e.listener = ln
	go e.acceptLoop(ln)
	e.Logf("listening on :%d for incoming peers", e.port)
	return ln, nil
}

// waitUpload delays an upload block to respect the global upload limit.
func (e *Engine) waitUpload(n int) {
	e.mu.Lock()
	lim := e.uploadLimiter
	e.mu.Unlock()
	if lim != nil {
		lim.wait(n)
	}
}

// ListenCloser returns the active peer listener (or nil).
func (e *Engine) ListenCloser() net.Listener {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.listener
}
