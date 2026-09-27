package dispatch

import (
	"context"
	"math"
	"math/rand"
	"time"

	"github.com/jiuyue1123/message-center/pkg/mcerr"
	"github.com/jiuyue1123/message-center/pkg/message"
)

// JitterStrategy decides how much randomness to add to a computed backoff.
//
// Without jitter, every delivery that failed at the same moment retries at the
// same moment, so a provider outage produces a synchronised wave of retries
// arriving exactly as the provider attempts to recover.
type JitterStrategy string

const (
	// JitterFull picks uniformly from [0, delay).
	JitterFull JitterStrategy = "full"

	// JitterEqual picks uniformly from [delay/2, delay), keeping a floor under
	// the wait while still spreading the wave.
	JitterEqual JitterStrategy = "equal"

	// JitterNone uses the computed delay exactly, for tests and for a channel
	// with a single delivery in flight.
	JitterNone JitterStrategy = "none"
)

var jitterStrategies = [...]JitterStrategy{JitterFull, JitterEqual, JitterNone}

// AllJitterStrategies returns every strategy.
func AllJitterStrategies() []JitterStrategy {
	out := make([]JitterStrategy, len(jitterStrategies))
	copy(out, jitterStrategies[:])
	return out
}

// Valid reports whether j is a recognised strategy.
func (j JitterStrategy) Valid() bool {
	for _, v := range jitterStrategies {
		if j == v {
			return true
		}
	}
	return false
}

func (j JitterStrategy) String() string { return string(j) }

// Backoff computes the delay before a retry.
type Backoff struct {
	// Base is the delay before the second attempt. The first attempt happens
	// immediately.
	Base time.Duration

	// Factor multiplies the delay per attempt. Typically 2. A factor of 1 is a
	// fixed delay.
	Factor float64

	// Max caps the delay. Zero means no cap, which reaches days within ten
	// attempts. Keep it well under the delivery's expiry.
	Max time.Duration

	// Jitter is the strategy applied to the computed delay.
	Jitter JitterStrategy

	// Rand supplies randomness, for tests. Nil means the shared source.
	Rand *rand.Rand
}

// Validate checks the backoff's parameters.
func (b Backoff) Validate() error {
	if b.Base < 0 {
		return mcerr.ErrInvalidArgument.WithMessage("backoff base cannot be negative")
	}
	if b.Factor < 1 {
		return mcerr.ErrInvalidArgument.WithMessage("backoff factor must be at least 1, got %v", b.Factor)
	}
	if b.Max < 0 {
		return mcerr.ErrInvalidArgument.WithMessage("backoff max cannot be negative")
	}
	if b.Max > 0 && b.Max < b.Base {
		return mcerr.ErrInvalidArgument.WithMessage("backoff max %s is below its base %s, so it could never grow", b.Max, b.Base)
	}
	if b.Jitter != "" && !b.Jitter.Valid() {
		return mcerr.ErrInvalidArgument.WithMessage("unknown jitter strategy %q", b.Jitter)
	}
	return nil
}

// Delay returns the wait before the attempt numbered attempt, which is 1-based.
// Attempt 1 is immediate and returns zero.
//
// The exponent saturates rather than overflowing: a large attempt count and a
// factor of 2 would otherwise produce an infinity, whose conversion to a
// Duration is negative and would schedule the retry in the past.
func (b Backoff) Delay(attempt int) time.Duration {
	if attempt <= 1 {
		return 0
	}
	factor := b.Factor
	if factor < 1 {
		factor = 1
	}

	maxDelay := b.Max
	if maxDelay <= 0 {
		maxDelay = math.MaxInt64
	}
	exponent := attempt - 2
	if exponent > 62 {
		exponent = 62
	}

	delay := float64(b.Base) * math.Pow(factor, float64(exponent))
	if delay > float64(maxDelay) {
		delay = float64(maxDelay)
	}
	if delay > float64(math.MaxInt64) {
		delay = float64(math.MaxInt64)
	}
	d := time.Duration(delay)
	if b.Max > 0 && d > b.Max {
		d = b.Max
	}

	return b.applyJitter(d)
}

