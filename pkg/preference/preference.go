// Package preference decides whether a message may be sent to a user at a given
// moment.
//
// It answers three questions: whether the user has opted out of a channel or an
// event type, whether the current time falls inside a quiet-hours window, and
// whether a frequency cap has been reached.
//
// Evaluation happens in two phases. Hard preferences, which are immutable facts
// about the user, are evaluated on the accept path, so that a message violating
// one never produces a delivery row. Temporal preferences, which depend on when
// the message would actually go out, are evaluated at claim time, since that is
// the only point at which a scheduled, deferred, or retried message's send time
// is known.
//
// A deferral is not a failure. When quiet hours or a frequency cap declines a
// delivery, it returns to pending with a future NextAttemptAt and its
// AttemptCount unchanged, following the Release path rather than CommitAttempt.
// Recording a deferral as a failed attempt would make a nightly quiet window fail
// every message delivered into it, without a single provider call being made.
package preference

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jiuyue1123/message-center/pkg/mcerr"
	"github.com/jiuyue1123/message-center/pkg/message"
)

// Preference is one user's notification settings. The zero value is a usable
// default: no opt-outs, no quiet hours, no caps.
type Preference struct {
	// UserID is the user these settings belong to.
	UserID string `json:"user_id"`

	// TenantID scopes them.
	TenantID string `json:"tenant_id,omitempty"`

	// ChannelOptOut lists channels the user has switched off entirely. Evaluated
	// on the accept path.
	ChannelOptOut message.ChannelSet `json:"channel_opt_out,omitempty"`

	// BizTypeOptOut lists caller event types the user has switched off, keyed by
	// channel. A channel key absent from the map means no per-event opt-outs on
	// that channel.
	BizTypeOptOut map[message.Channel]map[string]struct{} `json:"biz_type_opt_out,omitempty"`

	// QuietHours is the window in which the user does not want to be disturbed.
	// Nil means no quiet hours.
	QuietHours *QuietHours `json:"quiet_hours,omitempty"`

	// Caps are per-user frequency limits.
	Caps []CapRule `json:"caps,omitempty"`

	// SuppressedAddresses lists addresses reported as undeliverable, by a hard
	// bounce or by the user. Evaluated on the accept path.
	SuppressedAddresses []message.Address `json:"suppressed_addresses,omitempty"`

	// Timezone is the IANA zone used for quiet hours. Empty means UTC.
	//
	// It duplicates the Recipient's timezone deliberately: this is the zone the
	// user configured, and it wins when the two disagree.
	Timezone string `json:"timezone,omitempty"`

	// Locale is the user's preferred language, used for template selection when
	// the message does not carry one.
	Locale string `json:"locale,omitempty"`

	// UnsubscribedAt is when the user last opted out of anything, kept for audit.
	UnsubscribedAt time.Time `json:"unsubscribed_at,omitempty"`

	// UpdatedAt is when these settings last changed.
	UpdatedAt time.Time `json:"updated_at"`
}

// IsChannelOptedOut reports whether the user has switched a channel off.
func (p *Preference) IsChannelOptedOut(ch message.Channel) bool {
	return p != nil && p.ChannelOptOut.Has(ch)
}

// IsBizTypeOptedOut reports whether the user has suppressed one event type on one
// channel.
func (p *Preference) IsBizTypeOptedOut(ch message.Channel, bizType string) bool {
	if p == nil {
		return false
	}
	set, ok := p.BizTypeOptOut[ch]
	if !ok {
		return false
	}
	_, ok = set[bizType]
	return ok
}

// IsAddressSuppressed reports whether an address has been marked undeliverable.
//
// Comparison is case-insensitive for email and exact for every other form: email
// addresses are case-insensitive at every provider that matters, while phone
// numbers and device tokens are not.
func (p *Preference) IsAddressSuppressed(a message.Address) bool {
	if p == nil || a.Value == "" {
		return false
	}
	for _, s := range p.SuppressedAddresses {
		if s.Form != a.Form {
			continue
		}
		if s.Form == message.FormEmail {
			if strings.EqualFold(s.Value, a.Value) {
				return true
			}
			continue
		}
		if s.Value == a.Value {
			return true
		}
	}
	return false
}

