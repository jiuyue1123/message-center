package message

import (
	"fmt"
	"strings"
	"time"

	"github.com/jiuyue1123/message-center/pkg/mcerr"
)

// Address is one way to reach a recipient.
//
// Value is unvalidated at this layer: whether a given string is a usable phone
// number depends on the channel and the provider. Only the form is checked here;
// content rules belong to the Sender.
type Address struct {
	// Form identifies what kind of address Value is.
	Form AddressForm `json:"form"`

	// Value is the address itself. Every form except user_id and webhook_url is
	// PII and must be masked before being logged or returned in a response.
	Value string `json:"value"`

	// Label is a human-readable hint such as "work" or "iPhone 15". It carries no
	// routing meaning.
	Label string `json:"label,omitempty"`

	// Verified records that the address has been confirmed to belong to the
	// recipient. Never set by this package.
	Verified bool `json:"verified,omitempty"`
}

// NewAddress builds an address, trimming incidental whitespace from the value.
func NewAddress(form AddressForm, value string) Address {
	return Address{Form: form, Value: strings.TrimSpace(value)}
}

// Validate reports whether the address is structurally usable: a recognised form
// with a non-empty value. Channel-specific rules are a Sender's responsibility.
func (a Address) Validate() error {
	if !a.Form.Valid() {
		return mcerr.ErrInvalidArgument.WithMessage("unknown address form %q", a.Form)
	}
	if a.Value == "" {
		return mcerr.ErrInvalidArgument.WithMessage("address of form %q has an empty value", a.Form)
	}
	return nil
}

// Masked returns the address with its identifying parts obscured, for logging
// and for inclusion in API responses. Forms that are not personally identifiable
// are returned unchanged.
func (a Address) Masked() string {
	if !a.Form.IsPersonallyIdentifiable() {
		return a.Value
	}
	switch a.Form {
	case FormEmail:
		at := strings.LastIndex(a.Value, "@")
		if at <= 0 {
			return maskMiddle(a.Value, 1, 0)
		}
		return maskMiddle(a.Value[:at], 1, 0) + a.Value[at:]
	case FormPhone:
		if len(a.Value) <= 7 {
			return maskMiddle(a.Value, 0, 0)
		}
		return a.Value[:4] + "****" + a.Value[len(a.Value)-4:]
	default:
		return maskMiddle(a.Value, 4, 4)
	}
}

// maskMiddle reveals head and tail bytes, replacing the rest with a fixed number
// of asterisks. The count is constant so that the mask does not leak the hidden
// length.
func maskMiddle(s string, head, tail int) string {
	if head+tail >= len(s) {
		return strings.Repeat("*", 4)
	}
	return s[:head] + "****" + s[len(s)-tail:]
}

func (a Address) String() string { return a.Masked() }

// Recipient is who a message goes to, and every way they can be reached.
//
// A recipient may hold several addresses of the same form. For each channel the
// dispatcher selects the first address whose form appears in that channel's
// Sender.Accepts list.
type Recipient struct {
	// UserID is the recipient's identity in the message center's own space. It
	// is required even when every delivery goes to an external address: it keys
	// the in-app inbox, opt-out preferences, and erasure requests.
	UserID string `json:"user_id"`

	// Addresses are every known way to reach this user. May be empty for a
	// purely in-app message.
	Addresses []Address `json:"addresses,omitempty"`

	// DisplayName is used for template personalisation, never for routing.
	DisplayName string `json:"display_name,omitempty"`

	// Locale is a BCP 47 tag used to select a template variant. Empty means the
	// deployment default.
	Locale string `json:"locale,omitempty"`

	// Timezone is an IANA zone name, used for quiet hours and for formatting
	// times inside templates. Empty means UTC. It is separate from Locale
	// because a user's language and their local time are independent.
	Timezone string `json:"timezone,omitempty"`

	// Attributes are per-recipient template variables. Values are strings so
	// that the set is trivially serialisable and diffable.
	Attributes map[string]string `json:"attributes,omitempty"`
}

// AddressesOf returns every address of the given form, in the order supplied.
func (r Recipient) AddressesOf(form AddressForm) []Address {
	var out []Address
	for _, a := range r.Addresses {
		if a.Form == form {
			out = append(out, a)
		}
	}
	return out
}

