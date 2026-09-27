package dispatch

import (
	"math/rand"
	"testing"
	"time"

	"github.com/jiuyue1123/message-center/pkg/mcerr"
	"github.com/jiuyue1123/message-center/pkg/message"
)

func TestBackoffGrowsAndSaturates(t *testing.T) {
	b := Backoff{Base: 10 * time.Second, Factor: 3, Max: 2 * time.Minute, Jitter: JitterNone}

	// Delay(k) is the wait before attempt k, so attempt 1 is immediate and
	// attempt 2 waits Base.
	want := []time.Duration{
		0,
		0,
		10 * time.Second,
		30 * time.Second,
		90 * time.Second,
		2 * time.Minute,
		2 * time.Minute,
	}
	for attempt, w := range want {
		if got := b.Delay(attempt); got != w {
			t.Errorf("Delay(%d) = %s, want %s", attempt, got, w)
		}
	}
}

// TestBackoffDoesNotOverflow: converting an infinite float to a Duration yields a
// negative value, which would schedule the retry in the past and hot-loop against
// a provider that is already failing.
func TestBackoffDoesNotOverflow(t *testing.T) {
	b := Backoff{Base: time.Second, Factor: 2, Max: 0, Jitter: JitterNone}
	for _, attempt := range []int{64, 1000, 1 << 20} {
		if got := b.Delay(attempt); got < 0 {
			t.Fatalf("Delay(%d) = %s, which is negative", attempt, got)
		}
	}
}

func TestBackoffJitterStaysInRange(t *testing.T) {
	base := 30 * time.Second

	full := Backoff{Base: base, Factor: 2, Max: time.Hour, Jitter: JitterFull, Rand: rand.New(rand.NewSource(1))}
	equal := Backoff{Base: base, Factor: 2, Max: time.Hour, Jitter: JitterEqual, Rand: rand.New(rand.NewSource(1))}

	for i := 0; i < 200; i++ {
		if d := full.Delay(2); d < 0 || d > base {
			t.Fatalf("full jitter produced %s, outside [0, %s]", d, base)
		}
		if d := equal.Delay(2); d < base/2 || d > base {
			t.Fatalf("equal jitter produced %s, outside [%s, %s]", d, base/2, base)
		}
	}
}

func TestBackoffValidate(t *testing.T) {
	if err := (Backoff{Base: time.Second, Factor: 2, Max: time.Minute}).Validate(); err != nil {
		t.Fatalf("valid backoff rejected: %v", err)
	}
	if err := (Backoff{Base: time.Minute, Factor: 2, Max: time.Second}).Validate(); err == nil {
		t.Error("a max below the base can never grow and must be rejected")
	}
	if err := (Backoff{Base: time.Second, Factor: 0.5}).Validate(); err == nil {
		t.Error("a factor below 1 shrinks the delay and must be rejected")
	}
}

// TestNextAttemptAtAbortedDoesNotConsume: charging an aborted attempt against the
// budget would let a rolling deploy permanently fail every in-flight delivery
// without a provider call being made.
func TestNextAttemptAtAbortedDoesNotConsume(t *testing.T) {
	p := DefaultRetryPolicy
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)

	next, ok := p.NextAttemptAt(mcerr.KindAborted, p.MaxAttempts, now, 0)
	if !ok {
		t.Fatal("an aborted attempt must be re-queued, not exhausted")
	}
	if !next.Equal(now) {
		t.Fatalf("aborted attempt scheduled at %s, want it due immediately at %s", next, now)
	}
}

func TestNextAttemptAtRetriesThenExhausts(t *testing.T) {
	p := RetryPolicy{
		MaxAttempts:   3,
		LeaseDuration: 30 * time.Second,
		Backoff:       Backoff{Base: 10 * time.Second, Factor: 2, Max: time.Minute, Jitter: JitterNone},
	}
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)

	next, ok := p.NextAttemptAt(mcerr.KindRetryable, 1, now, 0)
	if !ok {
		t.Fatal("attempt 1 with budget left should retry")
	}
	// After the first failure the wait is Base, since Delay(attempt+1) is Delay(2).
	if want := now.Add(10 * time.Second); !next.Equal(want) {
		t.Fatalf("next = %s, want %s", next, want)
	}

	if _, ok := p.NextAttemptAt(mcerr.KindRetryable, 3, now, 0); ok {
		t.Fatal("attempt 3 of 3 must be exhausted")
	}
}

func TestNextAttemptAtPermanentDoesNotRetry(t *testing.T) {
	p := DefaultRetryPolicy
	now := time.Now()
	if _, ok := p.NextAttemptAt(mcerr.KindPermanent, 1, now, 0); ok {
		t.Fatal("a permanent failure must not be retried")
	}
	if p.ShouldRetry(mcerr.KindPermanent, 1) {
		t.Fatal("ShouldRetry disagrees with NextAttemptAt on a permanent failure")
	}
}

// TestNextAttemptAtHonoursProviderRetryAfter: a computed schedule would be
// throttled again, so the provider's own delay takes precedence.
func TestNextAttemptAtHonoursProviderRetryAfter(t *testing.T) {
	p := DefaultRetryPolicy
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)

	next, ok := p.NextAttemptAt(mcerr.KindRateLimited, 1, now, 5*time.Minute)
	if !ok {
		t.Fatal("a rate-limited failure with budget left should retry")
	}
	if want := now.Add(5 * time.Minute); !next.Equal(want) {
		t.Fatalf("next = %s, want the provider's %s", next, want)
	}
}

func TestNextAttemptAtNeverSchedulesInThePast(t *testing.T) {
	p := RetryPolicy{
		MaxAttempts:   5,
		LeaseDuration: time.Second,
		Backoff:       Backoff{Base: 0, Factor: 1, Max: 0, Jitter: JitterNone},
	}
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	next, ok := p.NextAttemptAt(mcerr.KindRetryable, 1, now, 0)
	if !ok {
		t.Fatal("should retry")
	}
	if !next.After(now) {
		t.Fatalf("next = %s, which is not after now", next)
	}
}

func TestPresetsAreValid(t *testing.T) {
	for name, p := range map[string]RetryPolicy{
		"default": DefaultRetryPolicy,
		"patient": PatientRetryPolicy,
		"urgent":  UrgentRetryPolicy,
		"none":    NoRetryPolicy,
	} {
		if err := p.Validate(); err != nil {
			t.Errorf("preset %q is invalid: %v", name, err)
		}
	}
	if UrgentRetryPolicy.DefaultExpiry > time.Hour {
		t.Error("the urgent preset's expiry is too long for a time-limited code")
	}
}

func TestStaticPolicyResolverPerChannel(t *testing.T) {
	r := StaticPolicyResolver{
		Policy: DefaultRetryPolicy,
		PerChannel: map[message.Channel]RetryPolicy{
			// SMS is billable per attempt.
			message.ChannelSMS: NoRetryPolicy,
		},
	}
	got := r.PolicyFor(nil, nil, &message.Delivery{Channel: message.ChannelSMS})
	if got.MaxAttempts != 1 {
		t.Fatalf("SMS policy has %d attempts, want 1", got.MaxAttempts)
	}
	got = r.PolicyFor(nil, nil, &message.Delivery{Channel: message.ChannelEmail})
	if got.MaxAttempts != DefaultRetryPolicy.MaxAttempts {
		t.Fatalf("email policy has %d attempts, want the default %d", got.MaxAttempts, DefaultRetryPolicy.MaxAttempts)
	}
}