// Location resolves the user's timezone, falling back to UTC for an unparseable
// value. A malformed zone should be surfaced by a validation job rather than by
// refusing to send the user anything.
func (p *Preference) Location() *time.Location {
	if p == nil || p.Timezone == "" {
		return time.UTC
	}
	loc, err := time.LoadLocation(p.Timezone)
	if err != nil {
		return time.UTC
	}
	return loc
}

// Validate checks the settings.
func (p *Preference) Validate() error {
	if strings.TrimSpace(p.UserID) == "" {
		return mcerr.ErrInvalidArgument.WithMessage("preference requires a user_id")
	}
	if p.Timezone != "" {
		if _, err := time.LoadLocation(p.Timezone); err != nil {
			return mcerr.ErrInvalidArgument.WithMessage("unknown timezone %q", p.Timezone)
		}
	}
	if p.QuietHours != nil {
		if err := p.QuietHours.Validate(); err != nil {
			return err
		}
	}
	for i, c := range p.Caps {
		if err := c.Validate(); err != nil {
			return mcerr.From(err).WithMessage("cap %d: %s", i, mcerr.From(err).Message)
		}
	}
	return nil
}

// QuietHours is a recurring daily window in which messages should not be sent.
//
// The window is evaluated in the recipient's timezone, never the server's, and
// may cross midnight. The usual configuration, 22:00 to 08:00, does cross it, so
// StartMinute greater than EndMinute is the common case rather than an edge one.
type QuietHours struct {
	// StartMinute is minutes since local midnight, inclusive. 22:00 is 1320.
	StartMinute int `json:"start_minute"`

	// EndMinute is minutes since local midnight, exclusive. 08:00 is 480. It is
	// exclusive so that adjacent windows tile without overlap and a window ending
	// at 08:00 permits a message at exactly 08:00.
	EndMinute int `json:"end_minute"`

	// Days restricts the window to specific days, empty for every day. The day
	// refers to the day the window starts on, so a Saturday-only window covers
	// Saturday 22:00 through Sunday 08:00.
	Days []time.Weekday `json:"days,omitempty"`

	// ExemptBizTypes lists event types that bypass quiet hours entirely, which is
	// how a password reset or a one-time code reaches a user overnight.
	ExemptBizTypes []string `json:"exempt_biz_types,omitempty"`

	// ExemptChannels lists channels that bypass quiet hours.
	ExemptChannels message.ChannelSet `json:"exempt_channels,omitempty"`

	// ExemptPriorities lists priorities that bypass quiet hours.
	ExemptPriorities []message.Priority `json:"exempt_priorities,omitempty"`
}

// Validate checks the window.
func (q *QuietHours) Validate() error {
	if q.StartMinute < 0 || q.StartMinute >= minutesPerDay {
		return mcerr.ErrInvalidArgument.WithMessage("quiet hours start minute %d is outside the day", q.StartMinute)
	}
	if q.EndMinute < 0 || q.EndMinute > minutesPerDay {
		return mcerr.ErrInvalidArgument.WithMessage("quiet hours end minute %d is outside the day", q.EndMinute)
	}
	if q.StartMinute == q.EndMinute {
		return mcerr.ErrInvalidArgument.WithMessage("quiet hours start and end are the same, which is an empty or infinite window")
	}
	for _, d := range q.Days {
		if d < time.Sunday || d > time.Saturday {
			return mcerr.ErrInvalidArgument.WithMessage("quiet hours list an invalid weekday %d", d)
		}
	}
	for _, p := range q.ExemptPriorities {
		if !p.Valid() {
			return mcerr.ErrInvalidArgument.WithMessage("quiet hours list an unknown priority %q", p)
		}
	}
	return nil
}

