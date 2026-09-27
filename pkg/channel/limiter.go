package channel

import (
	"context"
	"sync"
	"time"

	"github.com/jiuyue1123/message-center/pkg/message"
)

// Limiter gates how fast messages may be sent on a channel.
//
// A limiter denial is not a failure: the delivery returns to pending without
// incrementing AttemptCount, following the same path as mcerr.KindAborted.
// Recording a denial as a failed attempt would let a traffic burst exhaust every
// in-flight delivery's retry budget without a single provider call being made.
//
// An in-process limiter limits one process. With N replicas the effective rate is
// N times the configured value, so a deployment requiring a real provider quota
// must back this with shared state.
type Limiter interface {
	// Allow reports whether one message may be sent on ch right now. It must be
	// cheap and non-blocking; a limiter that blocks converts the worker pool into
	// a set of sleeping goroutines.
	//
	// When the answer is no, retryAfter is how long to wait before asking again.
	// A zero retryAfter means the caller applies a short fixed delay rather than
	// retrying immediately.
	Allow(ctx context.Context, ch message.Channel) (allowed bool, retryAfter time.Duration)

	// Reserve reports how many of n messages may be sent on ch, so that a worker
	// claiming a batch does not claim more than the channel can absorb.
	Reserve(ctx context.Context, ch message.Channel, n int) (allowed int, retryAfter time.Duration)
}

// LimiterFunc adapts a function to the Limiter interface.
type LimiterFunc func(ctx context.Context, ch message.Channel) (bool, time.Duration)

// Allow implements Limiter.
func (f LimiterFunc) Allow(ctx context.Context, ch message.Channel) (bool, time.Duration) {
	return f(ctx, ch)
}

// Reserve implements Limiter by asking Allow n times. A token-bucket
// implementation should override it, since this form consumes the tokens it is
// only speculating about; it errs toward under-granting.
func (f LimiterFunc) Reserve(ctx context.Context, ch message.Channel, n int) (int, time.Duration) {
	for i := 0; i < n; i++ {
		ok, retryAfter := f(ctx, ch)
		if !ok {
			return i, retryAfter
		}
	}
	return n, 0
}

// Unlimited is a Limiter that permits everything. It is the default, so that a
// deployment that has not configured rate limiting is not silently throttled.
var Unlimited Limiter = unlimited{}

type unlimited struct{}

// Allow implements Limiter.
func (unlimited) Allow(context.Context, message.Channel) (bool, time.Duration) { return true, 0 }

// Reserve implements Limiter.
func (unlimited) Reserve(_ context.Context, _ message.Channel, n int) (int, time.Duration) {
	return n, 0
}

// ChannelLimiter routes to a per-channel Limiter, falling back to a default.
// Rate limits are per-provider, so an SMS gateway's quota must not throttle
// email.
type ChannelLimiter struct {
	// Default applies to channels with no specific limiter. Nil means Unlimited.
	Default Limiter

	// PerChannel overrides the default for named channels.
	PerChannel map[message.Channel]Limiter
}

// For returns the limiter for a channel. It never returns nil; an unconfigured
// channel yields Unlimited, so a misconfiguration sends at full rate rather than
// panicking in a worker.
func (c ChannelLimiter) For(ch message.Channel) Limiter {
	if l, ok := c.PerChannel[ch]; ok && l != nil {
		return l
	}
	if c.Default != nil {
		return c.Default
	}
	return Unlimited
}

// Allow implements Limiter.
func (c ChannelLimiter) Allow(ctx context.Context, ch message.Channel) (bool, time.Duration) {
	return c.For(ch).Allow(ctx, ch)
}

// Reserve implements Limiter.
func (c ChannelLimiter) Reserve(ctx context.Context, ch message.Channel, n int) (int, time.Duration) {
	return c.For(ch).Reserve(ctx, ch, n)
}

// TokenBucket is a per-channel token bucket, provided as a reference
// implementation of Limiter.
//
// It is correct for a single process and wrong for a fleet: with N replicas the
// aggregate rate is N times Rate.
type TokenBucket struct {
	// Rate is tokens replenished per second.
	Rate float64

	// Burst is the bucket's capacity, and the most that may be granted at once.
	Burst int

	// Now supplies the clock, for tests. Nil means time.Now.
	Now func() time.Time

	mu      sync.Mutex
	tokens  float64
	updated time.Time
	started bool
}

// NewTokenBucket returns a bucket starting full, so that a freshly started
// process may immediately send its burst allowance rather than waiting a full
// refill period after every deploy.
func NewTokenBucket(ratePerSecond float64, burst int) *TokenBucket {
	if burst <= 0 {
		burst = 1
	}
	if ratePerSecond < 0 {
		ratePerSecond = 0
	}
	return &TokenBucket{Rate: ratePerSecond, Burst: burst}
}

// Allow implements Limiter.
func (b *TokenBucket) Allow(_ context.Context, _ message.Channel) (bool, time.Duration) {
	granted, retryAfter := b.Reserve(context.Background(), "", 1)
	return granted == 1, retryAfter
}

// Reserve implements Limiter.
func (b *TokenBucket) Reserve(_ context.Context, _ message.Channel, n int) (int, time.Duration) {
	if n <= 0 {
		return 0, 0
	}
	if n > b.Burst {
		n = b.Burst
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	now := time.Now()
	if b.Now != nil {
		now = b.Now()
	}
	if !b.started {
		b.tokens = float64(b.Burst)
		b.updated = now
		b.started = true
	} else if elapsed := now.Sub(b.updated); elapsed > 0 {
		b.tokens += elapsed.Seconds() * b.Rate
		if b.tokens > float64(b.Burst) {
			b.tokens = float64(b.Burst)
		}
		b.updated = now
	}

	granted := n
	if float64(granted) > b.tokens {
		granted = int(b.tokens)
	}
	b.tokens -= float64(granted)

	if granted == n {
		return granted, 0
	}

	// The wait for the first ungranted token. A zero rate never frees capacity,
	// and dividing by it would produce a NaN that a caller comparing against a
	// deadline would read as "no wait".
	if b.Rate <= 0 {
		return granted, 0
	}
	needed := float64(n - granted)
	return granted, time.Duration(needed / b.Rate * float64(time.Second))
}

var (
	_ Limiter = Unlimited
	_ Limiter = LimiterFunc(nil)
	_ Limiter = ChannelLimiter{}
	_ Limiter = (*TokenBucket)(nil)
)
