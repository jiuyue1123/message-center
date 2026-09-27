// Package store defines the persistence contract for the message center.
//
// The store is the source of truth. The queue may be in-memory and empty after a
// restart, a worker may die mid-attempt, and the dispatcher may restart at any
// moment; none of those lose a message, because the delivery rows remain in a
// state that says what to do next. Three method groups exist solely to make that
// true: ClaimDue and Release, ReclaimExpiredLeases, and ListDueScheduled.
//
// Store exposes accessors rather than embedding its sub-interfaces, so a caller
// can depend on the narrowest port it needs: the inbox handler takes an
// InboxStore, the retry loop takes a DeliveryStore.
//
// No method provides exactly-once semantics. A worker may crash after a provider
// accepted a message but before CommitAttempt recorded it; the lease then
// expires, the delivery is reclaimed, and the message is sent again. The only
// mitigation is SendRequest.DedupKey, passed to providers that support
// idempotency keys.
package store

import (
	"context"
	"errors"
	"time"

	"github.com/jiuyue1123/message-center/pkg/message"
	"github.com/jiuyue1123/message-center/pkg/template"
)

// Store aggregates the persistence ports. Every data operation lives on a
// sub-port reached through an accessor, so a caller's dependency is exactly as
// wide as its needs.
type Store interface {
	// Messages returns the message port.
	Messages() MessageStore

	// Deliveries returns the delivery port, which holds the claim query every
	// worker calls.
	Deliveries() DeliveryStore

	// Attempts returns the attempt-history port.
	Attempts() AttemptStore

	// Templates returns the template port.
	Templates() TemplateStore

	// Inbox returns the in-app inbox port.
	Inbox() InboxStore

	// Receipts returns the provider-receipt port.
	Receipts() ReceiptStore

	// Ping verifies the store is reachable, for a readiness probe. It must be
	// cheap enough to call every few seconds.
	Ping(ctx context.Context) error

	// Close releases resources. It must not wait for in-flight operations; the
	// dispatcher drains its workers first.
	Close() error
}

// Transactor runs a function in a transaction.
//
// It is separate from Store because not every store can provide one. The
// dispatcher needs it in exactly one place: accepting a message writes the
// message row and its delivery rows together, and the two must not be separable.
type Transactor interface {
	// WithTx runs fn inside a transaction, passing a Store bound to it.
	//
	// Every operation inside fn must use the inner Store. A caller that reaches
	// for the outer Store escapes the transaction and produces a write that
	// survives a rollback.
	//
	// Returning an error from fn rolls back. The implementation must also roll
	// back on panic.
	WithTx(ctx context.Context, fn func(ctx context.Context, tx Store) error) error
}

// MessageStore persists messages.
type MessageStore interface {
	// Create inserts a message.
	//
	// It must return mcerr.ErrConflict when the message's idempotency key already
	// exists for its tenant, and it must detect that by catching the uniqueness
	// violation on INSERT rather than by selecting first. Select-then-insert races
	// under concurrent identical submits, which is the case idempotency exists
	// for.
	Create(ctx context.Context, m *message.Message) error

	// Get returns a message by ID, or mcerr.ErrNotFound when absent. The not-found
	// error must be distinguishable from a transient failure.
	Get(ctx context.Context, tenantID string, id message.ID) (*message.Message, error)

	// GetByIdempotencyKey returns the message a previous submit created, for the
	// duplicate path of an idempotent submit.
	GetByIdempotencyKey(ctx context.Context, tenantID, key string) (*message.Message, error)

	// UpdateStatus moves a message to a new status.
	//
	// It must reject an illegal transition with mcerr.ErrConflict rather than
	// applying it, so that a late worker write cannot move a canceled message back
	// to pending.
	//
	// completedAt is set when the new status is terminal, and ignored otherwise.
	UpdateStatus(ctx context.Context, tenantID string, id message.ID, from, to message.MessageStatus, completedAt time.Time) error

	// List returns messages matching the query.
	List(ctx context.Context, q message.MessageQuery) (message.Page[*message.Message], error)

	// Count returns the number of messages matching the query.
	Count(ctx context.Context, q message.MessageQuery) (int64, error)

	// ListDueScheduled returns scheduled messages whose time has come.
	//
	// A scheduled message has no delivery rows and nothing pointing at it, so
	// only a periodic scan finds one whose moment passed during downtime. Without
	// it, scheduled messages are lost across every deploy.
	ListDueScheduled(ctx context.Context, now time.Time, limit int) ([]*message.Message, error)
}