const minutesPerDay = 24 * 60

// Active reports whether the window is in effect at the given instant.
//
// The location is passed explicitly rather than read from the time value, which
// is almost always in UTC when loaded from a database while the window is defined
// in the user's local day.
func (q *QuietHours) Active(at time.Time, loc *time.Location) bool {
	if q == nil {
		return false
	}
	if loc == nil {
		loc = time.UTC
	}
	local := at.In(loc)
	minute := local.Hour()*60 + local.Minute()

	if q.wraps() {
		// The day test applies to the day the window began, which for the
		// after-midnight half is yesterday.
		if minute >= q.StartMinute {
			return q.dayAllowed(local.Weekday())
		}
		if minute < q.EndMinute {
			return q.dayAllowed(local.AddDate(0, 0, -1).Weekday())
		}
		return false
	}

	if minute < q.StartMinute || minute >= q.EndMinute {
		return false
	}
	return q.dayAllowed(local.Weekday())
}

// wraps reports whether the window crosses midnight.
func (q *QuietHours) wraps() bool { return q.StartMinute > q.EndMinute }

// dayAllowed reports whether the window may run on the given day.
func (q *QuietHours) dayAllowed(d time.Weekday) bool {
	if len(q.Days) == 0 {
		return true
	}
	for _, allowed := range q.Days {
		if allowed == d {
			return true
		}
	}
	return false
}

// NextEnd returns the instant the current quiet window ends, or at unchanged when
// the window is not active. It is the value a deferred delivery's NextAttemptAt
// is set to.
func (q *QuietHours) NextEnd(at time.Time, loc *time.Location) time.Time {
	if q == nil || !q.Active(at, loc) {
		return at
	}
	if loc == nil {
		loc = time.UTC
	}
	local := at.In(loc)

	// The end is later today if the window does not wrap, or if it wraps and the
	// current time is still before midnight.
	dayOffset := 0
	if q.wraps() && local.Hour()*60+local.Minute() >= q.StartMinute {
		dayOffset = 1
	}

	end := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc).
		AddDate(0, 0, dayOffset).
		Add(time.Duration(q.EndMinute) * time.Minute)

	// A window restricted to particular days can compute an end that falls on a
	// day the window never runs, which would return an instant in the past and
	// cause a tight deferral loop. Advancing a day at a time bounds the search at
	// the most a day-restricted window can require.
	for i := 0; i < 8 && !end.After(at); i++ {
		end = end.AddDate(0, 0, 1)
	}
	return end
}

// ShouldExempt reports whether a delivery bypasses quiet hours. Any one of the
// three exemption lists is sufficient.
func (q *QuietHours) ShouldExempt(d *message.Delivery, bizType string) bool {
	if q == nil || d == nil {
		return false
	}
	if q.ExemptChannels.Has(d.Channel) {
		return true
	}
	for _, p := range q.ExemptPriorities {
		if p == d.Priority {
			return true
		}
	}
	for _, b := range q.ExemptBizTypes {
		if b == bizType {
			return true
		}
	}
	return false
}

// CapAction is what happens when a cap is reached.
type CapAction string

const (
	// CapDefer postpones the message until the cap window frees capacity.
	// Appropriate for transactional traffic.
	CapDefer CapAction = "defer"

	// CapDrop discards the message. Appropriate for marketing traffic, where a
	// late arrival is worse than none. Recorded as DeliverySkipped, never as a
	// failure.
	CapDrop CapAction = "drop"
)

var capActions = [...]CapAction{CapDefer, CapDrop}

// AllCapActions returns every cap action.
func AllCapActions() []CapAction {
	out := make([]CapAction, len(capActions))
	copy(out, capActions[:])
	return out
}

