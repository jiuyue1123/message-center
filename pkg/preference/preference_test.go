package preference

import (
	"context"
	"testing"
	"time"

	"github.com/jiuyue1123/message-center/pkg/message"
)

var shanghai = time.FixedZone("CST", 8*3600)

// TestQuietHoursWrapsMidnight covers the window shape almost every user
// configures. The naive check "after 22:00 and before 08:00" is false at every
// instant for a window crossing midnight.
func TestQuietHoursWrapsMidnight(t *testing.T) {
	q := &QuietHours{StartMinute: 22 * 60, EndMinute: 8 * 60}

	cases := []struct {
		localHour, localMin int
		want                bool
	}{
		{21, 59, false},
		{22, 0, true},  // start is inclusive
		{23, 40, true}, // before midnight
		{0, 30, true},  // after midnight, still inside
		{7, 59, true},
		{8, 0, false}, // end is exclusive
		{12, 0, false},
	}
	for _, tc := range cases {
		at := time.Date(2026, 9, 27, tc.localHour, tc.localMin, 0, 0, shanghai)
		if got := q.Active(at, shanghai); got != tc.want {
			t.Errorf("Active(%02d:%02d) = %t, want %t", tc.localHour, tc.localMin, got, tc.want)
		}
	}
}

func TestQuietHoursUsesRecipientTimezone(t *testing.T) {
	q := &QuietHours{StartMinute: 22 * 60, EndMinute: 8 * 60}

	// 23:40 in Shanghai is 15:40 UTC: inside the window locally, outside it in UTC.
	at := time.Date(2026, 9, 27, 23, 40, 0, 0, shanghai)
	if !q.Active(at, shanghai) {
		t.Error("window should be active in the recipient's zone")
	}
	if q.Active(at, time.UTC) {
		t.Error("window must not be evaluated in UTC when the recipient is not in UTC")
	}
}

func TestQuietHoursNonWrappingWindow(t *testing.T) {
	q := &QuietHours{StartMinute: 13 * 60, EndMinute: 14 * 60}
	if q.Active(time.Date(2026, 9, 27, 12, 59, 0, 0, time.UTC), time.UTC) {
		t.Error("12:59 should be outside a 13:00-14:00 window")
	}
	if !q.Active(time.Date(2026, 9, 27, 13, 30, 0, 0, time.UTC), time.UTC) {
		t.Error("13:30 should be inside a 13:00-14:00 window")
	}
	if q.Active(time.Date(2026, 9, 27, 14, 0, 0, 0, time.UTC), time.UTC) {
		t.Error("14:00 should be outside a 13:00-14:00 window")
	}
}

func TestQuietHoursDays(t *testing.T) {
	// A Saturday-only window of 22:00-08:00 covers Saturday 22:00 through Sunday
	// 08:00. 2026-09-26 is a Saturday, 2026-09-27 a Sunday.
	q := &QuietHours{
		StartMinute: 22 * 60,
		EndMinute:   8 * 60,
		Days:        []time.Weekday{time.Saturday},
	}
	satLate := time.Date(2026, 9, 26, 23, 0, 0, 0, time.UTC)
	sunEarly := time.Date(2026, 9, 27, 3, 0, 0, 0, time.UTC)
	sunLate := time.Date(2026, 9, 27, 23, 0, 0, 0, time.UTC)
	friLate := time.Date(2026, 9, 25, 23, 0, 0, 0, time.UTC)

	if !q.Active(satLate, time.UTC) {
		t.Error("Saturday 23:00 must be inside a Saturday-only window")
	}
	if !q.Active(sunEarly, time.UTC) {
		t.Error("Sunday 03:00 must still be inside the window that began Saturday")
	}
	if q.Active(sunLate, time.UTC) {
		t.Error("Sunday 23:00 must be outside a Saturday-only window")
	}
	if q.Active(friLate, time.UTC) {
		t.Error("Friday 23:00 must be outside a Saturday-only window")
	}
}

func TestQuietHoursNextEnd(t *testing.T) {
	q := &QuietHours{StartMinute: 22 * 60, EndMinute: 8 * 60}

	// Before midnight the window ends the following morning.
	at := time.Date(2026, 9, 27, 23, 40, 0, 0, shanghai)
	want := time.Date(2026, 9, 28, 8, 0, 0, 0, shanghai)
	if got := q.NextEnd(at, shanghai); !got.Equal(want) {
		t.Errorf("NextEnd(before midnight) = %s, want %s", got, want)
	}

	// After midnight it ends the same morning.
	at = time.Date(2026, 9, 28, 3, 0, 0, 0, shanghai)
	want = time.Date(2026, 9, 28, 8, 0, 0, 0, shanghai)
	if got := q.NextEnd(at, shanghai); !got.Equal(want) {
		t.Errorf("NextEnd(after midnight) = %s, want %s", got, want)
	}

	outside := time.Date(2026, 9, 28, 12, 0, 0, 0, shanghai)
	if got := q.NextEnd(outside, shanghai); !got.Equal(outside) {
		t.Errorf("NextEnd(outside window) = %s, want %s", got, outside)
	}

	// The returned instant must be outside the window, or a deferral loops.
	if q.Active(q.NextEnd(at, shanghai), shanghai) {
		t.Error("NextEnd returned an instant that is still inside the window")
	}
}