// applyJitter spreads a delay according to the strategy.
func (b Backoff) applyJitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	switch b.Jitter {
	case JitterNone:
		return d
	case JitterEqual:
		half := d / 2
		return half + time.Duration(b.randInt63n(int64(d-half)+1))
	default: // JitterFull
		return time.Duration(b.randInt63n(int64(d) + 1))
	}
}

// randInt63n returns a value in [0, n). It prefers a caller-supplied source so
// that a test can be deterministic, and falls back to the global source, which is
// safe for concurrent use.
func (b Backoff) randInt63n(n int64) int64 {
	if n <= 0 {
		return 0
	}
	if b.Rand != nil {
		return b.Rand.Int63n(n)
	}
	return rand.Int63n(n)
}

// RetryPolicy is the complete retry behaviour for a channel or a message.
//
// It is resolved once, at fan-out, and copied onto each Delivery as concrete
// numbers, so that a policy change does not retroactively alter deliveries
// already in flight.
type RetryPolicy struct {
	// MaxAttempts caps attempts per delivery. At least 1: the first attempt
	// always happens, so 1 means "try once, never retry".
	MaxAttempts int

	// Backoff computes the delay between attempts.
	Backoff Backoff

	// LeaseDuration is how long a worker may hold a delivery before another may
	// reclaim it. It must exceed the channel's p99 send latency with margin, or
	// healthy workers will have deliveries stolen from them mid-flight.
	LeaseDuration time.Duration

	// DefaultExpiry is how long a delivery remains worth attempting, applied when
	// the message does not specify ExpiresAt. Without one, a delivery that keeps
	// failing retries until MaxAttempts, which for a time-limited message means it
	// can arrive long after the event.
	DefaultExpiry time.Duration

	// MaxDeferral bounds how long a preference deferral may last. Zero means the
	// delivery's own expiry is the only bound.
	MaxDeferral time.Duration
}

// Validate checks the policy.
func (p RetryPolicy) Validate() error {
	if p.MaxAttempts < 1 {
		return mcerr.ErrInvalidArgument.WithMessage("retry policy max attempts must be at least 1, got %d", p.MaxAttempts)
	}
	if err := p.Backoff.Validate(); err != nil {
		return err
	}
	if p.LeaseDuration <= 0 {
		return mcerr.ErrInvalidArgument.WithMessage("retry policy needs a positive lease duration")
	}
	if p.DefaultExpiry < 0 {
		return mcerr.ErrInvalidArgument.WithMessage("retry policy default expiry cannot be negative")
	}
	if p.MaxDeferral < 0 {
		return mcerr.ErrInvalidArgument.WithMessage("retry policy max deferral cannot be negative")
	}
	return nil
}

// NextAttemptAt computes when a failed delivery should next be tried, and reports
// whether there is a next attempt at all.
//
// attempt is the attempt number that just failed, 1-based. A provider-supplied
// providerRetryAfter overrides the computed backoff, since the provider knows
// when it will be ready and a computed schedule would be throttled again. A Kind
// that does not consume an attempt leaves the delivery due immediately.
func (p RetryPolicy) NextAttemptAt(kind mcerr.Kind, attempt int, now time.Time, providerRetryAfter time.Duration) (time.Time, bool) {
	if !kind.ConsumesAttempt() {
		return now, true
	}
	if !kind.Retryable() {
		return time.Time{}, false
	}
	if attempt >= p.MaxAttempts {
		return time.Time{}, false
	}
	if providerRetryAfter > 0 {
		return now.Add(providerRetryAfter), true
	}
	delay := p.Backoff.Delay(attempt + 1)
	if delay <= 0 {
		// A zero backoff would make the delivery immediately due, producing a hot
		// loop against a provider that is already failing.
		delay = time.Second
	}
	return now.Add(delay), true
}