// Valid reports whether a is a recognised action.
func (a CapAction) Valid() bool {
	for _, v := range capActions {
		if a == v {
			return true
		}
	}
	return false
}

func (a CapAction) String() string { return string(a) }

// CapScope is what a cap counts over.
type CapScope string

const (
	// CapScopeAll counts every message to the user.
	CapScopeAll CapScope = "all"

	// CapScopeChannel counts messages on one channel, which is what protects a
	// per-message budget.
	CapScopeChannel CapScope = "channel"

	// CapScopeBizType counts one event type across all channels.
	CapScopeBizType CapScope = "biz_type"

	// CapScopeChannelBizType counts one event type on one channel.
	CapScopeChannelBizType CapScope = "channel_biz_type"
)

var capScopes = [...]CapScope{CapScopeAll, CapScopeChannel, CapScopeBizType, CapScopeChannelBizType}

// AllCapScopes returns every cap scope.
func AllCapScopes() []CapScope {
	out := make([]CapScope, len(capScopes))
	copy(out, capScopes[:])
	return out
}

// Valid reports whether s is a recognised scope.
func (s CapScope) Valid() bool {
	for _, v := range capScopes {
		if s == v {
			return true
		}
	}
	return false
}

func (s CapScope) String() string { return string(s) }

// CapRule limits how many messages a user may receive in a window.
type CapRule struct {
	// Name identifies the rule in metrics and in the skip reason shown to a
	// caller. It should be stable, since it appears in dashboards.
	Name string `json:"name"`

	// Scope is what the rule counts over.
	Scope CapScope `json:"scope"`

	// Window is the length of the counting window, measured back from now rather
	// than aligned to a calendar boundary. A fixed window would permit a double
	// allowance across its boundary, which is the burst a cap exists to prevent.
	Window time.Duration `json:"window"`

	// Max is the number of messages permitted in the window.
	Max int `json:"max"`

	// Action is what happens when the cap is reached.
	Action CapAction `json:"action"`

	// DeferBy is how long to postpone when Action is CapDefer. Zero means wait
	// until the oldest counted message leaves the window, as reported by the
	// counter.
	DeferBy time.Duration `json:"defer_by,omitempty"`
}

// Validate checks the rule.
func (c *CapRule) Validate() error {
	if c.Name == "" {
		return mcerr.ErrInvalidArgument.WithMessage("cap rule requires a name")
	}
	if !c.Scope.Valid() {
		return mcerr.ErrInvalidArgument.WithMessage("cap rule %q has unknown scope %q", c.Name, c.Scope)
	}
	if c.Window <= 0 {
		return mcerr.ErrInvalidArgument.WithMessage("cap rule %q needs a positive window", c.Name)
	}
	if c.Max < 0 {
		return mcerr.ErrInvalidArgument.WithMessage("cap rule %q has a negative max", c.Name)
	}
	if !c.Action.Valid() {
		return mcerr.ErrInvalidArgument.WithMessage("cap rule %q has unknown action %q", c.Name, c.Action)
	}
	if c.DeferBy < 0 {
		return mcerr.ErrInvalidArgument.WithMessage("cap rule %q has a negative defer duration", c.Name)
	}
	return nil
}

// CounterKey returns the key this rule counts under for a delivery. The key
// encodes the scope and includes the rule name, so that two rules with different
// windows do not share one counter.
func (c *CapRule) CounterKey(tenantID, userID string, d *message.Delivery, bizType string) string {
	var b strings.Builder
	b.WriteString("cap:")
	b.WriteString(c.Name)
	b.WriteString(":")
	b.WriteString(tenantID)
	b.WriteString(":")
	b.WriteString(userID)
	switch c.Scope {
	case CapScopeChannel:
		b.WriteString(":ch=")
		b.WriteString(string(d.Channel))
	case CapScopeBizType:
		b.WriteString(":biz=")
		b.WriteString(bizType)
	case CapScopeChannelBizType:
		b.WriteString(":ch=")
		b.WriteString(string(d.Channel))
		b.WriteString(":biz=")
		b.WriteString(bizType)
	case CapScopeAll:
		// No discriminator: this scope counts everything into one bucket.
	}
	return b.String()
}