func TestQuietHoursExemptions(t *testing.T) {
	q := &QuietHours{
		StartMinute:    22 * 60,
		EndMinute:      8 * 60,
		ExemptBizTypes: []string{"password_reset"},
	}
	d := &message.Delivery{Channel: message.ChannelSMS}
	if !q.ShouldExempt(d, "password_reset") {
		t.Error("an exempt biz type must bypass quiet hours")
	}
	if q.ShouldExempt(d, "order_shipped") {
		t.Error("a non-exempt biz type must not bypass quiet hours")
	}
}

func TestQuietHoursValidate(t *testing.T) {
	if err := (&QuietHours{StartMinute: 1320, EndMinute: 480}).Validate(); err != nil {
		t.Fatalf("a wrapping window must be valid: %v", err)
	}
	if err := (&QuietHours{StartMinute: 480, EndMinute: 480}).Validate(); err == nil {
		t.Error("an empty window must be rejected")
	}
	if err := (&QuietHours{StartMinute: -1, EndMinute: 480}).Validate(); err == nil {
		t.Error("a negative start must be rejected")
	}
}

func TestMemCounterSlidingWindow(t *testing.T) {
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	c := NewMemCounter()
	c.Now = func() time.Time { return now }
	ctx := context.Background()

	for i := 1; i <= 3; i++ {
		count, allowed, _, err := c.IncrIfBelow(ctx, "k", time.Hour, 3, now)
		if err != nil {
			t.Fatalf("IncrIfBelow: %v", err)
		}
		if !allowed {
			t.Fatalf("call %d was denied, want allowed (count=%d)", i, count)
		}
	}

	count, allowed, retryAfter, err := c.IncrIfBelow(ctx, "k", time.Hour, 3, now)
	if err != nil {
		t.Fatalf("IncrIfBelow: %v", err)
	}
	if allowed {
		t.Fatal("the 4th call should be denied")
	}
	// The over-limit call is still counted, so the true volume is observable.
	if count != 4 {
		t.Fatalf("count = %d, want 4", count)
	}
	if retryAfter <= 0 {
		t.Fatal("a denial should report when capacity frees up")
	}

	now = now.Add(time.Hour)
	if _, allowed, _, _ := c.IncrIfBelow(ctx, "k", time.Hour, 3, now); !allowed {
		t.Fatal("the window should have slid past the earlier events")
	}
}

func TestMemCounterZeroMaxDeniesEverything(t *testing.T) {
	c := NewMemCounter()
	_, allowed, _, err := c.IncrIfBelow(context.Background(), "k", time.Hour, 0, time.Now())
	if err != nil {
		t.Fatalf("IncrIfBelow: %v", err)
	}
	if allowed {
		t.Fatal("a cap of zero must deny")
	}
}

func TestCapRuleCounterKeySeparatesScopes(t *testing.T) {
	d := &message.Delivery{Channel: message.ChannelSMS, TenantID: "acme"}
	byChannel := &CapRule{Name: "sms", Scope: CapScopeChannel, Window: time.Hour, Max: 1, Action: CapDrop}
	byBiz := &CapRule{Name: "sms", Scope: CapScopeBizType, Window: time.Hour, Max: 1, Action: CapDrop}

	// Same rule name, different scopes: sharing a bucket would let the tighter
	// rule enforce the looser one's limit.
	if byChannel.CounterKey("acme", "u1", d, "order_shipped") == byBiz.CounterKey("acme", "u1", d, "order_shipped") {
		t.Fatal("different scopes produced the same counter key")
	}
	if byChannel.CounterKey("acme", "u1", d, "x") == byChannel.CounterKey("acme", "u2", d, "x") {
		t.Fatal("different users produced the same counter key")
	}
}

func TestHardPreferencesEvaluatedAtAccept(t *testing.T) {
	prefs := &Preference{
		UserID:        "u1",
		ChannelOptOut: message.NewChannelSet(message.ChannelSMS),
	}
	e := &StaticEvaluator{Prefs: prefs}

	d := &message.Delivery{
		Channel:   message.ChannelSMS,
		Recipient: message.Recipient{UserID: "u1"},
	}
	dec, err := e.Evaluate(context.Background(), Request{
		Delivery: d, BizType: "order_shipped", Now: time.Now(), Phase: PhaseAccept,
	})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if !dec.Dropped() || dec.Reason != ReasonChannelOptOut {
		t.Fatalf("decision = %+v, want a drop for channel opt-out", dec)
	}
}