// DeliveryStore persists deliveries and owns the claim protocol.
type DeliveryStore interface {
	// CreateBatch inserts a message's deliveries. It is always called inside the
	// transaction that creates the message.
	CreateBatch(ctx context.Context, ds []*message.Delivery) error

	// Get returns a delivery by ID.
	Get(ctx context.Context, id message.ID) (*message.Delivery, error)

	// ClaimDue atomically claims up to q.Limit due deliveries for q.LeaseOwner.
	//
	// It must be atomic: two concurrent workers must never receive the same
	// delivery. A SELECT followed by an UPDATE hands the same row to both under
	// load, producing duplicate messages only during traffic spikes.
	//
	// It must set Status, LeaseOwner, LeaseExpiresAt, and NextAttemptAt in the
	// same operation. A claim that sets the status but not the lease produces a
	// delivery that can never be reclaimed.
	//
	// It must not increment AttemptCount, which only CommitAttempt does.
	//
	// It selects rows whose NextAttemptAt is at or before q.Now, ordered by
	// priority then due time, restricted to q.Channels when that set is non-empty.
	// An empty result is the normal case and not an error.
	ClaimDue(ctx context.Context, q message.ClaimQuery) ([]*message.Delivery, error)

	// CommitAttempt atomically finalises an attempt and advances the delivery.
	//
	// It is the only method that increments AttemptCount. It handles three
	// outcomes: success advances the delivery to DeliveryAccepted; a retryable
	// failure with attempts remaining advances it to DeliveryRetrying with
	// NextAttemptAt set to the caller-computed backoff; a permanent failure or an
	// exhausted budget advances it to DeliveryFailed. The attempt row is written
	// in the same transaction.
	//
	// The backoff is computed by the dispatcher rather than here: it depends on
	// policy that varies by channel and deployment.
	//
	// It must also update the parent message's status, or the caller must do so in
	// the same transaction. A message whose deliveries have all settled but whose
	// status is still pending is one a client polls forever.
	CommitAttempt(ctx context.Context, outcome message.DeliveryOutcome, attempt *message.Attempt) error

	// Release returns a claimed delivery to the pending state without consuming
	// an attempt, for a worker that is shutting down or an attempt that was
	// aborted before reaching the provider. It clears the lease and leaves
	// AttemptCount untouched.
	//
	// nextAttemptAt may be set in the future, for a rate-limited release.
	Release(ctx context.Context, id message.ID, owner string, nextAttemptAt time.Time) error

	// ReclaimExpiredLeases returns deliveries whose lease lapsed to pending, and
	// reports how many it reclaimed.
	//
	// A worker that dies while holding a delivery leaves it in DeliverySending
	// with an expiring lease, invisible to every claim query until something
	// reclaims it.
	//
	// It must not increment AttemptCount: the attempt in flight did not complete.
	ReclaimExpiredLeases(ctx context.Context, now time.Time, limit int) (int, error)

	// UpdateStatus moves a delivery to a terminal status without an attempt, for
	// outcomes that are not attempts at all: skipped, canceled, expired.
	//
	// It must not increment AttemptCount.
	UpdateStatus(ctx context.Context, id message.ID, from, to message.DeliveryStatus, failure *message.Failure) error

	// List returns deliveries matching the query.
	List(ctx context.Context, q message.DeliveryQuery) (message.Page[*message.Delivery], error)

	// ListByMessage returns every delivery of one message.
	ListByMessage(ctx context.Context, messageID message.ID) ([]*message.Delivery, error)

	// CountByStatus returns delivery counts grouped by status.
	CountByStatus(ctx context.Context, q message.DeliveryQuery) (map[message.DeliveryStatus]int64, error)
}

// AttemptStore is the append-only attempt history.
type AttemptStore interface {
	// Create writes an attempt in flight. It is called in the same transaction
	// that flips the delivery to DeliverySending, so a delivery is never observed
	// as sending without a corresponding attempt row.
	Create(ctx context.Context, a *message.Attempt) error

	// Commit finalises an in-flight attempt. It is normally called as part of
	// DeliveryStore.CommitAttempt; it is exposed separately for reconciliation,
	// where an orphaned attempt is closed without touching the delivery.
	Commit(ctx context.Context, a *message.Attempt) error

	// List returns attempts matching the query, newest first. The query requires
	// a delivery or message filter.
	List(ctx context.Context, q message.AttemptQuery) (message.Page[*message.Attempt], error)
}

