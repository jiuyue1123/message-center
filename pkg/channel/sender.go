// Package channel defines the Sender extension point: the contract a transport
// implements to become a message-center delivery channel.
//
// Registering an implementation with Registry is the only step required to add a
// channel. Routing, fan-out, retry, attempt recording, and inbox projection are
// handled by the dispatcher against this interface alone.
//
// See docs/adding-a-channel.md for the implementation guide.
package channel

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jiuyue1123/message-center/pkg/mcerr"
	"github.com/jiuyue1123/message-center/pkg/message"
)

// Capabilities describes what a Sender can express and how its provider behaves.
//
// The dispatcher consults it to pre-reject or degrade content before a Send
// call. A Sender that does not support a content field must also declare how the
// content should be degraded via HTMLPolicy and the size limits.
type Capabilities struct {
	// SupportsTitle reports whether the channel has a title or subject line.
	// When false, the dispatcher folds the subject into the body if the channel
	// can carry it.
	SupportsTitle bool

	// SupportsHTML reports whether the channel renders HTML.
	SupportsHTML bool

	// SupportsData reports whether the channel carries a structured data
	// dictionary alongside the text.
	SupportsData bool

	// SupportsURL reports whether the channel can carry a deep link or landing
	// page.
	SupportsURL bool

	// SupportsImage reports whether the channel can carry an image reference.
	SupportsImage bool

	// SupportsReceipts reports whether the provider can call back with delivery
	// events. DeliveryAccepted does not imply the recipient received anything;
	// this field declares whether a Milestone will ever advance past
	// MilestoneNone.
	SupportsReceipts bool

	// Idempotent reports whether the provider honours a caller-supplied
	// deduplication key. It is the only defence against duplicate delivery for
	// retried attempts.
	Idempotent bool

	// MaxBodyBytes caps the body length. Zero means unbounded. The dispatcher
	// truncates rather than rejects, since a long body is an authoring mistake
	// that should not cost the recipient the whole message.
	MaxBodyBytes int

	// MaxTitleBytes caps the title length. Zero means unbounded.
	MaxTitleBytes int

	// HTMLPolicy declares what happens to an HTML body on a channel that does
	// not support HTML. Ignored when SupportsHTML is true.
	HTMLPolicy HTMLPolicy

	// Notes is free-form text for operators, surfaced by the channel listing
	// endpoint. Not interpreted.
	Notes string
}

// HTMLPolicy declares how a Sender handles an HTML body it cannot render.
type HTMLPolicy string

const (
	// HTMLReject causes Validate to return a permanent error for an HTML body.
	HTMLReject HTMLPolicy = "reject"

	// HTMLStrip causes the body to be reduced to text before sending, using the
	// dispatcher's stripper so that every channel strips identically.
	HTMLStrip HTMLPolicy = "strip"

	// HTMLPassthrough sends the body as-is. Correct only when the receiving end
	// renders HTML on its own.
	HTMLPassthrough HTMLPolicy = "passthrough"
)

var htmlPolicies = [...]HTMLPolicy{HTMLReject, HTMLStrip, HTMLPassthrough}

// AllHTMLPolicies returns every HTML policy.
func AllHTMLPolicies() []HTMLPolicy {
	out := make([]HTMLPolicy, len(htmlPolicies))
	copy(out, htmlPolicies[:])
	return out
}

// Valid reports whether p is a recognised policy.
func (p HTMLPolicy) Valid() bool {
	for _, v := range htmlPolicies {
		if p == v {
			return true
		}
	}
	return false
}

func (p HTMLPolicy) String() string { return string(p) }

// Normalize replaces the empty policy with HTMLStrip, which never presents raw
// markup to a recipient and never rejects a message that could be degraded.
func (p HTMLPolicy) Normalize() HTMLPolicy {
	if p == "" {
		return HTMLStrip
	}
	return p
}

