package message

import (
	"fmt"
	"strings"
)

// Priority orders queue service. It does not affect retry timing: a
// high-priority message that fails backs off on the same schedule as any other.
type Priority string

const (
	// PriorityLow is for bulk and informational traffic.
	PriorityLow Priority = "low"

	// PriorityNormal is the default.
	PriorityNormal Priority = "normal"

	// PriorityHigh is for time-sensitive transactional messages. It is a queue
	// ordering hint, not a delivery guarantee; this design provides no ordering
	// guarantee between channels.
	PriorityHigh Priority = "high"
)

var priorities = [...]Priority{PriorityLow, PriorityNormal, PriorityHigh}

// AllPriorities returns every priority in ascending order.
func AllPriorities() []Priority {
	out := make([]Priority, len(priorities))
	copy(out, priorities[:])
	return out
}

// Valid reports whether p is a recognised priority.
func (p Priority) Valid() bool {
	for _, v := range priorities {
		if p == v {
			return true
		}
	}
	return false
}

func (p Priority) String() string { return string(p) }

// Weight returns a sort key where a larger number is served first.
func (p Priority) Weight() int {
	switch p {
	case PriorityHigh:
		return 2
	case PriorityNormal:
		return 1
	default:
		return 0
	}
}

// ParsePriority converts a string to a Priority. An empty string yields
// PriorityNormal; an unrecognised value is an error.
func ParsePriority(s string) (Priority, error) {
	v := Priority(strings.ToLower(strings.TrimSpace(s)))
	if v == "" {
		return PriorityNormal, nil
	}
	if v.Valid() {
		return v, nil
	}
	return "", fmt.Errorf("message: unknown priority %q", s)
}

// MessageStatus is the lifecycle state of a Message.
//
// A Message is an envelope: its status is derived from the deliveries it fanned
// out into, via DeriveStatus.
type MessageStatus string

const (
	// StatusScheduled means the message has a future ScheduledAt and has not yet
	// fanned out. No Delivery rows exist in this state.
	StatusScheduled MessageStatus = "scheduled"

	// StatusPending means the message has fanned out and at least one delivery
	// is still in progress.
	StatusPending MessageStatus = "pending"

	// StatusSucceeded means every delivery was accepted.
	StatusSucceeded MessageStatus = "succeeded"

	// StatusPartiallySucceeded means at least one delivery was accepted and at
	// least one was not. Terminal, and declared only once every delivery has
	// settled.
	StatusPartiallySucceeded MessageStatus = "partially_succeeded"

	// StatusFailed means no delivery was accepted.
	StatusFailed MessageStatus = "failed"

	// StatusCanceled means the message was canceled before completing, or every
	// delivery was skipped.
	StatusCanceled MessageStatus = "canceled"
)

var messageStatuses = [...]MessageStatus{
	StatusScheduled,
	StatusPending,
	StatusSucceeded,
	StatusPartiallySucceeded,
	StatusFailed,
	StatusCanceled,
}

// AllMessageStatuses returns every message status in lifecycle order.
func AllMessageStatuses() []MessageStatus {
	out := make([]MessageStatus, len(messageStatuses))
	copy(out, messageStatuses[:])
	return out
}

// Valid reports whether s is a recognised message status.
func (s MessageStatus) Valid() bool {
	for _, v := range messageStatuses {
		if s == v {
			return true
		}
	}
	return false
}

func (s MessageStatus) String() string { return string(s) }

// IsTerminal reports whether the status is final, so that a polling client can
// stop.
func (s MessageStatus) IsTerminal() bool {
	switch s {
	case StatusSucceeded, StatusPartiallySucceeded, StatusFailed, StatusCanceled:
		return true
	default:
		return false
	}
}

// messageTransitions is the legal-transition table. It is data rather than a
// switch so that tests and Store implementations can iterate it.
var messageTransitions = map[MessageStatus][]MessageStatus{
	StatusScheduled: {StatusPending, StatusCanceled},
	StatusPending: {
		StatusSucceeded,
		StatusPartiallySucceeded,
		StatusFailed,
		StatusCanceled,
	},
	StatusSucceeded:          nil,
	StatusPartiallySucceeded: nil,
	StatusFailed:             nil,
	StatusCanceled:           nil,
}