// TemplateStore persists templates.
type TemplateStore interface {
	// Resolve returns the best matching enabled template for a query, trying the
	// candidates from Query.Candidates in order and taking the highest version
	// within a candidate.
	//
	// A single query of the form
	// `channel IN (?, '') AND locale IN (?, '') ORDER BY version DESC` filesorts;
	// prefer two queries, or resolve the lattice in Go over a small indexed result
	// set.
	Resolve(ctx context.Context, q template.Query) (*template.Template, error)

	// Get returns a specific version.
	Get(ctx context.Context, tenantID string, id message.ID) (*template.Template, error)

	// ListVersions returns every version of a key, newest first.
	ListVersions(ctx context.Context, k template.Key) ([]template.Template, error)

	// Create inserts a new template version. A version that already exists for
	// the key must return mcerr.ErrConflict rather than overwriting, since
	// templates are immutable by version.
	Create(ctx context.Context, t *template.Template) error

	// UpdateEnabled toggles a version's enabled flag. It is the only mutation a
	// template supports.
	UpdateEnabled(ctx context.Context, tenantID string, id message.ID, enabled bool) error
}

// InboxStore serves the in-app inbox. It is a port over deliveries rather than
// over a separate table, because an in-app delivery row is the inbox row.
type InboxStore interface {
	// List returns a page of a user's in-app messages, newest first.
	List(ctx context.Context, q message.InboxQuery) (message.Page[message.InboxItem], error)

	// UnreadCount returns the number of unread in-app messages for a user. It is
	// called on every app foreground and must be served by a covering index.
	UnreadCount(ctx context.Context, tenantID, userID string) (int64, error)

	// MarkRead marks specific deliveries read.
	//
	// It must be idempotent: marking an already-read item read is a success, so
	// that a client retrying after a timeout does not receive an error for having
	// succeeded.
	//
	// It must be scoped to userID. An unscoped mark-read lets any caller mark any
	// user's messages read.
	MarkRead(ctx context.Context, tenantID, userID string, ids []message.ID, at time.Time) error

	// MarkAllRead marks every unread in-app message for a user read.
	MarkAllRead(ctx context.Context, tenantID, userID string, at time.Time) (int64, error)
}

// ReceiptStore persists provider receipts and maintains the derived milestone.
//
// Record is a single method rather than an insert plus an update, because the two
// must not be separable: a receipt row without the milestone advanced leaves the
// funnel under-counting, and a milestone advanced without the row leaves it
// over-counting with nothing to explain why.
type ReceiptStore interface {
	// Record persists a receipt and folds it into the delivery's derived state.
	//
	// In one transaction it must:
	//
	//   - insert the receipt, rejecting a duplicate per Receipt.DedupKey with
	//     mcerr.ErrConflict. Providers retry their webhooks, so a handler that is
	//     not idempotent records every receipt several times after a single
	//     provider outage.
	//
	//   - advance the delivery's Milestone to the maximum of its current value and
	//     the receipt's, using message.Advance. Never assign: a late delivery
	//     receipt arriving after a read receipt must leave the milestone at read.
	//
	//   - set Bounced and BouncedAt for a bounce, upgrading soft to hard but never
	//     downgrading hard to soft.
	//
	//   - set ComplainedAt for a complaint, and ReadAt once the milestone reaches
	//     at least read.
	//
	//   - leave DeliveryStatus unchanged. A bounce does not move a delivery from
	//     DeliveryAccepted to DeliveryFailed: the send succeeded, and a retry
	//     would fail identically.
	//
	// It reports whether anything changed; a duplicate returns false with
	// mcerr.ErrConflict.
	Record(ctx context.Context, r *message.Receipt) (changed bool, err error)

	// List returns receipts matching the query, newest first.
	List(ctx context.Context, q message.ReceiptQuery) (message.Page[*message.Receipt], error)

	// Funnel returns delivery counts by milestone for a time window.
	//
	// It reads the denormalised Milestone on deliveries rather than aggregating
	// receipts, because the report is per-delivery and a delivery with four
	// receipts must count once.
	Funnel(ctx context.Context, q message.ReceiptQuery) (message.FunnelCounts, error)
}

// ErrNotFound is re-exported so that callers do not need to import mcerr for the
// common case of checking a lookup result.
var ErrNotFound = errors.New("store: not found")

// Health is a store's self-reported status, for a readiness endpoint.
type Health struct {
	// OK reports whether the store is usable.
	OK bool `json:"ok"`

	// Latency is how long the health check took.
	Latency time.Duration `json:"latency"`

	// Detail carries a human-readable reason when OK is false.
	Detail string `json:"detail,omitempty"`
}

// Reporter is an optional interface a Store may implement to expose health
// detail beyond Ping.
type Reporter interface {
	Health(ctx context.Context) Health
}
