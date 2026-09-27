// Package dispatch orchestrates acceptance, fan-out, and delivery.
//
// It is the only package that depends on all the others, and the only place where
// the order of operations is decided. The required order is:
//
// Accept path:
//
//  1. validate the request
//  2. reject channels with no registered Sender
//  3. evaluate hard preferences
//  4. resolve and render the template per channel
//  5. resolve the Sender and address per channel
//  6. call Sender.Validate per channel
//  7. write the Message and its Deliveries in one transaction
//  8. enqueue one job per delivery
//
// Step 3 precedes step 4 so that a user who opted out never has a template
// rendered for them, which can fail for reasons that should not matter to a
// message that was never going to be sent. Step 7 is one transaction because a
// message without its deliveries is invisible to the retry machinery, and a
// delivery without its message is invisible to every query. Step 8 follows step 7
// because a worker that claims a delivery before it is persisted has no correct
// recovery other than to treat it as lost.
//
// Claim path:
//
//  1. claim the delivery under a lease
//  2. check expiry
//  3. evaluate temporal preferences, releasing the delivery with a future
//     NextAttemptAt on a deferral and marking it skipped on a drop
//  4. load the Sender
//  5. call Sender.Validate
//  6. call Sender.Send
//  7. commit the outcome: CommitAttempt for a completed attempt, Release for an
//     aborted one
//
// Step 3 must precede step 6, since quiet hours and caps exist to prevent a send
// rather than to react to one, and must follow step 1, so that the decision is
// made against a delivery that is actually going to be attempted.
//
// Three periodic mechanisms are required for the queue to be safely treated as an
// accelerator rather than a source of truth: a sweeper that re-enqueues due
// deliveries, a reclaimer that recovers expired leases, and a scheduler that
// materialises scheduled messages whose time has passed. This package defines
// what they call; see Maintenance.
package dispatch

import (
	"context"
	"time"

	"github.com/jiuyue1123/message-center/pkg/channel"
	"github.com/jiuyue1123/message-center/pkg/mcerr"
	"github.com/jiuyue1123/message-center/pkg/message"
	"github.com/jiuyue1123/message-center/pkg/preference"
	"github.com/jiuyue1123/message-center/pkg/queue"
	"github.com/jiuyue1123/message-center/pkg/store"
	"github.com/jiuyue1123/message-center/pkg/template"
)

// Config is everything a dispatcher needs to run. Validate rejects a
// configuration that would otherwise fail on the first message.
type Config struct {
	// Store is the source of truth. Required.
	Store store.Store

	// Queue accelerates delivery. Required; use queue.Nop for a deployment that
	// delivers only via the sweeper.
	Queue queue.Queue

	// Registry holds the Senders. Required, and must be non-empty.
	Registry *channel.Registry

	// Resolver picks a Sender and address per channel. Required.
	Resolver channel.Resolver

	// Templates resolves and renders content. Required.
	Templates *template.Service

	// Preferences decides whether a message may be sent. Required. A deployment
	// with no settings UI passes an evaluator that always allows.
	Preferences preference.Evaluator

	// Policies supplies the retry policy per delivery. Required.
	Policies PolicyResolver

	// Limiter gates send rate per channel. Nil means channel.Unlimited.
	Limiter channel.Limiter

	// Clock supplies the current time. Nil means time.Now. Every timestamp flows
	// from here so that a test can move time and so that all components agree on
	// what "now" is.
	Clock func() time.Time

	// WorkerID identifies this process for lease ownership. Required, and must be
	// unique across the fleet: two workers sharing an ID can each believe they
	// hold a lease the other released.
	WorkerID string

	// DefaultPriority applies when a message does not set one.
	DefaultPriority message.Priority

	// DefaultMaxAttempts applies when neither the message nor the policy sets
	// one. Zero means DefaultRetryPolicy's value.
	DefaultMaxAttempts int

	// HTMLStrip reduces an HTML body to text for a channel that cannot render it.
	// Supplied by the deployment, since the correct handling of entities,
	// whitespace, and links is a policy decision. Nil rejects HTML bodies rather
	// than stripping them, which is the safe direction: a rejected message is
	// visible, a mangled one is not.
	HTMLStrip func(string) string
}