// FirstOf returns the first address of the given form.
func (r Recipient) FirstOf(form AddressForm) (Address, bool) {
	for _, a := range r.Addresses {
		if a.Form == form {
			return a, true
		}
	}
	return Address{}, false
}

// FirstAccepting returns the first address whose form appears in forms.
//
// It iterates forms rather than addresses so that the Sender's stated preference
// order wins over the order the addresses happen to be listed in.
func (r Recipient) FirstAccepting(forms []AddressForm) (Address, bool) {
	for _, f := range forms {
		if a, ok := r.FirstOf(f); ok && a.Value != "" {
			return a, true
		}
	}
	return Address{}, false
}

// Validate reports whether the recipient is usable. Individual addresses are
// checked structurally only.
func (r Recipient) Validate() error {
	if strings.TrimSpace(r.UserID) == "" {
		return mcerr.ErrInvalidArgument.WithMessage("recipient user_id is required")
	}
	for i, a := range r.Addresses {
		if err := a.Validate(); err != nil {
			return mcerr.From(err).WithMessage("address %d: %s", i, mcerr.From(err).Message)
		}
	}
	return nil
}

// Attribute returns a template variable, falling back to the built-in values the
// message center always provides.
func (r Recipient) Attribute(name string) (string, bool) {
	switch name {
	case "user_id":
		return r.UserID, true
	case "display_name", "name":
		return r.DisplayName, r.DisplayName != ""
	case "locale":
		return r.Locale, r.Locale != ""
	case "timezone":
		return r.Timezone, r.Timezone != ""
	}
	v, ok := r.Attributes[name]
	return v, ok
}

// Content is the rendered, channel-ready body of a message.
//
// Template resolution and variable substitution have already happened by the
// time a Content exists. A Delivery stores a snapshot of its Content and retries
// resend that snapshot rather than re-rendering, so that a template edited
// between attempts cannot change what the recipient receives.
type Content struct {
	// Subject is the title, subject line, or push notification title.
	Subject string `json:"subject,omitempty"`

	// Body is the message text.
	Body string `json:"body"`

	// BodyFormat tells a Sender how to interpret Body.
	BodyFormat BodyFormat `json:"body_format,omitempty"`

	// Data is a structured payload for channels that carry one: the APNs/FCM
	// custom data dictionary, or a webhook request body.
	Data map[string]string `json:"data,omitempty"`

	// URL is the deep link or landing page to open when the recipient acts on
	// the message.
	URL string `json:"url,omitempty"`

	// ImageURL is an optional accompanying image.
	ImageURL string `json:"image_url,omitempty"`
}

// BodyFormat describes how to interpret Content.Body.
type BodyFormat string

const (
	// FormatText is plain text, supported by every channel.
	FormatText BodyFormat = "text"

	// FormatHTML is HTML. Supported by email and in-app.
	FormatHTML BodyFormat = "html"

	// FormatMarkdown is Markdown, rendered by the channel or the client.
	FormatMarkdown BodyFormat = "markdown"
)

var bodyFormats = [...]BodyFormat{FormatText, FormatHTML, FormatMarkdown}

// AllBodyFormats returns every body format.
func AllBodyFormats() []BodyFormat {
	out := make([]BodyFormat, len(bodyFormats))
	copy(out, bodyFormats[:])
	return out
}

// Valid reports whether f is a recognised body format.
func (f BodyFormat) Valid() bool {
	for _, v := range bodyFormats {
		if f == v {
			return true
		}
	}
	return false
}

func (f BodyFormat) String() string { return string(f) }

// Normalize replaces the empty format with FormatText.
func (f BodyFormat) Normalize() BodyFormat {
	if f == "" {
		return FormatText
	}
	return f
}

// IsEmpty reports whether the content carries nothing to send.
func (c Content) IsEmpty() bool {
	return strings.TrimSpace(c.Subject) == "" &&
		strings.TrimSpace(c.Body) == "" &&
		len(c.Data) == 0
}

// Size returns the byte length of the content, for comparison against a
// channel's Capabilities.MaxBodyBytes.
func (c Content) Size() int {
	n := len(c.Subject) + len(c.Body)
	for k, v := range c.Data {
		n += len(k) + len(v)
	}
	return n
}