// CanTransition reports whether a message may move from s to next. Store
// implementations should call it before issuing an UPDATE.
func (s MessageStatus) CanTransition(next MessageStatus) bool {
	if !s.Valid() || !next.Valid() {
		return false
	}
	for _, allowed := range messageTransitions[s] {
		if allowed == next {
			return true
		}
	}
	return false
}

// AllowedTransitions returns the statuses reachable from s.
func (s MessageStatus) AllowedTransitions() []MessageStatus {
	out := make([]MessageStatus, len(messageTransitions[s]))
	copy(out, messageTransitions[s])
	return out
}

// DeliveryStatus is the lifecycle state of a Delivery: one channel's share of
// one message.
//
// It covers only the part of the journey the message center controls, ending at
// DeliveryAccepted, which means the provider took responsibility for the
// message. What happens afterwards arrives asynchronously and out of order and
// is modelled by Milestone and Receipt.
type DeliveryStatus string

const (
	// DeliveryPending means the delivery is waiting to be claimed, either for
	// the first time or after a lease was released.
	DeliveryPending DeliveryStatus = "pending"

	// DeliverySending means a worker holds the lease and is calling the
	// provider. A delivery left here indicates a worker that died; it is
	// recovered by ReclaimExpiredLeases.
	DeliverySending DeliveryStatus = "sending"

	// DeliveryRetrying means an attempt failed retryably and the delivery is
	// waiting out its backoff.
	DeliveryRetrying DeliveryStatus = "retrying"

	// DeliveryAccepted means the provider took the message. It does not mean the
	// recipient received it; see Milestone.
	DeliveryAccepted DeliveryStatus = "accepted"

	// DeliveryFailed means the delivery will not be attempted again, either
	// because of a permanent error or because MaxAttempts was exhausted.
	DeliveryFailed DeliveryStatus = "failed"

	// DeliverySkipped means there was nothing to do: no acceptable address, an
	// opt-out, or a deferral that outlived the delivery's expiry. Not an error.
	DeliverySkipped DeliveryStatus = "skipped"

	// DeliveryCanceled means the parent message was canceled.
	DeliveryCanceled DeliveryStatus = "canceled"

	// DeliveryExpired means the delivery passed its ExpiresAt before being
	// accepted.
	DeliveryExpired DeliveryStatus = "expired"
)

var deliveryStatuses = [...]DeliveryStatus{
	DeliveryPending,
	DeliverySending,
	DeliveryRetrying,
	DeliveryAccepted,
	DeliveryFailed,
	DeliverySkipped,
	DeliveryCanceled,
	DeliveryExpired,
}

// AllDeliveryStatuses returns every delivery status in lifecycle order.
func AllDeliveryStatuses() []DeliveryStatus {
	out := make([]DeliveryStatus, len(deliveryStatuses))
	copy(out, deliveryStatuses[:])
	return out
}

// Valid reports whether s is a recognised delivery status.
func (s DeliveryStatus) Valid() bool {
	for _, v := range deliveryStatuses {
		if s == v {
			return true
		}
	}
	return false
}

func (s DeliveryStatus) String() string { return string(s) }

// IsTerminal reports whether the delivery will never be attempted again.
// Receipts may still arrive for a terminal delivery.
func (s DeliveryStatus) IsTerminal() bool {
	switch s {
	case DeliveryAccepted, DeliveryFailed, DeliverySkipped, DeliveryCanceled, DeliveryExpired:
		return true
	default:
		return false
	}
}

// IsRunnable reports whether the delivery is work a worker should pick up.
func (s DeliveryStatus) IsRunnable() bool {
	return s == DeliveryPending || s == DeliveryRetrying
}

// deliveryTransitions is the legal-transition table.
var deliveryTransitions = map[DeliveryStatus][]DeliveryStatus{
	DeliveryPending: {
		DeliverySending,
		DeliverySkipped,
		DeliveryCanceled,
		DeliveryExpired,
	},
	DeliverySending: {
		DeliveryAccepted,
		DeliveryRetrying,
		DeliveryFailed,
		// Released: shut down, aborted, or lost the lease. The only backward
		// transition, and the only one that must not increment AttemptCount.
		DeliveryPending,
		DeliveryCanceled,
	},
	DeliveryRetrying: {
		DeliverySending,
		DeliveryFailed,
		DeliveryCanceled,
		DeliveryExpired,
	},
	DeliveryAccepted: nil,
	DeliveryFailed:   nil,
	DeliverySkipped:  nil,
	DeliveryCanceled: nil,
	DeliveryExpired:  nil,
}