// Validate checks the configuration.
func (c *Config) Validate() error {
	switch {
	case c.Store == nil:
		return mcerr.ErrInvalidArgument.WithMessage("config: Store is required")
	case c.Queue == nil:
		return mcerr.ErrInvalidArgument.WithMessage("config: Queue is required (use queue.Nop to run without one)")
	case c.Registry == nil:
		return mcerr.ErrInvalidArgument.WithMessage("config: Registry is required")
	case c.Registry.Len() == 0:
		return mcerr.ErrInvalidArgument.WithMessage("config: Registry has no senders registered, so no message could ever be delivered")
	case c.Resolver == nil:
		return mcerr.ErrInvalidArgument.WithMessage("config: Resolver is required")
	case c.Templates == nil:
		return mcerr.ErrInvalidArgument.WithMessage("config: Templates is required")
	case c.Preferences == nil:
		return mcerr.ErrInvalidArgument.WithMessage("config: Preferences is required (use an always-allow evaluator if the deployment has no settings)")
	case c.Policies == nil:
		return mcerr.ErrInvalidArgument.WithMessage("config: Policies is required")
	case c.WorkerID == "":
		return mcerr.ErrInvalidArgument.WithMessage("config: WorkerID is required; leases cannot be released without it")
	}
	if c.DefaultPriority != "" && !c.DefaultPriority.Valid() {
		return mcerr.ErrInvalidArgument.WithMessage("config: unknown default priority %q", c.DefaultPriority)
	}
	if c.DefaultMaxAttempts < 0 {
		return mcerr.ErrInvalidArgument.WithMessage("config: default max attempts cannot be negative")
	}
	return nil
}

// now returns the configured clock's time.
func (c *Config) now() time.Time {
	if c.Clock != nil {
		return c.Clock()
	}
	return time.Now()
}

// SubmitRequest is a caller's request to send a message.
type SubmitRequest struct {
	// TenantID scopes the message.
	TenantID string

	// IdempotencyKey deduplicates submissions.
	IdempotencyKey string

	// BizType is the caller's event type. Required.
	BizType string

	// Channels is the set of channels to send on. Required.
	Channels message.ChannelSet

	// Recipient is who to send to. Required.
	Recipient message.Recipient

	// Content is the default body. Optional when a template is supplied, and the
	// template's output wins where both are present.
	Content message.Content

	// ChannelContent overrides Content per channel.
	ChannelContent map[message.Channel]message.Content

	// Template is the template to render. When nil, Content is used as-is.
	Template *template.Query

	// TemplateVars are the variables supplied to the template.
	TemplateVars map[string]string

	// Priority orders queue service.
	Priority message.Priority

	// ScheduledAt defers the whole message. Zero means now.
	ScheduledAt time.Time

	// ExpiresAt bounds delivery attempts. Zero means the policy's default.
	ExpiresAt time.Time

	// MaxAttempts caps retries per delivery. Zero means the policy's default.
	MaxAttempts int

	// Metadata is caller-supplied data passed through to Senders.
	Metadata map[string]string

	// SkipPreferences bypasses the accept-time preference check, for an
	// administrative send that a user cannot opt out of.
	//
	// It does not bypass quiet hours or frequency caps, which are evaluated at
	// claim time and concern not disturbing a person rather than consent. A
	// deployment needing to bypass those expresses it through
	// QuietHours.ExemptBizTypes.
	SkipPreferences bool
}

// Validate checks the request.
func (r *SubmitRequest) Validate() error {
	if r.BizType == "" {
		return mcerr.ErrInvalidArgument.WithMessage("biz_type is required")
	}
	if r.Channels.IsEmpty() {
		return mcerr.ErrInvalidArgument.WithMessage("at least one channel is required")
	}
	if err := r.Recipient.Validate(); err != nil {
		return err
	}
	if r.Template == nil && r.Content.IsEmpty() {
		return mcerr.ErrInvalidArgument.WithMessage("either a template or a non-empty content is required")
	}
	if r.Template != nil {
		if err := r.Template.Validate(); err != nil {
			return err
		}
	}
	if r.Priority != "" && !r.Priority.Valid() {
		return mcerr.ErrInvalidArgument.WithMessage("unknown priority %q", r.Priority)
	}
	if !r.ExpiresAt.IsZero() && !r.ScheduledAt.IsZero() && r.ExpiresAt.Before(r.ScheduledAt) {
		return mcerr.ErrInvalidArgument.WithMessage("expires_at is before scheduled_at, so the message could never be attempted")
	}
	if r.MaxAttempts < 0 {
		return mcerr.ErrInvalidArgument.WithMessage("max_attempts cannot be negative")
	}
	return nil
}

