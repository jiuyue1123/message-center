package message

import (
	"fmt"
	"strings"
	"time"

	"github.com/jiuyue1123/message-center/pkg/mcerr"
)

// DefaultPageSize and MaxPageSize bound every listing endpoint. A limit above
// MaxPageSize is rejected rather than truncated, since a silently short page is
// indistinguishable from the end of the results.
const (
	DefaultPageSize = 20
	MaxPageSize     = 100
)

// PageRequest describes a page of results.
//
// Pagination is keyset-based. The inbox and the delivery listing are both ordered
// by a time that only moves forward, so an offset-based reader silently skips a
// row when an insert lands between two page fetches. A cursor encodes the last
// row's sort key and the next page starts strictly after it.
type PageRequest struct {
	// Cursor is an opaque continuation token from a previous response. Empty
	// means start from the beginning.
	Cursor string `json:"cursor,omitempty"`

	// Limit is the maximum number of items to return. Zero means
	// DefaultPageSize.
	Limit int `json:"limit,omitempty"`
}

// Normalize returns the request with the page size resolved, or an error if the
// limit is out of range.
func (p PageRequest) Normalize() (PageRequest, error) {
	out := p
	switch {
	case p.Limit < 0:
		return out, mcerr.ErrInvalidArgument.WithMessage("limit cannot be negative")
	case p.Limit == 0:
		out.Limit = DefaultPageSize
	case p.Limit > MaxPageSize:
		return out, mcerr.ErrInvalidArgument.WithMessage("limit %d exceeds the maximum of %d", p.Limit, MaxPageSize)
	}
	return out, nil
}

// Page is one page of results. An empty NextCursor is the only end-of-results
// signal.
type Page[T any] struct {
	// Items are the results, in the query's sort order.
	Items []T `json:"items"`

	// NextCursor is the continuation token for the following page.
	NextCursor string `json:"next_cursor,omitempty"`
}

// NewPage builds a page from a result slice that may contain one extra item.
// Store implementations fetch limit+1 rows so that NextCursor needs no second
// query.
func NewPage[T any](items []T, limit int, cursorOf func(T) string) Page[T] {
	if limit <= 0 || len(items) <= limit {
		return Page[T]{Items: items}
	}
	items = items[:limit]
	return Page[T]{
		Items:      items,
		NextCursor: cursorOf(items[len(items)-1]),
	}
}

// IsEmpty reports whether the page has no results.
func (p Page[T]) IsEmpty() bool { return len(p.Items) == 0 }

// TimeRange is a half-open time window: From inclusive, To exclusive, so that
// adjacent windows tile without overlapping or leaving a gap.
type TimeRange struct {
	// From is inclusive. Zero means unbounded.
	From time.Time `json:"from,omitempty"`

	// To is exclusive. Zero means unbounded.
	To time.Time `json:"to,omitempty"`
}

// Contains reports whether t falls in the range.
func (r TimeRange) Contains(t time.Time) bool {
	if !r.From.IsZero() && t.Before(r.From) {
		return false
	}
	if !r.To.IsZero() && !t.Before(r.To) {
		return false
	}
	return true
}

// IsZero reports whether the range is unbounded on both ends.
func (r TimeRange) IsZero() bool { return r.From.IsZero() && r.To.IsZero() }

// Validate rejects an inverted range, which most databases would otherwise
// report as an empty result set.
func (r TimeRange) Validate() error {
	if !r.From.IsZero() && !r.To.IsZero() && !r.From.Before(r.To) {
		return mcerr.ErrInvalidArgument.WithMessage("time range start %s is not before end %s", r.From, r.To)
	}
	return nil
}

// MessageQuery filters a message listing.
type MessageQuery struct {
	// TenantID scopes the query. Empty means the deployment default.
	TenantID string `json:"tenant_id,omitempty"`

	// UserID filters to messages sent to one recipient.
	UserID string `json:"user_id,omitempty"`

	// BizType filters to one caller event type.
	BizType string `json:"biz_type,omitempty"`

	// Statuses filters by message status. Empty means no filter.
	Statuses []MessageStatus `json:"statuses,omitempty"`

	// Channels matches messages that fanned out to any of these channels.
	Channels ChannelSet `json:"channels,omitempty"`

	// CreatedIn bounds the creation time.
	CreatedIn TimeRange `json:"created_in,omitempty"`

	// IdempotencyKey looks up a single message by its dedup key.
	IdempotencyKey string `json:"idempotency_key,omitempty"`

	// Page is the pagination request.
	Page PageRequest `json:"page,omitempty"`
}

// Validate returns a normalised copy of the query.
func (q MessageQuery) Validate() (MessageQuery, error) {
	out := q
	page, err := q.Page.Normalize()
	if err != nil {
		return out, err
	}
	out.Page = page
	if err := q.CreatedIn.Validate(); err != nil {
		return out, err
	}
	for _, s := range q.Statuses {
		if !s.Valid() {
			return out, mcerr.ErrInvalidArgument.WithMessage("unknown message status %q", s)
		}
	}
	for c := range q.Channels {
		if !c.Valid() {
			return out, mcerr.ErrInvalidArgument.WithMessage("unknown channel %q", c)
		}
	}
	return out, nil
}

