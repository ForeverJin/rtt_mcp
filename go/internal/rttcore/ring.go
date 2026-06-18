package rttcore

import "sync"

// ring is a bounded FIFO of stamped lines, mirroring Python's
// deque(maxlen=RTT_RING_BUFFER_SIZE). drain returns and clears the contents,
// keeping only the last maxBytes — matching rtt_read's single-consumer,
// read-once semantics.
type ring struct {
	mu  sync.Mutex
	max int
	buf []string
}

func newRing(max int) *ring {
	if max < 1 {
		max = 1
	}
	return &ring{max: max}
}

// append adds a line, dropping the oldest when at capacity (deque maxlen).
func (r *ring) append(s string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf = append(r.buf, s)
	if len(r.buf) > r.max {
		r.buf = r.buf[len(r.buf)-r.max:]
	}
}

// drain joins and clears the buffer, returning at most the last maxBytes.
func (r *ring) drain(maxBytes int) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.buf) == 0 {
		return ""
	}
	joined := join(r.buf)
	r.buf = r.buf[:0]
	if maxBytes > 0 && len(joined) > maxBytes {
		joined = joined[len(joined)-maxBytes:]
	}
	return joined
}

func (r *ring) clear() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf = r.buf[:0]
}

func (r *ring) len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.buf)
}

func join(ss []string) string {
	n := 0
	for _, s := range ss {
		n += len(s)
	}
	b := make([]byte, 0, n)
	for _, s := range ss {
		b = append(b, s...)
	}
	return string(b)
}
