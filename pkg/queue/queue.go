// Package queue defines the work queue between the dispatcher's accept path and
// its workers.
//
// The queue is an accelerator, not a source of truth; the deliveries table is.
// A job lost when a process restarts is recovered by the sweeper, which finds
// deliveries that are due but were never queued. A job delivered twice is
// harmless, because a worker claims the delivery under a lease and the second
// claim finds it already sending.
//
// Two beliefs are therefore invalid and must not be encoded anywhere: that a
// successful Enqueue implies delivery, and that an empty queue implies no work.
//
// Jobs carry identifiers only, never rendered content or resolved addresses. A
// worker reads the delivery from the store, which is the only place holding a
// current answer.
package queue

import (
	"context"
	"time"

	"github.com/jiuyue1123/message-center/pkg/message"
)

// Job is a unit of work: one delivery to attempt.
type Job struct {
	// DeliveryID is the delivery to attempt. It is the job's identity: two jobs
	// with the same DeliveryID are duplicates, and the store's lease makes
	// processing the second one a no-op.
	DeliveryID message.ID `json:"delivery_id"`

	// Channel routes the job to a per-channel worker pool. It is the only
	// delivery attribute carried on the job, because it is immutable for the life
	// of a delivery.
	Channel message.Channel `json:"channel"`

	// TenantID scopes the job, for per-tenant workers or fair sharing.
	TenantID string `json:"tenant_id,omitempty"`

	// Priority orders the job against others in the same channel. A queue with
	// no priority ordering produces a correct system with worse latency for
	// high-priority messages, not an incorrect one.
	Priority message.Priority `json:"priority,omitempty"`

	// EnqueuedAt is when the job was queued, used for queue-latency metrics. It
	// is not the delivery's creation time; the difference between the two is the
	// backlog.
	EnqueuedAt time.Time `json:"enqueued_at"`
}

// Validate checks the job's structure. A malformed job must be rejected at
// Enqueue rather than reaching a worker that cannot route it.
func (j Job) Validate() error {
	if j.DeliveryID.IsZero() {
		return errInvalidJob("delivery_id is required")
	}
	if j.Channel == "" {
		return errInvalidJob("channel is required")
	}
	return nil
}

// Queue is the work queue contract.
type Queue interface {
	// Enqueue adds jobs.
	//
	// It must not block and must not be required for correctness: a caller that
	// ignores the returned error still delivers the message via the sweeper, so a
	// queue that is down degrades latency rather than reliability. An
	// implementation that cannot accept work should return an error promptly,
	// since blocking here blocks the accept path.
	Enqueue(ctx context.Context, jobs ...Job) error

	// Dequeue returns up to max jobs, waiting until at least one is available,
	// the context is done, or wait elapses.
	//
	// An empty slice with a nil error is legal and means nothing arrived before
	// the deadline. It is not a shutdown signal: a worker that treats it as one
	// exits during a traffic lull.
	//
	// channels restricts which channels this call accepts, which is how
	// per-channel worker pools are built. An empty set means all channels.
	Dequeue(ctx context.Context, channels message.ChannelSet, max int, wait time.Duration) ([]Job, error)

	// Ack reports that a job was processed and need not be redelivered. For a
	// queue with no redelivery this is a no-op.
	Ack(ctx context.Context, jobs ...Job) error

	// Nack reports that a job was not processed and should be redelivered, as
	// when a worker releases a delivery on shutdown. requeueAt may be in the
	// future, for a job that was rate limited.
	Nack(ctx context.Context, jobs []Job, requeueAt time.Time) error

	// Len reports the approximate number of queued jobs per channel. It is used
	// for dashboards and autoscaling, never for correctness.
	Len(ctx context.Context) (map[message.Channel]int, error)

	// Close stops accepting work and releases resources. It does not drain; work
	// still on the queue is recovered by the sweeper on the next start.
	Close() error
}

// errInvalidJob builds the error returned by Job.Validate.
//
// It is a function rather than an mcerr value so that this package does not
// import mcerr, which would introduce a cycle once a store implementation wants
// to use a queue.
func errInvalidJob(msg string) error { return invalidJobError(msg) }

type invalidJobError string

func (e invalidJobError) Error() string { return "queue: invalid job: " + string(e) }

// Nop is a Queue that discards everything, for a deployment that delivers only
// via the sweeper.
var Nop Queue = nopQueue{}

type nopQueue struct{}

// Enqueue implements Queue.
func (nopQueue) Enqueue(context.Context, ...Job) error { return nil }

// Dequeue implements Queue. It blocks for the full wait duration and then
// reports that nothing arrived, matching the documented contract so that
// substituting Nop cannot change a caller's behaviour beyond throughput.
func (nopQueue) Dequeue(ctx context.Context, _ message.ChannelSet, _ int, wait time.Duration) ([]Job, error) {
	if wait > 0 {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return nil, nil
}

// Ack implements Queue.
func (nopQueue) Ack(context.Context, ...Job) error { return nil }

// Nack implements Queue.
func (nopQueue) Nack(context.Context, []Job, time.Time) error { return nil }

// Len implements Queue.
func (nopQueue) Len(context.Context) (map[message.Channel]int, error) { return nil, nil }

// Close implements Queue.
func (nopQueue) Close() error { return nil }

var _ Queue = Nop