// Counter is the shared counter a frequency cap is enforced against.
//
// The implementation must be shared across replicas. An in-process counter
// limits one process, so with N replicas the effective cap is N times the
// configured value.
//
// The interface deliberately offers no separate read and write: a caller that
// read a count, decided it was within the limit, and then incremented would race
// another worker doing the same, and both would permit a message.
type Counter interface {
	// IncrIfBelow increments the counter for key, and reports whether the new
	// value is within max along with the value after the increment.
	//
	// It must increment even when the answer is no. A counter that stops
	// incrementing at the limit cannot report how far over the limit the traffic
	// went, and every rule built on it is blind past the ceiling.
	//
	// retryAfter is how long until the oldest event leaves the window, and is
	// meaningful when allowed is false. Zero means the caller falls back to the
	// rule's DeferBy.
	IncrIfBelow(ctx context.Context, key string, window time.Duration, max int, now time.Time) (count int, allowed bool, retryAfter time.Duration, err error)

	// Peek returns the current count without incrementing, for a settings screen.
	// Never use it for enforcement.
	Peek(ctx context.Context, key string, window time.Duration, now time.Time) (int, error)
}

// Outcome is what the evaluator decided.
type Outcome string

const (
	// OutcomeAllow means send it now.
	OutcomeAllow Outcome = "allow"

	// OutcomeDefer means send it later, at Decision.DeferUntil. The delivery
	// returns to pending without consuming an attempt.
	OutcomeDefer Outcome = "defer"

	// OutcomeDrop means never send it. The delivery becomes DeliverySkipped, not
	// a failure.
	OutcomeDrop Outcome = "drop"
)

var outcomes = [...]Outcome{OutcomeAllow, OutcomeDefer, OutcomeDrop}

// AllOutcomes returns every outcome.
func AllOutcomes() []Outcome {
	out := make([]Outcome, len(outcomes))
	copy(out, outcomes[:])
	return out
}

// Valid reports whether o is a recognised outcome.
func (o Outcome) Valid() bool {
	for _, v := range outcomes {
		if o == v {
			return true
		}
	}
	return false
}

func (o Outcome) String() string { return string(o) }

// Reason explains a decision. It is a closed enum because these values are
// aggregated: "how many messages did quiet hours suppress" cannot be answered
// over free-form text.
type Reason string

const (
	// ReasonAllowed means nothing blocked it.
	ReasonAllowed Reason = "allowed"

	// ReasonChannelOptOut means the user switched the channel off.
	ReasonChannelOptOut Reason = "channel_opt_out"

	// ReasonBizTypeOptOut means the user switched this event type off on this
	// channel.
	ReasonBizTypeOptOut Reason = "biz_type_opt_out"

	// ReasonQuietHours means it is inside the user's do-not-disturb window.
	ReasonQuietHours Reason = "quiet_hours"

	// ReasonFrequencyCap means the user has had their allowance of this kind of
	// message in the current window.
	ReasonFrequencyCap Reason = "frequency_cap"

	// ReasonAddressSuppressed means the address has been marked undeliverable.
	ReasonAddressSuppressed Reason = "address_suppressed"

	// ReasonExpired means the deferral would have pushed the delivery past its
	// ExpiresAt. It is distinct from the reason that caused the deferral because
	// the operator response differs: an expiry kills a message that a window or
	// cap would merely have delayed.
	ReasonExpired Reason = "expired_during_deferral"
)

var reasons = [...]Reason{
	ReasonAllowed,
	ReasonChannelOptOut,
	ReasonBizTypeOptOut,
	ReasonQuietHours,
	ReasonFrequencyCap,
	ReasonAddressSuppressed,
	ReasonExpired,
}

