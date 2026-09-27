package message

import (
	"strings"
	"testing"
	"time"
)

func TestIDRoundTripAndOrdering(t *testing.T) {
	base := time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC)

	var prev ID
	for i := 0; i < 64; i++ {
		id, err := NewAt(base.Add(time.Duration(i)*time.Millisecond), nil)
		if err != nil {
			t.Fatalf("NewAt: %v", err)
		}
		if !id.Valid() {
			t.Fatalf("generated id %q is not valid", id)
		}
		if prev != "" && id <= prev {
			t.Fatalf("id %q does not sort after %q", id, prev)
		}
		if got := id.Time(); !got.Equal(base.Add(time.Duration(i) * time.Millisecond)) {
			t.Fatalf("id %q timestamp = %s, want %s", id, got, base.Add(time.Duration(i)*time.Millisecond))
		}
		prev = id
	}
}

func TestIDParseRejectsMalformed(t *testing.T) {
	cases := map[string]string{
		"too short":       "01JCK8",
		"too long":        "01JCK8AAAAAAAAAAAAAAAAAAAAAA",
		"invalid rune":    "01JCK8!!!!!!!!!!!!!!!!!!!!!",
		"bad leading bit": "Z1JCK8AAAAAAAAAAAAAAAAAAAA",
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse(in); err == nil {
				t.Fatalf("Parse(%q) succeeded, want an error", in)
			}
		})
	}
}

func TestIDParseAcceptsCrockfordAliases(t *testing.T) {
	id, err := NewAt(time.UnixMilli(1_700_000_000_000), strings.NewReader(strings.Repeat("A", 10)))
	if err != nil {
		t.Fatalf("NewAt: %v", err)
	}
	lowered := ID(strings.ToLower(string(id)))
	parsed, err := Parse(string(lowered))
	if err != nil {
		t.Fatalf("Parse(lowercase) failed: %v", err)
	}
	if got, want := parsed.Canonical(), id.Canonical(); got != want {
		t.Fatalf("Canonical(lowercase id) = %q, want %q", got, want)
	}
}

func TestIDBytesRoundTripPreservesOrdering(t *testing.T) {
	// The base is anchored to now rather than a fixed date: NewAt clamps a
	// backwards clock, so a hardcoded date earlier than one used by another test
	// would collapse every timestamp to the same value.
	base := time.Now().UnixMilli() + int64(time.Hour/time.Millisecond)

	var prev [16]byte
	for i := 0; i < 32; i++ {
		id, err := NewAt(time.UnixMilli(base+int64(i)), nil)
		if err != nil {
			t.Fatalf("NewAt: %v", err)
		}
		b, err := id.Bytes()
		if err != nil {
			t.Fatalf("Bytes: %v", err)
		}
		if i > 0 && !lessBytes(prev, b) {
			t.Fatalf("byte encoding of %q does not sort after the previous id", id)
		}
		prev = b
	}
}

// TestNewAtClampsABackwardsClock covers the ordering guarantee against an NTP
// correction. The assertion is on the encoded timestamp rather than the whole ID:
// two IDs sharing a millisecond are ordered by their random component.
func TestNewAtClampsABackwardsClock(t *testing.T) {
	future := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	first, err := NewAt(future, nil)
	if err != nil {
		t.Fatalf("NewAt: %v", err)
	}

	second, err := NewAt(future.AddDate(-1, 0, 0), nil)
	if err != nil {
		t.Fatalf("NewAt: %v", err)
	}
	if got, prev := second.Time(), first.Time(); got.Before(prev) {
		t.Fatalf("id minted after a backwards clock step carries %s, before the previous %s", got, prev)
	}

	// The clamp is a floor, not a freeze.
	later, err := NewAt(future.Add(time.Hour), nil)
	if err != nil {
		t.Fatalf("NewAt: %v", err)
	}
	if want := future.Add(time.Hour); !later.Time().Equal(want) {
		t.Fatalf("timestamp = %s, want %s", later.Time(), want)
	}
}