// DeliveryQuery filters a delivery listing.
type DeliveryQuery struct {
	// TenantID scopes the query.
	TenantID string `json:"tenant_id,omitempty"`

	// MessageID filters to one message's deliveries.
	MessageID ID `json:"message_id,omitempty"`

	// Channel filters by transport.
	Channel Channel `json:"channel,omitempty"`

	// Statuses filters by delivery status.
	Statuses []DeliveryStatus `json:"statuses,omitempty"`

	// RunnableOnly restricts the result to deliveries a worker should pick up.
	// It is a named flag rather than a Statuses entry because getting it wrong
	// produces a sweeper that either spins on terminal rows or skips retries.
	RunnableOnly bool `json:"runnable_only,omitempty"`

	// DueBefore restricts to deliveries whose NextAttemptAt has passed. Zero
	// means no restriction.
	DueBefore time.Time `json:"due_before,omitempty"`

	// CreatedIn bounds the creation time.
	CreatedIn TimeRange `json:"created_in,omitempty"`

	// Page is the pagination request.
	Page PageRequest `json:"page,omitempty"`
}

// Validate returns a normalised copy of the query.
func (q DeliveryQuery) Validate() (DeliveryQuery, error) {
	out := q
	page, err := q.Page.Normalize()
	if err != nil {
		return out, err
	}
	out.Page = page
	if err := q.CreatedIn.Validate(); err != nil {
		return out, err
	}
	for _, s := range q.Statuses {
		if !s.Valid() {
			return out, mcerr.ErrInvalidArgument.WithMessage("unknown delivery status %q", s)
		}
	}
	if q.Channel != "" && !q.Channel.Valid() {
		return out, mcerr.ErrInvalidArgument.WithMessage("unknown channel %q", q.Channel)
	}
	return out, nil
}

// ClaimQuery describes a batch of due deliveries for a worker to claim.
//
// It is separate from DeliveryQuery because it is the mutating hot path, ordered
// by priority and due time, and a Store implements it with a locking read rather
// than an index scan.
type ClaimQuery struct {
	// TenantID scopes the claim. Empty means all tenants.
	TenantID string `json:"tenant_id,omitempty"`

	// Channels restricts the claim to the channels this worker serves. Workers
	// are per-channel because channels differ in concurrency characteristics.
	Channels ChannelSet `json:"channels,omitempty"`

	// Limit is the batch size. Zero means the dispatcher's default.
	Limit int `json:"limit,omitempty"`

	// Now is the reference time for the due check, supplied by the caller so
	// that the store, the retry policy, and tests agree on what "now" means.
	Now time.Time `json:"now"`

	// LeaseOwner identifies the claiming worker.
	LeaseOwner string `json:"lease_owner"`

	// LeaseDuration is how long the claim is held before another worker may
	// reclaim it. Zero means the dispatcher's default.
	LeaseDuration time.Duration `json:"lease_duration,omitempty"`
}

// Validate checks the claim query.
func (q ClaimQuery) Validate() error {
	if q.Limit < 0 {
		return mcerr.ErrInvalidArgument.WithMessage("claim limit cannot be negative")
	}
	if q.Now.IsZero() {
		return mcerr.ErrInvalidArgument.WithMessage("claim query requires a reference time")
	}
	if q.LeaseOwner == "" {
		return mcerr.ErrInvalidArgument.WithMessage("claim query requires a lease owner")
	}
	if q.LeaseDuration < 0 {
		return mcerr.ErrInvalidArgument.WithMessage("lease duration cannot be negative")
	}
	return nil
}

// InboxQuery filters a user's in-app inbox.
type InboxQuery struct {
	// TenantID scopes the query.
	TenantID string `json:"tenant_id,omitempty"`

	// UserID is the inbox owner. Required.
	UserID string `json:"user_id,omitempty"`

	// UnreadOnly restricts to unread items.
	UnreadOnly bool `json:"unread_only,omitempty"`

	// BizType filters to one caller event type.
	BizType string `json:"biz_type,omitempty"`

	// CreatedIn bounds the creation time.
	CreatedIn TimeRange `json:"created_in,omitempty"`

	// Page is the pagination request.
	Page PageRequest `json:"page,omitempty"`
}

// Validate returns a normalised copy of the query.
func (q InboxQuery) Validate() (InboxQuery, error) {
	out := q
	if strings.TrimSpace(q.UserID) == "" {
		return out, mcerr.ErrInvalidArgument.WithMessage("inbox query requires a user_id")
	}
	page, err := q.Page.Normalize()
	if err != nil {
		return out, err
	}
	out.Page = page
	if err := q.CreatedIn.Validate(); err != nil {
		return out, err
	}
	return out, nil
}