// AllReasons returns every reason.
func AllReasons() []Reason {
	out := make([]Reason, len(reasons))
	copy(out, reasons[:])
	return out
}

// Valid reports whether r is a recognised reason.
func (r Reason) Valid() bool {
	for _, v := range reasons {
		if v == r {
			return true
		}
	}
	return false
}

func (r Reason) String() string { return string(r) }

// Decision is the evaluator's answer.
type Decision struct {
	// Outcome is what to do.
	Outcome Outcome `json:"outcome"`

	// Reason explains it, for metrics and for the skip record.
	Reason Reason `json:"reason"`

	// DeferUntil is when to try again. Set when Outcome is OutcomeDefer.
	DeferUntil time.Time `json:"defer_until,omitempty"`

	// Rule names the cap rule that fired, when the reason is
	// ReasonFrequencyCap.
	Rule string `json:"rule,omitempty"`

	// Detail is a human-readable elaboration for logs. Must not contain PII.
	Detail string `json:"detail,omitempty"`
}

// Allowed reports whether the decision permits sending.
func (d Decision) Allowed() bool { return d.Outcome == OutcomeAllow }

// Deferred reports whether the decision postpones sending.
func (d Decision) Deferred() bool { return d.Outcome == OutcomeDefer }

// Dropped reports whether the decision discards the message.
func (d Decision) Dropped() bool { return d.Outcome == OutcomeDrop }

// Allow builds an allow decision.
func Allow() Decision {
	return Decision{Outcome: OutcomeAllow, Reason: ReasonAllowed}
}

// Defer builds a deferral.
func Defer(until time.Time, reason Reason, format string, args ...any) Decision {
	return Decision{
		Outcome:    OutcomeDefer,
		Reason:     reason,
		DeferUntil: until,
		Detail:     fmt.Sprintf(format, args...),
	}
}

// Drop builds a discard.
func Drop(reason Reason, format string, args ...any) Decision {
	return Decision{
		Outcome: OutcomeDrop,
		Reason:  reason,
		Detail:  fmt.Sprintf(format, args...),
	}
}

// Validate checks the decision is internally consistent.
//
// A Defer with no DeferUntil would return a delivery to pending with a zero
// NextAttemptAt, which every claim query selects immediately.
func (d Decision) Validate() error {
	if !d.Outcome.Valid() {
		return mcerr.ErrInternal.WithMessage("decision has unknown outcome %q", d.Outcome)
	}
	if !d.Reason.Valid() {
		return mcerr.ErrInternal.WithMessage("decision has unknown reason %q", d.Reason)
	}
	if d.Outcome == OutcomeDefer && d.DeferUntil.IsZero() {
		return mcerr.ErrInternal.WithMessage("deferral decision %q has no defer time", d.Reason)
	}
	return nil
}

// Phase distinguishes the accept-time check from the claim-time check.
type Phase string

const (
	// PhaseAccept evaluates the hard preferences, which cannot change between
	// accept and send.
	PhaseAccept Phase = "accept"

	// PhaseClaim evaluates the temporal preferences, which depend on when the
	// message would actually go out.
	PhaseClaim Phase = "claim"
)

var phases = [...]Phase{PhaseAccept, PhaseClaim}

// AllPhases returns every phase.
func AllPhases() []Phase {
	out := make([]Phase, len(phases))
	copy(out, phases[:])
	return out
}

// Valid reports whether p is a recognised phase.
func (p Phase) Valid() bool {
	for _, v := range phases {
		if p == v {
			return true
		}
	}
	return false
}

func (p Phase) String() string { return string(p) }

