package preference

import (
	"context"
	"sync"
	"time"
)

// MemCounter is an in-process Counter, for tests and single-replica deployments.
//
// With N replicas each holds its own counts, so the effective cap is N times the
// configured value. A deployment running more than one replica needs a shared
// counter.
type MemCounter struct {
	mu      sync.Mutex
	buckets map[string]*bucket

	// Now supplies the clock, for tests. Nil means time.Now.
	Now func() time.Time
}

// bucket holds one timestamp per counted occurrence, oldest first. Timestamps
// rather than a counter and a window start, because the window slides: the
// question is when each event happened, not how many have happened since a reset
// point.
type bucket struct {
	events []time.Time
}

// NewMemCounter returns an empty in-process counter.
func NewMemCounter() *MemCounter {
	return &MemCounter{buckets: make(map[string]*bucket)}
}

// IncrIfBelow implements Counter.
func (c *MemCounter) IncrIfBelow(_ context.Context, key string, window time.Duration, max int, now time.Time) (int, bool, time.Duration, error) {
	now = c.clock(now)

	c.mu.Lock()
	defer c.mu.Unlock()

	b := c.buckets[key]
	if b == nil {
		b = &bucket{}
		c.buckets[key] = b
	}
	b.events = prune(b.events, now, window)

	// The event is recorded regardless of the answer, so that the count reflects
	// true volume rather than plateauing at the limit.
	b.events = append(b.events, now)
	count := len(b.events)

	if max <= 0 {
		return count, false, c.retryAfter(b, now, window), nil
	}
	if count <= max {
		return count, true, 0, nil
	}
	return count, false, c.retryAfter(b, now, window), nil
}

// Peek implements Counter.
func (c *MemCounter) Peek(_ context.Context, key string, window time.Duration, now time.Time) (int, error) {
	now = c.clock(now)

	c.mu.Lock()
	defer c.mu.Unlock()

	b := c.buckets[key]
	if b == nil {
		return 0, nil
	}
	b.events = prune(b.events, now, window)
	if len(b.events) == 0 {
		delete(c.buckets, key)
		return 0, nil
	}
	return len(b.events), nil
}

// Reset clears every bucket, for tests.
func (c *MemCounter) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.buckets = make(map[string]*bucket)
}

// Sweep drops empty buckets and returns how many it removed.
//
// A cap key includes the user ID, so a long-running process accumulates one
// bucket per user per rule. A shared counter gets expiry from its store; this one
// has to be told.
func (c *MemCounter) Sweep(window time.Duration) int {
	now := c.clock(time.Time{})

	c.mu.Lock()
	defer c.mu.Unlock()

	dropped := 0
	for k, b := range c.buckets {
		b.events = prune(b.events, now, window)
		if len(b.events) == 0 {
			delete(c.buckets, k)
			dropped++
		}
	}
	return dropped
}

// retryAfter reports how long until the oldest event leaves the window, which is
// the earliest instant at which the count can drop below max.
func (c *MemCounter) retryAfter(b *bucket, now time.Time, window time.Duration) time.Duration {
	if len(b.events) == 0 {
		return 0
	}
	expiry := b.events[0].Add(window)
	if !expiry.After(now) {
		return 0
	}
	return expiry.Sub(now)
}

func (c *MemCounter) clock(fallback time.Time) time.Time {
	if c.Now != nil {
		return c.Now()
	}
	if !fallback.IsZero() {
		return fallback
	}
	return time.Now()
}

// prune drops events that have left the window, returning the surviving tail.
// It reuses the backing array, since it runs under a lock on a path every message
// takes.
func prune(events []time.Time, now time.Time, window time.Duration) []time.Time {
	cutoff := now.Add(-window)
	i := 0
	for i < len(events) && !events[i].After(cutoff) {
		i++
	}
	if i == 0 {
		return events
	}
	n := copy(events, events[i:])
	// Clear the tail so the dropped timestamps are not retained by the array.
	var zero time.Time
	for j := n; j < len(events); j++ {
		events[j] = zero
	}
	return events[:n]
}

var _ Counter = (*MemCounter)(nil)