// ShouldRetry reports whether a failure is worth another attempt. It agrees with
// NextAttemptAt by construction, since both delegate to the same Kind methods.
func (p RetryPolicy) ShouldRetry(kind mcerr.Kind, attempt int) bool {
	if !kind.ConsumesAttempt() {
		return true
	}
	return kind.Retryable() && attempt < p.MaxAttempts
}

// Presets.
var (
	// DefaultRetryPolicy suits a transactional message on a reliable provider.
	// Three attempts over roughly a minute and a half, expiring after a day.
	DefaultRetryPolicy = RetryPolicy{
		MaxAttempts:   3,
		LeaseDuration: 30 * time.Second,
		DefaultExpiry: 24 * time.Hour,
		Backoff: Backoff{
			Base:   10 * time.Second,
			Factor: 3,
			Max:    2 * time.Minute,
			Jitter: JitterFull,
		},
	}

	// PatientRetryPolicy suits a channel where eventual delivery is worth a long
	// wait and a duplicate is harmless. Five attempts over roughly half an hour;
	// the window is long enough that a time-limited message can outlive its event.
	PatientRetryPolicy = RetryPolicy{
		MaxAttempts:   5,
		LeaseDuration: 60 * time.Second,
		DefaultExpiry: 72 * time.Hour,
		Backoff: Backoff{
			Base:   30 * time.Second,
			Factor: 3,
			Max:    10 * time.Minute,
			Jitter: JitterFull,
		},
	}

	// UrgentRetryPolicy suits a message whose value decays in seconds. Two
	// attempts and a fifteen-minute expiry: a code delivered after it stops
	// working is worse than one never delivered.
	UrgentRetryPolicy = RetryPolicy{
		MaxAttempts:   2,
		LeaseDuration: 10 * time.Second,
		DefaultExpiry: 15 * time.Minute,
		MaxDeferral:   5 * time.Minute,
		Backoff: Backoff{
			Base:   3 * time.Second,
			Factor: 2,
			Max:    10 * time.Second,
			Jitter: JitterEqual,
		},
	}

	// NoRetryPolicy attempts once and gives up, for a channel where a duplicate
	// is worse than a miss. A non-idempotent webhook endpoint is the canonical
	// case.
	NoRetryPolicy = RetryPolicy{
		MaxAttempts:   1,
		LeaseDuration: 30 * time.Second,
		DefaultExpiry: 1 * time.Hour,
		Backoff: Backoff{
			Base:   time.Second,
			Factor: 1,
			Max:    time.Second,
			Jitter: JitterNone,
		},
	}
)

// PolicyResolver returns the retry policy for a channel and message.
type PolicyResolver interface {
	// PolicyFor returns the policy to use. It must return a valid policy: the
	// zero value would give every delivery a MaxAttempts of zero and a zero
	// lease.
	PolicyFor(ctx context.Context, m *message.Message, d *message.Delivery) RetryPolicy
}

// PolicyResolverFunc adapts a function to PolicyResolver.
type PolicyResolverFunc func(ctx context.Context, m *message.Message, d *message.Delivery) RetryPolicy

// PolicyFor implements PolicyResolver.
func (f PolicyResolverFunc) PolicyFor(ctx context.Context, m *message.Message, d *message.Delivery) RetryPolicy {
	return f(ctx, m, d)
}

// StaticPolicyResolver returns one policy for every delivery, with per-channel
// overrides.
type StaticPolicyResolver struct {
	// Policy is returned for every request.
	Policy RetryPolicy

	// PerChannel overrides it for named channels.
	PerChannel map[message.Channel]RetryPolicy
}

// PolicyFor implements PolicyResolver.
func (s StaticPolicyResolver) PolicyFor(_ context.Context, _ *message.Message, d *message.Delivery) RetryPolicy {
	if p, ok := s.PerChannel[d.Channel]; ok {
		return p
	}
	return s.Policy
}

var (
	_ PolicyResolver = PolicyResolverFunc(nil)
	_ PolicyResolver = StaticPolicyResolver{}
)