// Request is what the evaluator is asked about.
type Request struct {
	// Delivery is the delivery being evaluated. Required.
	Delivery *message.Delivery

	// Message is the parent message, when available.
	Message *message.Message

	// BizType is the caller's event type, used for opt-out and exemption
	// matching.
	BizType string

	// Preference is the user's settings. Nil means no settings, treated as
	// permitting everything.
	Preference *Preference

	// Now is the reference instant. Required.
	Now time.Time

	// Phase is which evaluation is being performed.
	Phase Phase
}

// Validate checks the request.
func (r Request) Validate() error {
	if r.Delivery == nil {
		return mcerr.ErrInvalidArgument.WithMessage("evaluation request has no delivery")
	}
	if r.Now.IsZero() {
		return mcerr.ErrInvalidArgument.WithMessage("evaluation request has no reference time")
	}
	if !r.Phase.Valid() {
		return mcerr.ErrInvalidArgument.WithMessage("evaluation request has unknown phase %q", r.Phase)
	}
	return nil
}

// Evaluator decides whether a delivery may be sent.
//
// Implementations must be safe for concurrent use: the claim path calls Evaluate
// from every worker goroutine. Evaluate must not write to the store, with the
// single exception of incrementing a frequency-cap counter.
type Evaluator interface {
	Evaluate(ctx context.Context, req Request) (Decision, error)
}

// StaticEvaluator applies fixed preferences and cap rules to every request.
type StaticEvaluator struct {
	// Prefs is consulted for every request, regardless of the user.
	Prefs *Preference

	// Caps is the rule set, evaluated after Prefs.
	Caps []CapRule

	// Counter enforces the caps. A nil Counter with a non-empty rule set is
	// rejected by NewStaticEvaluator.
	Counter Counter

	// Now supplies the clock, for tests. Nil means the request's own time is
	// used.
	Now func() time.Time
}

// Evaluate implements Evaluator.
func (e *StaticEvaluator) Evaluate(ctx context.Context, req Request) (Decision, error) {
	if err := req.Validate(); err != nil {
		return Decision{}, err
	}
	prefs := e.Prefs
	if prefs == nil {
		prefs = req.Preference
	}

	if d, blocked := evaluateHardPreferences(prefs, req); blocked {
		return d, nil
	}
	if req.Phase == PhaseAccept {
		// Temporal preferences are not evaluated on the accept path: the message
		// has not been scheduled against a clock yet, and a deferral has nowhere
		// to be stored.
		return Allow(), nil
	}
	return e.evaluateTemporal(ctx, prefs, req)
}

// evaluateTemporal applies quiet hours and frequency caps.
func (e *StaticEvaluator) evaluateTemporal(ctx context.Context, prefs *Preference, req Request) (Decision, error) {
	now := req.Now
	if e.Now != nil {
		now = e.Now()
	}

	if q := prefs.QuietHours; q != nil && !q.ShouldExempt(req.Delivery, req.BizType) {
		loc := prefs.Location()
		if q.Active(now, loc) {
			until := q.NextEnd(now, loc)
			return deferOrExpire(req.Delivery, until, ReasonQuietHours,
				"inside quiet hours ending at %s", until.Format(time.RFC3339)), nil
		}
	}

	if e.Counter == nil {
		return Allow(), nil
	}
	for i := range e.Caps {
		rule := &e.Caps[i]
		key := rule.CounterKey(req.Delivery.TenantID, req.Delivery.Recipient.UserID, req.Delivery, req.BizType)
		count, allowed, retryAfter, err := e.Counter.IncrIfBelow(ctx, key, rule.Window, rule.Max, now)
		if err != nil {
			// An unreachable counter is most likely an unreachable dependency, and
			// the correct response is to retry rather than to bypass the cap.
			return Decision{}, mcerr.From(err).WithMessage("frequency cap %q could not be evaluated", rule.Name)
		}
		if allowed {
			continue
		}
		if rule.Action == CapDrop {
			return Decision{
				Outcome: OutcomeDrop,
				Reason:  ReasonFrequencyCap,
				Rule:    rule.Name,
				Detail:  fmt.Sprintf("cap %q reached (%d in %s)", rule.Name, count, rule.Window),
			}, nil
		}
		until := now.Add(rule.DeferBy)
		if rule.DeferBy <= 0 && retryAfter > 0 {
			until = now.Add(retryAfter)
		}
		if rule.DeferBy <= 0 && retryAfter <= 0 {
			until = now.Add(rule.Window)
		}
		d := deferOrExpire(req.Delivery, until, ReasonFrequencyCap,
			"cap %q reached (%d in %s)", rule.Name, count, rule.Window)
		d.Rule = rule.Name
		return d, nil
	}
	return Allow(), nil
}