func TestTemporalPreferencesNotEvaluatedAtAccept(t *testing.T) {
	prefs := &Preference{
		UserID:     "u1",
		QuietHours: &QuietHours{StartMinute: 22 * 60, EndMinute: 8 * 60},
	}
	e := &StaticEvaluator{Prefs: prefs}

	at := time.Date(2026, 9, 27, 23, 40, 0, 0, time.UTC)
	d := &message.Delivery{Channel: message.ChannelEmail, Recipient: message.Recipient{UserID: "u1"}}

	accept, err := e.Evaluate(context.Background(), Request{
		Delivery: d, BizType: "x", Preference: prefs, Now: at, Phase: PhaseAccept,
	})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	// At accept time there is no delivery to defer and no send time to evaluate
	// against, so the check is deferred to the claim path.
	if !accept.Allowed() {
		t.Fatalf("accept phase = %+v, want allow", accept)
	}

	claim, err := e.Evaluate(context.Background(), Request{
		Delivery: d, BizType: "x", Preference: prefs, Now: at, Phase: PhaseClaim,
	})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if !claim.Deferred() || claim.Reason != ReasonQuietHours {
		t.Fatalf("claim phase = %+v, want a quiet-hours deferral", claim)
	}
	if err := claim.Validate(); err != nil {
		t.Fatalf("a deferral without a defer time is a hot loop: %v", err)
	}
}

func TestDeferralPastExpiryBecomesDrop(t *testing.T) {
	prefs := &Preference{
		UserID:     "u1",
		QuietHours: &QuietHours{StartMinute: 22 * 60, EndMinute: 8 * 60},
	}
	e := &StaticEvaluator{Prefs: prefs}

	at := time.Date(2026, 9, 27, 23, 40, 0, 0, time.UTC)
	d := &message.Delivery{
		Channel:   message.ChannelSMS,
		Recipient: message.Recipient{UserID: "u1"},
		// Expires before quiet hours end, so deferring cannot help.
		ExpiresAt: at.Add(time.Hour),
	}
	dec, err := e.Evaluate(context.Background(), Request{
		Delivery: d, BizType: "otp", Preference: prefs, Now: at, Phase: PhaseClaim,
	})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	// The reason is the expiry rather than the quiet hours, so that the two are
	// distinguishable in metrics.
	if !dec.Dropped() || dec.Reason != ReasonExpired {
		t.Fatalf("decision = %+v, want a drop with reason %q", dec, ReasonExpired)
	}
}

func TestFrequencyCapDefers(t *testing.T) {
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	counter := NewMemCounter()
	counter.Now = func() time.Time { return now }

	rules := []CapRule{{
		Name: "sms-daily", Scope: CapScopeAll, Window: time.Hour,
		Max: 1, Action: CapDefer,
	}}
	e, err := NewStaticEvaluator(&Preference{UserID: "u1"}, rules, counter)
	if err != nil {
		t.Fatalf("NewStaticEvaluator: %v", err)
	}
	d := &message.Delivery{Channel: message.ChannelSMS, Recipient: message.Recipient{UserID: "u1"}}

	first, err := e.Evaluate(context.Background(), Request{
		Delivery: d, BizType: "x", Now: now, Phase: PhaseClaim,
	})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if !first.Allowed() {
		t.Fatalf("first message denied: %+v", first)
	}

	second, err := e.Evaluate(context.Background(), Request{
		Delivery: d, BizType: "x", Now: now, Phase: PhaseClaim,
	})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if !second.Deferred() || second.Reason != ReasonFrequencyCap {
		t.Fatalf("second message = %+v, want a frequency-cap deferral", second)
	}
	if second.Rule != "sms-daily" {
		t.Fatalf("decision names rule %q, want sms-daily", second.Rule)
	}
}

func TestNewStaticEvaluatorRejectsUnenforceableCaps(t *testing.T) {
	rules := []CapRule{{
		Name: "x", Scope: CapScopeAll, Window: time.Hour, Max: 1, Action: CapDrop,
	}}
	if _, err := NewStaticEvaluator(nil, rules, nil); err == nil {
		t.Fatal("rules without a counter must be rejected")
	}
	if _, err := NewStaticEvaluator(nil, rules, NewMemCounter()); err != nil {
		t.Fatalf("rules with a counter should be accepted: %v", err)
	}
}

func TestSuppressedAddressIsCaseInsensitiveForEmail(t *testing.T) {
	p := &Preference{
		UserID:              "u1",
		SuppressedAddresses: []message.Address{message.NewAddress(message.FormEmail, "Ada@Example.com")},
	}
	if !p.IsAddressSuppressed(message.NewAddress(message.FormEmail, "ada@example.com")) {
		t.Error("email suppression should be case-insensitive")
	}
	// Device tokens are case-sensitive: conflating two distinct tokens breaks push.
	p.SuppressedAddresses = []message.Address{message.NewAddress(message.FormDeviceToken, "AbC")}
	if p.IsAddressSuppressed(message.NewAddress(message.FormDeviceToken, "abc")) {
		t.Error("device token suppression must be case-sensitive")
	}
}