// SendRequest is everything a Sender needs to perform one attempt.
//
// It is a value rather than an interface so that a Sender cannot reach back into
// the dispatcher, and so that the whole request is visible in a test fixture.
type SendRequest struct {
	// Delivery is the delivery being attempted, with the rendered content and
	// the resolved address already applied.
	Delivery *message.Delivery

	// Recipient is the resolved recipient, including the chosen address.
	Recipient message.Recipient

	// Content is the rendered content for this channel, already degraded to fit
	// Capabilities: an absent title where SupportsTitle is false, HTML stripped
	// where SupportsHTML is false, and the body truncated to MaxBodyBytes.
	Content message.Content

	// Attempt is the 1-based attempt number, equal to Delivery.AttemptCount at
	// the moment of the call.
	Attempt int

	// Metadata is the merged message-level and delivery-level metadata.
	Metadata map[string]string

	// Now is the reference time, supplied by the dispatcher so that a Sender
	// does not read the clock directly.
	Now time.Time
}

// DedupKey returns the value to pass to a provider that supports idempotency
// keys. It is derived from the message ID and channel, so it is stable across
// retries of the same logical send.
func (r SendRequest) DedupKey() string {
	if r.Delivery == nil {
		return ""
	}
	return string(r.Delivery.MessageID) + ":" + string(r.Delivery.Channel)
}

// MessageID returns the parent message ID, or the zero ID if the request is
// malformed.
func (r SendRequest) MessageID() message.ID {
	if r.Delivery == nil {
		return ""
	}
	return r.Delivery.MessageID
}

// TenantID returns the tenant the delivery belongs to.
func (r SendRequest) TenantID() string {
	if r.Delivery == nil {
		return ""
	}
	return r.Delivery.TenantID
}

// Validate checks that the request is internally consistent.
func (r SendRequest) Validate() error {
	if r.Delivery == nil {
		return mcerr.ErrInvalidArgument.WithMessage("send request has no delivery")
	}
	if r.Delivery.MessageID.IsZero() {
		return mcerr.ErrInvalidArgument.WithMessage("send request delivery has no message_id")
	}
	if r.Delivery.Channel == "" {
		return mcerr.ErrInvalidArgument.WithMessage("send request delivery has no channel")
	}
	if r.Attempt <= 0 {
		return mcerr.ErrInvalidArgument.WithMessage("send request attempt must be positive")
	}
	if r.Now.IsZero() {
		return mcerr.ErrInvalidArgument.WithMessage("send request has no reference time")
	}
	return nil
}

// Sender is the extension point. Implementing this interface and registering the
// result is the entire cost of adding a channel.
//
// Implementations must be safe for concurrent use, since the dispatcher calls
// Send from a worker pool. A Sender must not write to the Store; persistence is
// the dispatcher's responsibility, and a Sender that records its own attempts
// races the dispatcher's writes.
type Sender interface {
	// Channel is the routing key this Sender serves. Registering two Senders for
	// the same channel is a startup error.
	Channel() message.Channel

	// Accepts lists the address forms this Sender can consume, in preference
	// order. It must be non-empty; the dispatcher resolves an address with
	// Recipient.FirstAccepting, so the order is a real preference.
	//
	// Returning FormUserID is how a channel opts into resolving its own address
	// from an identity. The dispatcher does not do it on the Sender's behalf.
	Accepts() []message.AddressForm

	// Capabilities describes content support and provider behaviour. Called once
	// at registration and cached, so it must be cheap and must not vary per call.
	Capabilities() Capabilities

	// Validate performs offline, side-effect-free checks on a request.
	//
	// It must return a *mcerr.Error of KindPermanent for anything that can never
	// succeed. It must not perform network I/O: Validate runs synchronously on
	// the request accept path, for every channel of every message, and a provider
	// call here becomes an accept-path latency regression.
	Validate(ctx context.Context, req SendRequest) error

	// Send performs one delivery attempt.
	//
	// On success it returns a SendResult whose AcceptedAt is set. On failure it
	// must return an error whose Kind encodes retryability: a bare error is
	// classified as KindUnknown and retried. When the context is canceled it must
	// return mcerr.FromContext's result, so that the delivery is re-queued
	// without consuming an attempt.
	//
	// Success means the provider accepted the message. It does not mean the
	// recipient received it.
	Send(ctx context.Context, req SendRequest) (message.SendResult, error)
}