// SubmitResult is what a caller gets back.
type SubmitResult struct {
	// MessageID is the handle for polling and cancellation.
	MessageID message.ID

	// Status is the message's status at the moment of acceptance, normally
	// StatusPending or StatusScheduled.
	Status message.MessageStatus

	// Deliveries describes what was fanned out.
	Deliveries []DeliverySummary

	// Deduplicated reports that this call matched an existing message via the
	// idempotency key, and that MessageID refers to that original message. A
	// deduplicated submit is a success.
	Deduplicated bool
}

// DeliverySummary is one channel's share of a submitted message.
type DeliverySummary struct {
	// ID is the delivery's identifier.
	ID message.ID

	// Channel is the transport.
	Channel message.Channel

	// Status is the delivery's status at fan-out: DeliveryPending, or
	// DeliverySkipped when a preference suppressed it.
	Status message.DeliveryStatus

	// SkipReason explains a skip. Returned to the caller rather than only logged,
	// since the caller's own support team is asked why a user did not receive a
	// message.
	SkipReason preference.Reason
}

// Service is the dispatcher's public surface.
type Service interface {
	// Submit accepts a message and fans it out. It returns once the message and
	// its deliveries are persisted, and its result says nothing about whether
	// anything was received.
	Submit(ctx context.Context, req *SubmitRequest) (*SubmitResult, error)

	// Get returns a message and its deliveries.
	Get(ctx context.Context, tenantID string, id message.ID) (*message.Message, []*message.Delivery, error)

	// Cancel stops a message that has not completed, returning the number of
	// deliveries canceled. A cancel that races a worker is not an error.
	Cancel(ctx context.Context, tenantID string, id message.ID) (int, error)

	// HandleReceipt records a provider receipt against a delivery, reporting
	// whether it advanced anything. A duplicate returns (false, nil): a provider
	// retrying its webhook is normal, and a webhook endpoint that rejects a
	// duplicate will be retried again.
	HandleReceipt(ctx context.Context, r *message.Receipt) (bool, error)

	// Inbox returns a page of a user's in-app messages.
	Inbox(ctx context.Context, q message.InboxQuery) (message.Page[message.InboxItem], error)

	// UnreadCount returns a user's unread in-app message count.
	UnreadCount(ctx context.Context, tenantID, userID string) (int64, error)

	// MarkRead marks specific in-app messages read.
	MarkRead(ctx context.Context, tenantID, userID string, ids []message.ID) error

	// MarkAllRead marks every unread in-app message for a user read.
	MarkAllRead(ctx context.Context, tenantID, userID string) (int64, error)
}

// Worker processes deliveries.
//
// The implementation belongs to this package; the interface exists so that a
// deployment can wrap the worker for instrumentation, and so that the sweeper,
// the queue consumer, and the reclaimer share one contract.
type Worker interface {
	// ProcessDelivery works one delivery to a conclusion.
	//
	// It returns a nil error when the delivery reached a terminal state,
	// including a negative one: a failed send is a successful unit of work, and
	// returning an error for it would make the caller's error rate measure
	// provider health rather than worker health.
	//
	// A non-nil error means the delivery could not be brought to a conclusion,
	// and the caller should not acknowledge the job.
	ProcessDelivery(ctx context.Context, job queue.Job) error

	// ProcessBatch works several deliveries. The default implementation loops; a
	// channel whose provider accepts batched requests may override it.
	ProcessBatch(ctx context.Context, jobs []queue.Job) error
}

// Maintenance is the recovery surface. A deployment must run all three
// mechanisms, since the queue is permitted to lose work.
type Maintenance interface {
	// SweepDue re-enqueues deliveries that are due but were not queued. A
	// deployment running with queue.Nop relies on it entirely.
	SweepDue(ctx context.Context) (int, error)

	// ReclaimLeases returns deliveries stranded by a dead worker to pending. It
	// must run at startup, before workers begin, so that this process does not
	// compete with the leases the previous one left behind.
	ReclaimLeases(ctx context.Context) (int, error)

	// MaterializeScheduled fans out scheduled messages whose time has come. It
	// invokes the same fan-out routine Submit uses, which is why fan-out logic
	// exists in exactly one place despite having two callers.
	MaterializeScheduled(ctx context.Context) (int, error)
}