// Merge returns a copy of c with overlay's non-empty fields applied. Nil and
// empty maps behave identically.
func (c Content) Merge(overlay Content) Content {
	out := c
	if overlay.Subject != "" {
		out.Subject = overlay.Subject
	}
	if overlay.Body != "" {
		out.Body = overlay.Body
	}
	if overlay.BodyFormat != "" {
		out.BodyFormat = overlay.BodyFormat
	}
	if overlay.URL != "" {
		out.URL = overlay.URL
	}
	if overlay.ImageURL != "" {
		out.ImageURL = overlay.ImageURL
	}
	if len(overlay.Data) > 0 {
		out.Data = make(map[string]string, len(c.Data)+len(overlay.Data))
		for k, v := range c.Data {
			out.Data[k] = v
		}
		for k, v := range overlay.Data {
			out.Data[k] = v
		}
	}
	return out
}

// Message is an envelope: one caller intent, fanned out to one or more channels.
//
// Everything that can fail, retry, or vary per transport lives on the Deliveries
// it owns, not here.
type Message struct {
	// ID is the message identifier, and the handle returned to the caller.
	ID ID `json:"id"`

	// TenantID scopes the message. Empty means the deployment default. Present
	// from the first version because adding a tenant discriminator to live
	// composite indexes later requires a table rebuild.
	TenantID string `json:"tenant_id,omitempty"`

	// IdempotencyKey deduplicates submissions. Scoped per tenant.
	IdempotencyKey string `json:"idempotency_key,omitempty"`

	// BizType is the caller's own event type, such as "order_shipped". It is the
	// join key between this system and the caller's, used for template
	// selection, opt-out rules, and per-event metrics. Never interpreted here.
	BizType string `json:"biz_type"`

	// Channels is the set of channels to fan out to.
	Channels ChannelSet `json:"channels"`

	// Recipient is who the message goes to.
	Recipient Recipient `json:"recipient"`

	// Content is the default body, applied to every channel that does not
	// override it.
	Content Content `json:"content"`

	// ChannelContent overrides Content per channel.
	ChannelContent map[Channel]Content `json:"channel_content,omitempty"`

	// Priority orders queue service. It does not affect retry timing.
	Priority Priority `json:"priority,omitempty"`

	// ScheduledAt defers fan-out until the given time. Zero means now. A
	// scheduled message has no Delivery rows until it comes due, so that
	// addresses are resolved against current state rather than state captured at
	// submit time.
	ScheduledAt time.Time `json:"scheduled_at,omitempty"`

	// ExpiresAt bounds a delivery's useful life. Zero means the dispatcher
	// applies its default.
	ExpiresAt time.Time `json:"expires_at,omitempty"`

	// MaxAttempts caps per-delivery retries. Zero means the dispatcher's
	// default. Per-delivery rather than per-message so that one channel's
	// exhausted budget does not consume another's.
	MaxAttempts int `json:"max_attempts,omitempty"`

	// Metadata is caller-supplied data passed through to Senders. Distinct from
	// Content.Data, which the recipient's client sees; Metadata is never
	// rendered into a message body.
	Metadata map[string]string `json:"metadata,omitempty"`

	// Status is the derived lifecycle state. See DeriveStatus.
	Status MessageStatus `json:"status"`

	// CreatedAt is when the message was accepted.
	CreatedAt time.Time `json:"created_at"`

	// UpdatedAt is when the status last changed.
	UpdatedAt time.Time `json:"updated_at"`

	// CompletedAt is when the message reached a terminal status. Zero while
	// non-terminal.
	CompletedAt time.Time `json:"completed_at,omitempty"`
}

// ContentFor returns the content to send on the given channel: the channel
// override merged over the default.
func (m *Message) ContentFor(ch Channel) Content {
	base := m.Content
	if override, ok := m.ChannelContent[ch]; ok {
		return base.Merge(override)
	}
	return base
}

// IsScheduled reports whether the message is waiting for its scheduled time.
func (m *Message) IsScheduled() bool {
	return !m.ScheduledAt.IsZero() && m.ScheduledAt.After(m.CreatedAt)
}

// IsTerminal reports whether the message will never change again.
func (m *Message) IsTerminal() bool { return m.Status.IsTerminal() }

