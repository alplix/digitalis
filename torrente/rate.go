package torrente

import (
	"sync"
	"time"
)

// rateLimiter is a simple token-bucket rate limiter. bytesPerSec <= 0
// disables limiting.
type rateLimiter struct {
	mu          sync.Mutex
	bytesPerSec float64
	tokens      float64
	last        time.Time
	burst       float64
}

func newRateLimiter(bytesPerSec int64) *rateLimiter {
	return &rateLimiter{
		bytesPerSec: float64(bytesPerSec),
		tokens:      0,
		last:        time.Now(),
		burst:       0,
	}
}

func (r *rateLimiter) setRate(bytesPerSec int64) {
	r.mu.Lock()
	r.bytesPerSec = float64(bytesPerSec)
	r.mu.Unlock()
}

// wait blocks until `n` bytes of budget are available. If the limit is
// disabled it returns immediately.
func (r *rateLimiter) wait(n int) {
	r.mu.Lock()
	r.refill()
	if r.bytesPerSec <= 0 {
		r.mu.Unlock()
		return
	}
	for r.tokens < float64(n) {
		need := float64(n) - r.tokens
		if r.bytesPerSec <= 0 {
			break
		}
		sleepFor := time.Duration(need / r.bytesPerSec * float64(time.Second))
		r.mu.Unlock()
		time.Sleep(sleepFor)
		r.mu.Lock()
		r.refill()
	}
	r.tokens -= float64(n)
	r.mu.Unlock()
}

func (r *rateLimiter) refill() {
	now := time.Now()
	elapsed := now.Sub(r.last).Seconds()
	r.last = now
	if r.bytesPerSec > 0 {
		r.tokens += elapsed * r.bytesPerSec
		max := r.bytesPerSec
		if r.burst > 0 {
			max = r.burst
		}
		if r.tokens > max {
			r.tokens = max
		}
	}
}