// evaluateHardPreferences applies the accept-time checks.
func evaluateHardPreferences(prefs *Preference, req Request) (Decision, bool) {
	if prefs == nil {
		return Decision{}, false
	}
	ch := req.Delivery.Channel
	if prefs.IsChannelOptedOut(ch) {
		return Drop(ReasonChannelOptOut, "user has opted out of %q", ch), true
	}
	if prefs.IsBizTypeOptedOut(ch, req.BizType) {
		return Drop(ReasonBizTypeOptOut, "user has opted out of %q on %q", req.BizType, ch), true
	}
	if req.Delivery.Address.Value != "" && prefs.IsAddressSuppressed(req.Delivery.Address) {
		// The masked form: this detail is logged and recorded as a skip reason.
		return Drop(ReasonAddressSuppressed, "address %s is suppressed", req.Delivery.Address.Masked()), true
	}
	return Decision{}, false
}

// deferOrExpire converts a deferral that would outlive the delivery into a drop,
// so that a short expiry and a long window produce one decision rather than an
// endless sequence of deferrals each followed by an expiry check.
func deferOrExpire(d *message.Delivery, until time.Time, reason Reason, format string, args ...any) Decision {
	if !d.ExpiresAt.IsZero() && !until.Before(d.ExpiresAt) {
		return Drop(ReasonExpired,
			"%s would defer past expiry at %s", reason, d.ExpiresAt.Format(time.RFC3339))
	}
	return Defer(until, reason, format, args...)
}

// NewStaticEvaluator returns an evaluator, rejecting a rule set that would not be
// enforced.
func NewStaticEvaluator(prefs *Preference, rules []CapRule, counter Counter) (*StaticEvaluator, error) {
	for i := range rules {
		if err := rules[i].Validate(); err != nil {
			return nil, err
		}
	}
	if len(rules) > 0 && counter == nil {
		return nil, mcerr.ErrInvalidArgument.
			WithMessage("%d frequency cap rules are configured but no counter is supplied, so none would be enforced", len(rules))
	}
	if prefs != nil {
		if err := prefs.Validate(); err != nil {
			return nil, err
		}
	}
	return &StaticEvaluator{Prefs: prefs, Caps: rules, Counter: counter}, nil
}

var _ Evaluator = (*StaticEvaluator)(nil)

// Store persists preferences.
//
// It is separate from the message-center Store because preferences are the part
// of this system a deployment is most likely to already have: an existing user
// table with a notification-settings column is the normal case.
type Store interface {
	// Get returns a user's preferences.
	//
	// It must return an empty Preference and a nil error when the user has none,
	// not mcerr.ErrNotFound. The common case is a user who has never opened the
	// settings screen.
	Get(ctx context.Context, tenantID, userID string) (*Preference, error)

	// Put writes preferences.
	Put(ctx context.Context, p *Preference) error

	// SuppressAddress records an address as undeliverable, for a hard bounce.
	//
	// It is a method of its own rather than a read-modify-write through Get and
	// Put because the bounce webhook path calls it concurrently for the same user,
	// and a read-modify-write would lose all but one of the suppressions.
	SuppressAddress(ctx context.Context, tenantID, userID string, a message.Address, at time.Time) error
}