// Validate checks the message's own structure. Content is not checked against
// any channel's capabilities; that requires asking each Sender and happens
// during fan-out.
func (m *Message) Validate() error {
	if m.BizType == "" {
		return mcerr.ErrInvalidArgument.WithMessage("biz_type is required")
	}
	if m.Channels.IsEmpty() {
		return mcerr.ErrInvalidArgument.WithMessage("at least one channel is required")
	}
	if err := m.Recipient.Validate(); err != nil {
		return err
	}
	if m.Content.IsEmpty() {
		return mcerr.ErrInvalidArgument.WithMessage("content is empty: nothing to send")
	}
	if !m.Priority.Valid() && m.Priority != "" {
		return mcerr.ErrInvalidArgument.WithMessage("unknown priority %q", m.Priority)
	}
	if !m.ExpiresAt.IsZero() && !m.ScheduledAt.IsZero() && m.ExpiresAt.Before(m.ScheduledAt) {
		return mcerr.ErrInvalidArgument.WithMessage("expires_at is before scheduled_at")
	}
	if m.MaxAttempts < 0 {
		return mcerr.ErrInvalidArgument.WithMessage("max_attempts cannot be negative")
	}
	return nil
}

// DeriveStatus computes the message status implied by the given delivery
// statuses, so that the two cannot disagree.
//
// It returns StatusPending while any delivery is still in progress. Partial
// success is declared only once every delivery has settled. Receipts are not
// considered: DeliveryAccepted counts as success here, and a later bounce is a
// milestone fact rather than a send outcome.
func DeriveStatus(deliveries []DeliveryStatus) MessageStatus {
	if len(deliveries) == 0 {
		return StatusPending
	}
	var accepted, failed, canceled, skipped, working int
	for _, d := range deliveries {
		switch d {
		case DeliveryAccepted:
			accepted++
		case DeliveryFailed, DeliveryExpired:
			failed++
		case DeliveryCanceled:
			canceled++
		case DeliverySkipped:
			skipped++
		default:
			working++
		}
	}
	if working > 0 {
		return StatusPending
	}
	switch {
	case accepted == 0 && failed == 0:
		// Every delivery was skipped or canceled. An opt-out is not a failure.
		return StatusCanceled
	case failed == 0 && canceled == 0 && skipped == 0:
		return StatusSucceeded
	case accepted == 0:
		return StatusFailed
	default:
		return StatusPartiallySucceeded
	}
}