func lessBytes(a, b [16]byte) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

func TestMilestoneNeverGoesBackwards(t *testing.T) {
	d := &Delivery{Milestone: MilestoneNone}
	at := time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC)

	// A click arrives before the delivery and read receipts.
	if changed := d.ApplyReceipt(&Receipt{Kind: ReceiptClicked, At: at.Add(3 * time.Minute)}); !changed {
		t.Fatal("click receipt did not advance the milestone")
	}
	if d.Milestone != MilestoneClicked {
		t.Fatalf("milestone = %q, want %q", d.Milestone, MilestoneClicked)
	}

	d.ApplyReceipt(&Receipt{Kind: ReceiptDelivered, At: at.Add(1 * time.Minute)})
	if d.Milestone != MilestoneClicked {
		t.Fatalf("late delivery receipt moved the milestone back to %q", d.Milestone)
	}
	d.ApplyReceipt(&Receipt{Kind: ReceiptRead, At: at.Add(2 * time.Minute)})
	if d.Milestone != MilestoneClicked {
		t.Fatalf("late read receipt moved the milestone back to %q", d.Milestone)
	}
}

func TestApplyReceiptIsIdempotent(t *testing.T) {
	d := &Delivery{}
	at := time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC)
	r := &Receipt{Kind: ReceiptDelivered, At: at}

	if changed := d.ApplyReceipt(r); !changed {
		t.Fatal("first delivery receipt reported no change")
	}
	if changed := d.ApplyReceipt(r); changed {
		t.Fatal("duplicate delivery receipt reported a change")
	}
}

func TestNegativeReceiptsAreOrthogonal(t *testing.T) {
	d := &Delivery{}
	at := time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC)

	d.ApplyReceipt(&Receipt{Kind: ReceiptDelivered, At: at})
	if got := d.Milestone; got != MilestoneDelivered {
		t.Fatalf("milestone = %q, want %q", got, MilestoneDelivered)
	}
	if !d.ReadAt.IsZero() {
		t.Fatal("a delivery receipt set ReadAt")
	}

	d.ApplyReceipt(&Receipt{Kind: ReceiptBounced, BounceType: BounceSoft, At: at.Add(time.Minute)})
	if d.Milestone != MilestoneDelivered {
		t.Fatalf("a bounce moved the milestone to %q", d.Milestone)
	}
	if d.Bounced != BounceSoft {
		t.Fatalf("bounced = %q, want %q", d.Bounced, BounceSoft)
	}
	if !d.BouncedAt.Equal(at.Add(time.Minute)) {
		t.Fatalf("bounced_at = %s", d.BouncedAt)
	}

	d.ApplyReceipt(&Receipt{Kind: ReceiptBounced, BounceType: BounceHard, At: at.Add(2 * time.Minute)})
	if d.Bounced != BounceHard {
		t.Fatalf("hard bounce did not upgrade soft: bounced = %q", d.Bounced)
	}
	d.ApplyReceipt(&Receipt{Kind: ReceiptBounced, BounceType: BounceSoft, At: at.Add(3 * time.Minute)})
	if d.Bounced != BounceHard {
		t.Fatalf("soft bounce downgraded a hard one: bounced = %q", d.Bounced)
	}
}

func TestBounceShouldSuppress(t *testing.T) {
	if !BounceHard.ShouldSuppress() {
		t.Error("hard bounce should suppress")
	}
	if BounceSoft.ShouldSuppress() || BounceUnknown.ShouldSuppress() {
		t.Error("soft and unknown bounces must not suppress an address")
	}
}