// CanTransition reports whether a delivery may move from s to next.
func (s DeliveryStatus) CanTransition(next DeliveryStatus) bool {
	if !s.Valid() || !next.Valid() {
		return false
	}
	for _, allowed := range deliveryTransitions[s] {
		if allowed == next {
			return true
		}
	}
	return false
}

// AllowedTransitions returns the statuses reachable from s.
func (s DeliveryStatus) AllowedTransitions() []DeliveryStatus {
	out := make([]DeliveryStatus, len(deliveryTransitions[s]))
	copy(out, deliveryTransitions[s])
	return out
}

// AttemptStatus is the outcome of a single call to a Sender.
type AttemptStatus string

const (
	// AttemptInFlight means a worker is executing the attempt. A row left in
	// this state when its delivery's lease is reclaimed is an orphan: the process
	// died mid-attempt. Orphaned rows are retained as history rather than
	// rewritten.
	AttemptInFlight AttemptStatus = "in_flight"

	// AttemptSucceeded means the provider accepted the message.
	AttemptSucceeded AttemptStatus = "succeeded"

	// AttemptFailed means the attempt failed. A failed attempt always carries a
	// Failure.
	AttemptFailed AttemptStatus = "failed"
)

var attemptStatuses = [...]AttemptStatus{AttemptInFlight, AttemptSucceeded, AttemptFailed}

// AllAttemptStatuses returns every attempt status.
func AllAttemptStatuses() []AttemptStatus {
	out := make([]AttemptStatus, len(attemptStatuses))
	copy(out, attemptStatuses[:])
	return out
}

// Valid reports whether s is a recognised attempt status.
func (s AttemptStatus) Valid() bool {
	for _, v := range attemptStatuses {
		if s == v {
			return true
		}
	}
	return false
}

func (s AttemptStatus) String() string { return string(s) }

// IsFinal reports whether the attempt has been concluded.
func (s AttemptStatus) IsFinal() bool { return s != AttemptInFlight }

// ReceiptKind is an event reported by a provider after it accepted a message.
//
// Receipts arrive asynchronously, out of order, and more than once, so they are
// stored as individual append-only rows rather than being folded into
// DeliveryStatus. The delivery carries a derived summary: the highest Milestone
// reached, plus flags for the events that are not stages of a funnel.
type ReceiptKind string

const (
	// ReceiptAccepted means the provider confirmed it took the message.
	// Redundant for providers that confirm synchronously.
	ReceiptAccepted ReceiptKind = "accepted"

	// ReceiptDelivered means the message reached the recipient's mailbox,
	// device, or handset.
	ReceiptDelivered ReceiptKind = "delivered"

	// ReceiptRead means the recipient opened the message. Reliability varies by
	// channel; see Capabilities.SupportsReceipts.
	ReceiptRead ReceiptKind = "read"

	// ReceiptClicked means the recipient followed a link.
	ReceiptClicked ReceiptKind = "clicked"

	// ReceiptBounced means the message could not reach the recipient. BounceType
	// distinguishes a permanent failure from a temporary one.
	ReceiptBounced ReceiptKind = "bounced"

	// ReceiptComplained means the recipient marked the message as spam.
	ReceiptComplained ReceiptKind = "complained"

	// ReceiptUnsubscribed means the recipient opted out through a mechanism in
	// the message itself, such as a List-Unsubscribe header.
	ReceiptUnsubscribed ReceiptKind = "unsubscribed"

	// ReceiptRejected means a provider or intermediary refused the message on
	// policy grounds. Distinct from a bounce: a bounce means the address was
	// unreachable, a rejection means the message was unacceptable.
	ReceiptRejected ReceiptKind = "rejected"
)

var receiptKinds = [...]ReceiptKind{
	ReceiptAccepted,
	ReceiptDelivered,
	ReceiptRead,
	ReceiptClicked,
	ReceiptBounced,
	ReceiptComplained,
	ReceiptUnsubscribed,
	ReceiptRejected,
}

// AllReceiptKinds returns every receipt kind.
func AllReceiptKinds() []ReceiptKind {
	out := make([]ReceiptKind, len(receiptKinds))
	copy(out, receiptKinds[:])
	return out
}