// Delivery is one channel's share of one message, and the unit of retry.
//
// When its channel is ChannelInApp it is also the inbox row: the in-app channel
// has no external transport, and a separate row would create a second source of
// truth for an unread count.
type Delivery struct {
	// ID identifies the delivery; also the inbox item ID for in-app.
	ID ID `json:"id"`

	// MessageID is the parent message.
	MessageID ID `json:"message_id"`

	// TenantID is denormalised from the message so that the claim query needs no
	// join.
	TenantID string `json:"tenant_id,omitempty"`

	// Channel is the transport this delivery uses.
	Channel Channel `json:"channel"`

	// Recipient is the recipient as resolved at fan-out, frozen so that a retry
	// cannot be redirected to an address that changed in the meantime.
	Recipient Recipient `json:"recipient"`

	// Address is the single address chosen for this channel. Empty when the
	// channel addresses by identity alone, as in-app does.
	Address Address `json:"address,omitempty"`

	// Content is the rendered snapshot sent on this channel.
	Content Content `json:"content"`

	// Priority is copied from the message so that the claim query can order
	// without joining.
	Priority Priority `json:"priority,omitempty"`

	// Status is the delivery's lifecycle state.
	Status DeliveryStatus `json:"status"`

	// AttemptCount is how many times a provider was actually called. Incremented
	// only by CommitAttempt, never by a lease release.
	AttemptCount int `json:"attempt_count"`

	// MaxAttempts caps AttemptCount. Copied from the message at fan-out and
	// resolved to a concrete value.
	MaxAttempts int `json:"max_attempts"`

	// NextAttemptAt is when the delivery becomes claimable. Terminal rows carry a
	// far-future sentinel rather than zero so that the claim index stays a tight
	// range scan.
	NextAttemptAt time.Time `json:"next_attempt_at"`

	// LeaseOwner identifies the worker holding this delivery. Empty when
	// unclaimed.
	LeaseOwner string `json:"lease_owner,omitempty"`

	// LeaseExpiresAt bounds how long a worker may hold a claimed delivery.
	LeaseExpiresAt time.Time `json:"lease_expires_at,omitempty"`

	// ExpiresAt bounds the delivery's useful life. Zero means no expiry.
	ExpiresAt time.Time `json:"expires_at,omitempty"`

	// Failure is the most recent failure, cleared on success. The full history
	// is in Attempt.
	Failure *Failure `json:"failure,omitempty"`

	// Milestone is how far the message got after the provider took it. Monotonic
	// and derived from receipts, which remain the source of truth.
	Milestone Milestone `json:"milestone,omitempty"`

	// Bounced is the bounce type, empty when the message did not bounce.
	Bounced BounceType `json:"bounced,omitempty"`

	// BouncedAt is when the bounce was reported.
	BouncedAt time.Time `json:"bounced_at,omitempty"`

	// ComplainedAt is when the recipient marked the message as spam.
	ComplainedAt time.Time `json:"complained_at,omitempty"`

	// LastReceiptAt is when any receipt last arrived, for detecting a provider
	// whose callbacks have stopped.
	LastReceiptAt time.Time `json:"last_receipt_at,omitempty"`

	// ReadAt is when the recipient read the message. For in-app it is what the
	// unread count is computed from.
	ReadAt time.Time `json:"read_at,omitempty"`

	// CreatedAt is when the delivery was fanned out.
	CreatedAt time.Time `json:"created_at"`

	// UpdatedAt is when the delivery last changed state.
	UpdatedAt time.Time `json:"updated_at"`
}

// ApplyReceipt folds a receipt into the delivery's derived fields, and reports
// whether anything changed so that a caller can skip a write for a duplicate.
//
// The milestone takes the maximum of its current value and the receipt's. Direct
// assignment would let a receipt arriving out of order move the funnel backwards.
func (d *Delivery) ApplyReceipt(r *Receipt) bool {
	if r == nil || !r.Kind.Valid() {
		return false
	}
	changed := false

	if next, ok := r.Kind.MilestoneFor(); ok {
		if advanced := Advance(d.Milestone, next); advanced != d.Milestone {
			d.Milestone = advanced
			changed = true
		}
	}

	switch r.Kind {
	case ReceiptBounced:
		bt := r.BounceType
		if bt == "" {
			bt = BounceUnknown
		}
		// A hard bounce upgrades a soft one; the reverse never applies, since a
		// downgrade would un-suppress an address that should stay suppressed.
		if d.Bounced == "" || (bt == BounceHard && d.Bounced != BounceHard) {
			d.Bounced = bt
			d.BouncedAt = r.At
			changed = true
		}
	case ReceiptComplained:
		if d.ComplainedAt.IsZero() {
			d.ComplainedAt = r.At
			changed = true
		}
	}

	if r.At.After(d.LastReceiptAt) {
		d.LastReceiptAt = r.At
		changed = true
	}
	if d.Milestone.Rank() >= MilestoneRead.Rank() && d.ReadAt.IsZero() {
		d.ReadAt = r.At
		changed = true
	}
	return changed
}

// IsInboxItem reports whether this delivery is visible in a user's in-app inbox.
func (d *Delivery) IsInboxItem() bool { return d.Channel == ChannelInApp }

// IsUnread reports whether an in-app delivery has not been read.
func (d *Delivery) IsUnread() bool { return d.IsInboxItem() && d.ReadAt.IsZero() }

// HasAttemptsLeft reports whether the delivery may be attempted again.
func (d *Delivery) HasAttemptsLeft() bool { return d.AttemptCount < d.MaxAttempts }

// IsExpired reports whether the delivery has passed its expiry at time t.
func (d *Delivery) IsExpired(t time.Time) bool {
	return !d.ExpiresAt.IsZero() && !t.Before(d.ExpiresAt)
}