// Registry holds the Senders available to a dispatcher. It is read-mostly:
// registrations happen at startup, lookups on every fan-out.
type Registry struct {
	mu      sync.RWMutex
	senders map[message.Channel]Sender
	caps    map[message.Channel]Capabilities
	order   []message.Channel
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{
		senders: make(map[message.Channel]Sender),
		caps:    make(map[message.Channel]Capabilities),
	}
}

// Register adds a Sender.
//
// Registering the same channel twice is an error rather than a replacement: a
// silent override would make deliveries go through whichever registration
// happened to run last.
func (r *Registry) Register(s Sender) error {
	if s == nil {
		return fmt.Errorf("channel: cannot register a nil sender")
	}
	ch := s.Channel()
	if ch == "" {
		return fmt.Errorf("channel: sender %T reports an empty channel", s)
	}

	accepts := s.Accepts()
	if len(accepts) == 0 {
		return fmt.Errorf("channel: sender for %q accepts no address forms, so it can never be routed to", ch)
	}
	for i, f := range accepts {
		if !f.Valid() {
			return fmt.Errorf("channel: sender for %q lists unknown address form %q at position %d", ch, f, i)
		}
	}

	caps := s.Capabilities()
	if !caps.SupportsHTML && !caps.HTMLPolicy.Normalize().Valid() {
		return fmt.Errorf("channel: sender for %q sets unknown HTML policy %q", ch, caps.HTMLPolicy)
	}
	if caps.MaxBodyBytes < 0 || caps.MaxTitleBytes < 0 {
		return fmt.Errorf("channel: sender for %q declares a negative size limit", ch)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.senders[ch]; exists {
		return fmt.Errorf("channel: a sender for %q is already registered", ch)
	}
	r.senders[ch] = s
	r.caps[ch] = caps
	r.order = append(r.order, ch)
	return nil
}

// MustRegister panics if Register fails.
func (r *Registry) MustRegister(s Sender) {
	if err := r.Register(s); err != nil {
		panic(err)
	}
}

// Lookup returns the Sender for a channel.
func (r *Registry) Lookup(ch message.Channel) (Sender, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	s, ok := r.senders[ch]
	return s, ok
}

// Capabilities returns the cached capabilities for a channel.
func (r *Registry) Capabilities(ch message.Channel) (Capabilities, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	c, ok := r.caps[ch]
	return c, ok
}

// Channels returns the registered channels in registration order.
func (r *Registry) Channels() []message.Channel {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]message.Channel, len(r.order))
	copy(out, r.order)
	return out
}

// Len returns the number of registered Senders.
func (r *Registry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.senders)
}

// Missing returns the channels in want that have no registered Sender. The
// dispatcher calls it before accepting a message so that an unregistered channel
// is a 400 rather than a permanently stuck delivery.
func (r *Registry) Missing(want message.ChannelSet) []message.Channel {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []message.Channel
	for _, ch := range want.Channels() {
		if _, ok := r.senders[ch]; !ok {
			out = append(out, ch)
		}
	}
	return out
}

// Resolver selects a Sender and an address for one channel.
type Resolver interface {
	// Resolve returns the Sender for ch and the address to use.
	//
	// It returns a permanent error when the channel has no Sender or the
	// recipient has no acceptable address. The dispatcher turns the latter into a
	// skipped delivery rather than a failed one.
	Resolve(ch message.Channel, r message.Recipient) (Sender, message.Address, error)
}