func TestDeliveryReleaseDoesNotConsumeAttempt(t *testing.T) {
	if !DeliverySending.CanTransition(DeliveryPending) {
		t.Fatal("sending -> pending must be legal: it is the lease-release path")
	}
	if !DeliverySending.CanTransition(DeliveryRetrying) {
		t.Fatal("sending -> retrying must be legal")
	}
	if DeliveryAccepted.CanTransition(DeliveryPending) {
		t.Fatal("accepted -> pending must be illegal: it would resend a delivered message")
	}
	if DeliveryFailed.CanTransition(DeliveryRetrying) {
		t.Fatal("failed -> retrying must be illegal: failed is terminal")
	}
}

func TestDeriveStatus(t *testing.T) {
	cases := []struct {
		name string
		in   []DeliveryStatus
		want MessageStatus
	}{
		{"no deliveries", nil, StatusPending},
		{"one still working", []DeliveryStatus{DeliveryAccepted, DeliveryRetrying}, StatusPending},
		{"all accepted", []DeliveryStatus{DeliveryAccepted, DeliveryAccepted}, StatusSucceeded},
		{"one failed", []DeliveryStatus{DeliveryAccepted, DeliveryFailed}, StatusPartiallySucceeded},
		{"all failed", []DeliveryStatus{DeliveryFailed, DeliveryExpired}, StatusFailed},
		{"all skipped is not a failure", []DeliveryStatus{DeliverySkipped, DeliverySkipped}, StatusCanceled},
		{"skipped alongside success is partial", []DeliveryStatus{DeliveryAccepted, DeliverySkipped}, StatusPartiallySucceeded},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := DeriveStatus(tc.in); got != tc.want {
				t.Fatalf("DeriveStatus(%v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestFunnelCountsValidate(t *testing.T) {
	ok := FunnelCounts{Sent: 100, Delivered: 90, Read: 40, Clicked: 10}
	if err := ok.Validate(); err != nil {
		t.Fatalf("consistent counts rejected: %v", err)
	}
	bad := FunnelCounts{Sent: 100, Delivered: 90, Read: 120}
	if err := bad.Validate(); err == nil {
		t.Fatal("read > delivered must be rejected")
	}
}

func TestAddressMasking(t *testing.T) {
	cases := []struct {
		addr Address
		want string
	}{
		{NewAddress(FormEmail, "ada@example.com"), "a****@example.com"},
		{NewAddress(FormPhone, "+8613800138000"), "+861****8000"},
		{NewAddress(FormUserID, "u_10231"), "u_10231"},
		{NewAddress(FormWebhookURL, "https://example.com/hook"), "https://example.com/hook"},
	}
	for _, tc := range cases {
		if got := tc.addr.Masked(); got != tc.want {
			t.Errorf("Masked(%q/%q) = %q, want %q", tc.addr.Form, tc.addr.Value, got, tc.want)
		}
	}
}

func TestChannelSetOrderingIsStable(t *testing.T) {
	s := NewChannelSet(ChannelWebhook, ChannelInApp, ChannelEmail)
	want := []Channel{ChannelInApp, ChannelEmail, ChannelWebhook}
	got := s.Channels()
	if len(got) != len(want) {
		t.Fatalf("Channels() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Channels() = %v, want %v", got, want)
		}
	}
}

func TestRecipientFirstAcceptingHonoursFormOrder(t *testing.T) {
	r := Recipient{
		UserID: "u_1",
		Addresses: []Address{
			NewAddress(FormDeviceToken, "fcm_abc"),
			NewAddress(FormEmail, "ada@example.com"),
		},
	}
	// The Sender prefers email, so iterating addresses rather than forms would
	// pick the device token instead.
	got, ok := r.FirstAccepting([]AddressForm{FormEmail, FormDeviceToken})
	if !ok || got.Form != FormEmail {
		t.Fatalf("FirstAccepting picked %q, want email", got.Form)
	}
	if _, ok := r.FirstAccepting([]AddressForm{FormPhone}); ok {
		t.Fatal("FirstAccepting found a phone number that is not present")
	}
}