// IsLeaseExpired reports whether the delivery's lease has lapsed at time t. A
// delivery that is not sending has no lease and is never lease-expired.
func (d *Delivery) IsLeaseExpired(t time.Time) bool {
	return d.Status == DeliverySending && !d.LeaseExpiresAt.IsZero() && t.After(d.LeaseExpiresAt)
}

// Validate checks the delivery's internal consistency.
func (d *Delivery) Validate() error {
	if d.MessageID.IsZero() {
		return mcerr.ErrInvalidArgument.WithMessage("delivery has no message_id")
	}
	if !d.Channel.Valid() {
		return mcerr.ErrInvalidArgument.WithMessage("delivery has unknown channel %q", d.Channel)
	}
	if d.MaxAttempts <= 0 {
		return mcerr.ErrInvalidArgument.WithMessage("delivery max_attempts must be positive")
	}
	if d.AttemptCount < 0 {
		return mcerr.ErrInvalidArgument.WithMessage("delivery attempt_count cannot be negative")
	}
	return nil
}

// Attempt is one call to a provider.
//
// The table is append-only: a row is inserted in flight and finalised once, and
// an attempt orphaned by a crash is retained rather than cleaned up.
type Attempt struct {
	// ID identifies the attempt.
	ID ID `json:"id"`

	// DeliveryID is the delivery this attempt belongs to.
	DeliveryID ID `json:"delivery_id"`

	// MessageID is denormalised so that a message's full history needs no join.
	MessageID ID `json:"message_id"`

	// Channel is denormalised for per-channel metrics.
	Channel Channel `json:"channel"`

	// AttemptNo is the 1-based sequence number within the delivery.
	AttemptNo int `json:"attempt_no"`

	// Status is the attempt outcome.
	Status AttemptStatus `json:"status"`

	// StartedAt is when the provider call began.
	StartedAt time.Time `json:"started_at"`

	// FinishedAt is when it ended. Zero while in flight.
	FinishedAt time.Time `json:"finished_at,omitempty"`

	// Duration is the recorded latency.
	Duration time.Duration `json:"duration,omitempty"`

	// Failure is why the attempt failed. Nil on success.
	Failure *Failure `json:"failure,omitempty"`

	// ProviderMessageID is the provider's identifier for an accepted message.
	ProviderMessageID string `json:"provider_message_id,omitempty"`

	// ResponseDigest is a hash of the provider's raw response. A digest rather
	// than the response itself, which routinely echoes the recipient's address
	// and the message body.
	ResponseDigest string `json:"response_digest,omitempty"`

	// ResponseSnippet is a truncated, redacted excerpt for human debugging.
	ResponseSnippet string `json:"response_snippet,omitempty"`

	// WorkerID identifies the process that made the call.
	WorkerID string `json:"worker_id,omitempty"`
}

// Failure records why something did not succeed. It is a value type so that it
// can be persisted and read back without access to the original error chain.
type Failure struct {
	// Kind is the retry classification, mirroring mcerr.Kind.
	Kind string `json:"kind"`

	// Code is the stable machine-readable failure code.
	Code string `json:"code"`

	// Message is a human-readable description. Must not contain PII.
	Message string `json:"message"`

	// Provider is the upstream provider's error code. Not for control flow.
	Provider string `json:"provider,omitempty"`

	// RetryAfter is a provider-supplied backoff hint.
	RetryAfter time.Duration `json:"retry_after,omitempty"`

	// At is when the failure was recorded.
	At time.Time `json:"at"`
}

// FailureFrom converts an error into a serialisable Failure, or nil for a nil
// error. This is the only place a Go error crosses into stored data.
func FailureFrom(err error, at time.Time) *Failure {
	if err == nil {
		return nil
	}
	e := mcerr.From(err)
	return &Failure{
		Kind:       string(e.Kind),
		Code:       e.Code,
		Message:    e.Message,
		Provider:   e.Provider,
		RetryAfter: e.RetryAfter,
		At:         at,
	}
}

// Err converts a stored Failure back into a classified error, round-tripping
// Kind and RetryAfter with FailureFrom.
func (f *Failure) Err() error {
	if f == nil {
		return nil
	}
	e := mcerr.New(mcerr.Kind(f.Kind), f.Code, "%s", f.Message)
	if f.Provider != "" {
		e = e.WithProvider(f.Provider)
	}
	if f.RetryAfter > 0 {
		e.RetryAfter = f.RetryAfter
	}
	return e
}