// String renders the query for a log line, with the user ID masked.
func (q InboxQuery) String() string {
	return fmt.Sprintf("inbox{tenant=%q user=%s unread=%t biz=%q}",
		q.TenantID, maskMiddle(q.UserID, 2, 0), q.UnreadOnly, q.BizType)
}

// AttemptQuery filters a delivery attempt history.
type AttemptQuery struct {
	// DeliveryID filters to one delivery's attempts.
	DeliveryID ID `json:"delivery_id,omitempty"`

	// MessageID filters to every attempt made for one message, across all its
	// channels.
	MessageID ID `json:"message_id,omitempty"`

	// Channel filters by transport.
	Channel Channel `json:"channel,omitempty"`

	// Statuses filters by attempt status. Filtering to AttemptInFlight is how
	// reconciliation finds orphaned attempts.
	Statuses []AttemptStatus `json:"statuses,omitempty"`

	// StartedIn bounds the attempt start time.
	StartedIn TimeRange `json:"started_in,omitempty"`

	// Page is the pagination request.
	Page PageRequest `json:"page,omitempty"`
}

// Validate returns a normalised copy of the query. One of DeliveryID or
// MessageID is required: an unfiltered scan of this table is not a supported
// operation.
func (q AttemptQuery) Validate() (AttemptQuery, error) {
	out := q
	if q.DeliveryID.IsZero() && q.MessageID.IsZero() {
		return out, mcerr.ErrInvalidArgument.WithMessage("attempt query requires a delivery_id or a message_id")
	}
	page, err := q.Page.Normalize()
	if err != nil {
		return out, err
	}
	out.Page = page
	if err := q.StartedIn.Validate(); err != nil {
		return out, err
	}
	for _, s := range q.Statuses {
		if !s.Valid() {
			return out, mcerr.ErrInvalidArgument.WithMessage("unknown attempt status %q", s)
		}
	}
	if q.Channel != "" && !q.Channel.Valid() {
		return out, mcerr.ErrInvalidArgument.WithMessage("unknown channel %q", q.Channel)
	}
	return out, nil
}

// ReceiptQuery filters provider receipts.
type ReceiptQuery struct {
	// TenantID scopes the query.
	TenantID string `json:"tenant_id,omitempty"`

	// DeliveryID filters to one delivery's receipts.
	DeliveryID ID `json:"delivery_id,omitempty"`

	// MessageID filters to every receipt for one message.
	MessageID ID `json:"message_id,omitempty"`

	// Channel filters by transport.
	Channel Channel `json:"channel,omitempty"`

	// Kinds filters by receipt kind.
	Kinds []ReceiptKind `json:"kinds,omitempty"`

	// Milestones filters to receipts that advanced the funnel to one of these
	// values.
	Milestones []Milestone `json:"milestones,omitempty"`

	// OccurredIn bounds the event time as reported by the provider.
	OccurredIn TimeRange `json:"occurred_in,omitempty"`

	// ReceivedIn bounds the receipt time, for measuring callback lag.
	ReceivedIn TimeRange `json:"received_in,omitempty"`

	// Page is the pagination request.
	Page PageRequest `json:"page,omitempty"`
}

// Validate returns a normalised copy of the query. One of DeliveryID or
// MessageID is required; reporting queries use ReceiptStore.Funnel instead.
func (q ReceiptQuery) Validate() (ReceiptQuery, error) {
	out := q
	if q.DeliveryID.IsZero() && q.MessageID.IsZero() {
		return out, mcerr.ErrInvalidArgument.WithMessage("receipt query requires a delivery_id or a message_id")
	}
	page, err := q.Page.Normalize()
	if err != nil {
		return out, err
	}
	out.Page = page
	if err := q.OccurredIn.Validate(); err != nil {
		return out, err
	}
	if err := q.ReceivedIn.Validate(); err != nil {
		return out, err
	}
	for _, k := range q.Kinds {
		if !k.Valid() {
			return out, mcerr.ErrInvalidArgument.WithMessage("unknown receipt kind %q", k)
		}
	}
	for _, m := range q.Milestones {
		if !m.Valid() {
			return out, mcerr.ErrInvalidArgument.WithMessage("unknown milestone %q", m)
		}
	}
	if q.Channel != "" && !q.Channel.Valid() {
		return out, mcerr.ErrInvalidArgument.WithMessage("unknown channel %q", q.Channel)
	}
	return out, nil
}

// StatsQuery scopes an aggregate count.
type StatsQuery struct {
	// TenantID scopes the query.
	TenantID string `json:"tenant_id,omitempty"`

	// UserID filters to one recipient, for an unread badge count.
	UserID string `json:"user_id,omitempty"`

	// BizType filters to one caller event type.
	BizType string `json:"biz_type,omitempty"`

	// CreatedIn bounds the window being counted.
	CreatedIn TimeRange `json:"created_in,omitempty"`
}