// RegistryResolver is the default Resolver, backed by a Registry.
type RegistryResolver struct {
	// Registry supplies the Senders. Required.
	Registry *Registry

	// AllowEmptyAddress permits a Sender whose channel addresses by identity
	// alone to be resolved, using the recipient's user ID as the address. The
	// in-app channel requires it.
	AllowEmptyAddress bool
}

// Resolve implements Resolver.
func (rr RegistryResolver) Resolve(ch message.Channel, r message.Recipient) (Sender, message.Address, error) {
	if rr.Registry == nil {
		return nil, message.Address{}, mcerr.ErrInternal.WithMessage("resolver has no registry")
	}
	s, ok := rr.Registry.Lookup(ch)
	if !ok {
		return nil, message.Address{}, mcerr.ErrInvalidArgument.
			WithMessage("no sender registered for channel %q", ch)
	}

	addr, found := r.FirstAccepting(s.Accepts())
	if found {
		return s, addr, nil
	}

	if rr.AllowEmptyAddress && acceptsIdentity(s.Accepts()) {
		return s, message.Address{Form: message.FormUserID, Value: r.UserID}, nil
	}

	return nil, message.Address{}, mcerr.ErrNoAddress.
		WithChannel(string(ch)).
		WithMessage("recipient has no address accepted by %q (wants one of %s)",
			ch, joinForms(s.Accepts()))
}

// acceptsIdentity reports whether the Sender declared user_id as an acceptable
// form, which is how it opts into addressing by identity.
func acceptsIdentity(forms []message.AddressForm) bool {
	for _, f := range forms {
		if f == message.FormUserID {
			return true
		}
	}
	return false
}

// joinForms renders a form list for an error message.
func joinForms(forms []message.AddressForm) string {
	parts := make([]string, len(forms))
	for i, f := range forms {
		parts[i] = string(f)
	}
	return strings.Join(parts, ", ")
}

// SenderFunc adapts functions to the Sender interface, for tests and for trivial
// channels. SendFunc is required; the remaining fields have defaults.
type SenderFunc struct {
	// ChannelName is what Channel returns.
	ChannelName message.Channel

	// Forms is what Accepts returns. Defaults to [FormUserID] when empty.
	Forms []message.AddressForm

	// Caps is what Capabilities returns.
	Caps Capabilities

	// ValidateFunc is what Validate returns. A nil value validates successfully.
	ValidateFunc func(ctx context.Context, req SendRequest) error

	// SendFunc is what Send returns.
	SendFunc func(ctx context.Context, req SendRequest) (message.SendResult, error)

	// SortedForms sorts Forms before returning it. Ordinarily unset; Accepts is
	// expected to express a preference order.
	SortedForms bool
}

// Channel implements Sender.
func (f SenderFunc) Channel() message.Channel { return f.ChannelName }

// Accepts implements Sender.
func (f SenderFunc) Accepts() []message.AddressForm {
	if len(f.Forms) == 0 {
		return []message.AddressForm{message.FormUserID}
	}
	out := make([]message.AddressForm, len(f.Forms))
	copy(out, f.Forms)
	if f.SortedForms {
		sort.SliceStable(out, func(i, j int) bool { return out[i] < out[j] })
	}
	return out
}

// Capabilities implements Sender.
func (f SenderFunc) Capabilities() Capabilities { return f.Caps }

// Validate implements Sender.
func (f SenderFunc) Validate(ctx context.Context, req SendRequest) error {
	if f.ValidateFunc == nil {
		return nil
	}
	return f.ValidateFunc(ctx, req)
}

// Send implements Sender.
func (f SenderFunc) Send(ctx context.Context, req SendRequest) (message.SendResult, error) {
	if f.SendFunc == nil {
		return message.SendResult{}, mcerr.ErrInternal.
			WithMessage("channel %q has no Send implementation", f.ChannelName)
	}
	return f.SendFunc(ctx, req)
}

var _ Sender = SenderFunc{}