// SendResult is what a Sender returns after the provider accepted a message.
type SendResult struct {
	// ProviderMessageID is the provider's identifier for the accepted message.
	ProviderMessageID string `json:"provider_message_id,omitempty"`

	// AcceptedAt is when the provider accepted it.
	AcceptedAt time.Time `json:"accepted_at"`

	// ResponseDigest and ResponseSnippet mirror the Attempt fields.
	ResponseDigest  string `json:"response_digest,omitempty"`
	ResponseSnippet string `json:"response_snippet,omitempty"`

	// RetryAfter is an optional provider hint, honoured even on success: it
	// describes when the next message may be sent, not this one.
	RetryAfter time.Duration `json:"retry_after,omitempty"`
}

// DeliveryOutcome is the result of processing one delivery.
type DeliveryOutcome struct {
	// DeliveryID is the delivery that was processed.
	DeliveryID ID `json:"delivery_id"`

	// Status is the delivery's new state.
	Status DeliveryStatus `json:"status"`

	// Failure is set when Status is not a success.
	Failure *Failure `json:"failure,omitempty"`

	// NextAttemptAt is when the delivery will be retried, when Status is
	// DeliveryRetrying.
	NextAttemptAt time.Time `json:"next_attempt_at,omitempty"`
}

// InboxItem is one entry in a user's in-app inbox.
//
// It is a projection of a Delivery, omitting the retry bookkeeping that is an
// operator concern and would pin the wire format to internal state.
type InboxItem struct {
	// ID is the delivery ID, which is also the handle used to mark it read.
	ID ID `json:"id"`

	// MessageID is the parent message.
	MessageID ID `json:"message_id"`

	// BizType is the caller's event type, so a client can route to the right
	// screen without a lookup.
	BizType string `json:"biz_type"`

	// Subject and Body are the rendered content.
	Subject string `json:"subject,omitempty"`
	Body    string `json:"body"`

	// URL, ImageURL, and Data mirror Content.
	URL      string            `json:"url,omitempty"`
	ImageURL string            `json:"image_url,omitempty"`
	Data     map[string]string `json:"data,omitempty"`

	// ReadAt is when it was read. Zero when unread.
	ReadAt time.Time `json:"read_at,omitempty"`

	// CreatedAt is the sort key for the inbox listing.
	CreatedAt time.Time `json:"created_at"`
}

// IsUnread reports whether the inbox item has not been read.
func (i InboxItem) IsUnread() bool { return i.ReadAt.IsZero() }

// InboxItemFrom projects a delivery into an inbox item. It returns an error for
// a delivery that is not an in-app message rather than producing an empty item.
func InboxItemFrom(d *Delivery, bizType string) (InboxItem, error) {
	if !d.IsInboxItem() {
		return InboxItem{}, fmt.Errorf("message: delivery %s is on channel %q, not %q", d.ID, d.Channel, ChannelInApp)
	}
	return InboxItem{
		ID:        d.ID,
		MessageID: d.MessageID,
		BizType:   bizType,
		Subject:   d.Content.Subject,
		Body:      d.Content.Body,
		URL:       d.Content.URL,
		ImageURL:  d.Content.ImageURL,
		Data:      d.Content.Data,
		ReadAt:    d.ReadAt,
		CreatedAt: d.CreatedAt,
	}, nil
}