// Valid reports whether k is a recognised receipt kind.
func (k ReceiptKind) Valid() bool {
	for _, v := range receiptKinds {
		if k == v {
			return true
		}
	}
	return false
}

func (k ReceiptKind) String() string { return string(k) }

// IsMilestone reports whether this kind advances the delivery's Milestone. The
// non-milestone kinds are the orthogonal ones: bounce, complaint, unsubscribe,
// and rejection are events that happened to the message, not stages it passed
// through.
func (k ReceiptKind) IsMilestone() bool {
	switch k {
	case ReceiptAccepted, ReceiptDelivered, ReceiptRead, ReceiptClicked:
		return true
	default:
		return false
	}
}

// IsNegative reports whether this kind indicates a problem that should affect
// future sends.
func (k ReceiptKind) IsNegative() bool {
	switch k {
	case ReceiptBounced, ReceiptComplained, ReceiptUnsubscribed, ReceiptRejected:
		return true
	default:
		return false
	}
}

// Milestone is how far a delivery has progressed through the recipient-facing
// funnel.
//
// It is monotonic: ApplyReceipt takes the maximum rather than assigning, so a
// receipt arriving late cannot move the funnel backwards.
type Milestone string

const (
	// MilestoneNone means only that the provider accepted the message.
	MilestoneNone Milestone = "none"

	// MilestoneDelivered means it reached the recipient's mailbox or device.
	MilestoneDelivered Milestone = "delivered"

	// MilestoneRead means the recipient opened it.
	MilestoneRead Milestone = "read"

	// MilestoneClicked means the recipient followed a link in it.
	MilestoneClicked Milestone = "clicked"
)

var milestones = [...]Milestone{
	MilestoneNone,
	MilestoneDelivered,
	MilestoneRead,
	MilestoneClicked,
}

// AllMilestones returns every milestone in funnel order.
func AllMilestones() []Milestone {
	out := make([]Milestone, len(milestones))
	copy(out, milestones[:])
	return out
}

// Valid reports whether m is a recognised milestone.
func (m Milestone) Valid() bool {
	for _, v := range milestones {
		if m == v {
			return true
		}
	}
	return false
}

func (m Milestone) String() string { return string(m) }

// Rank returns a comparable position; larger is further along. Callers should
// compare ranks via Advance rather than assigning a milestone directly.
func (m Milestone) Rank() int {
	switch m {
	case MilestoneClicked:
		return 3
	case MilestoneRead:
		return 2
	case MilestoneDelivered:
		return 1
	default:
		return 0
	}
}

// MilestoneFor returns the milestone a receipt kind advances to, and whether the
// kind advances one at all. The second return keeps a bounce from being applied
// as a funnel stage.
func (k ReceiptKind) MilestoneFor() (Milestone, bool) {
	switch k {
	case ReceiptAccepted:
		return MilestoneNone, true
	case ReceiptDelivered:
		return MilestoneDelivered, true
	case ReceiptRead:
		return MilestoneRead, true
	case ReceiptClicked:
		return MilestoneClicked, true
	default:
		return MilestoneNone, false
	}
}

// Advance returns the further of two milestones.
func Advance(current, next Milestone) Milestone {
	if next.Rank() > current.Rank() {
		return next
	}
	return current
}

// BounceType distinguishes a permanent address failure from a temporary one.
type BounceType string

const (
	// BounceUnknown means the provider did not say. Treated as soft, because
	// suppressing a valid address is the more damaging mistake.
	BounceUnknown BounceType = "unknown"

	// BounceSoft means a temporary condition such as a full mailbox. The address
	// remains valid.
	BounceSoft BounceType = "soft"

	// BounceHard means the address does not exist or will never accept mail.
	BounceHard BounceType = "hard"
)

var bounceTypes = [...]BounceType{BounceUnknown, BounceSoft, BounceHard}

// AllBounceTypes returns every bounce type.
func AllBounceTypes() []BounceType {
	out := make([]BounceType, len(bounceTypes))
	copy(out, bounceTypes[:])
	return out
}

// Valid reports whether b is a recognised bounce type.
func (b BounceType) Valid() bool {
	for _, v := range bounceTypes {
		if b == v {
			return true
		}
	}
	return false
}

func (b BounceType) String() string { return string(b) }

// ShouldSuppress reports whether a bounce of this type should suppress the
// address. Only a hard bounce should.
func (b BounceType) ShouldSuppress() bool { return b == BounceHard }