// Receipt is one asynchronous fact a provider reported about a delivery.
//
// Receipts are stored one row each and never updated; the delivery's Milestone
// is a cache of their maximum. Ordering cannot be trusted and nothing may assume
// it: providers batch, retry, and rarely guarantee sequence. ApplyReceipt is
// monotonic and idempotent, and is the only correct way to apply one.
type Receipt struct {
	// ID identifies the receipt.
	ID ID `json:"id"`

	// DeliveryID is the delivery this receipt is about. Receipts for an unknown
	// delivery are dropped rather than stored against nothing.
	DeliveryID ID `json:"delivery_id"`

	// MessageID is denormalised for querying a message's full funnel.
	MessageID ID `json:"message_id"`

	// Channel is denormalised for per-channel deliverability metrics.
	Channel Channel `json:"channel"`

	// Kind is what happened.
	Kind ReceiptKind `json:"kind"`

	// At is when the event occurred, as reported by the provider. Provider time
	// rather than arrival time, so that a webhook queued during an outage does
	// not make the funnel's timing describe the outage.
	At time.Time `json:"at"`

	// ReceivedAt is when the message center recorded it, for measuring callback
	// lag.
	ReceivedAt time.Time `json:"received_at"`

	// BounceType is meaningful only when Kind is ReceiptBounced.
	BounceType BounceType `json:"bounce_type,omitempty"`

	// URL is the link that was clicked, when Kind is ReceiptClicked.
	URL string `json:"url,omitempty"`

	// UserAgent and IP are the requester's, when the receipt came from a click
	// or open tracker. Both are PII and must be hashed or dropped before storage
	// in a deployment subject to GDPR.
	UserAgent string `json:"user_agent,omitempty"`
	IP        string `json:"ip,omitempty"`

	// ProviderEventID is the provider's identifier for this event, used to
	// deduplicate a retried webhook. Empty when the provider supplies none.
	ProviderEventID string `json:"provider_event_id,omitempty"`

	// Provider is the upstream that reported it.
	Provider string `json:"provider,omitempty"`

	// Raw is a redacted excerpt of the provider's payload, never the full body.
	Raw string `json:"raw,omitempty"`
}

// Validate checks the receipt's structure.
func (r *Receipt) Validate() error {
	if r.DeliveryID.IsZero() {
		return mcerr.ErrInvalidArgument.WithMessage("receipt has no delivery_id")
	}
	if !r.Kind.Valid() {
		return mcerr.ErrInvalidArgument.WithMessage("receipt has unknown kind %q", r.Kind)
	}
	if r.At.IsZero() {
		return mcerr.ErrInvalidArgument.WithMessage("receipt has no event time")
	}
	if r.BounceType != "" && !r.BounceType.Valid() {
		return mcerr.ErrInvalidArgument.WithMessage("receipt has unknown bounce type %q", r.BounceType)
	}
	if r.Kind != ReceiptBounced && r.BounceType != "" {
		return mcerr.ErrInvalidArgument.WithMessage("bounce type is only meaningful on a bounce, not on %q", r.Kind)
	}
	return nil
}

// DedupKey returns the value the store uses to reject a duplicate receipt: the
// provider's event ID when there is one, and a natural key otherwise.
func (r *Receipt) DedupKey() string {
	if r.ProviderEventID != "" {
		return "pid:" + r.ProviderEventID
	}
	return fmt.Sprintf("nat:%s:%s:%d:%s", r.DeliveryID, r.Kind, r.At.UnixNano(), r.BounceType)
}

// AppliesTo reports whether a receipt is about the given delivery.
func (r *Receipt) AppliesTo(deliveryID ID) bool { return r.DeliveryID == deliveryID }

// FunnelCounts is the aggregate a deliverability report is built from.
//
// The fields nest: Delivered is a subset of Sent, Read a subset of Delivered.
// Validate catches violations of that nesting, which indicate a milestone was
// assigned rather than advanced.
type FunnelCounts struct {
	// Sent is deliveries the provider accepted. The denominator for every rate.
	Sent int64 `json:"sent"`

	// Delivered is deliveries that reached the recipient.
	Delivered int64 `json:"delivered"`

	// Read is deliveries the recipient opened.
	Read int64 `json:"read"`

	// Clicked is deliveries the recipient followed a link from.
	Clicked int64 `json:"clicked"`

	// Bounced is deliveries reported as undeliverable.
	Bounced int64 `json:"bounced"`

	// Complained is deliveries the recipient marked as spam.
	Complained int64 `json:"complained"`
}

// Validate reports whether the counts are internally consistent.
func (f FunnelCounts) Validate() error {
	switch {
	case f.Delivered > f.Sent:
		return fmt.Errorf("message: %d delivered exceeds %d sent", f.Delivered, f.Sent)
	case f.Read > f.Delivered:
		return fmt.Errorf("message: %d read exceeds %d delivered", f.Read, f.Delivered)
	case f.Clicked > f.Read:
		return fmt.Errorf("message: %d clicked exceeds %d read", f.Clicked, f.Read)
	}
	return nil
}
