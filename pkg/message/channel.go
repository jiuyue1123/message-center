package message

import (
	"fmt"
	"sort"
	"strings"
)

// Channel identifies a delivery transport.
//
// It is a string so that it is readable in a column, a metric label, a query
// parameter, and a log line without a lookup table. The set is open: a
// deployment may register a transport this package does not know about, which is
// what Sender.Accepts and Capabilities exist to support.
type Channel string

const (
	// ChannelInApp is the in-product message inbox. Delivery cannot fail, so it
	// never retries, and the delivery row is also the inbox row.
	ChannelInApp Channel = "in_app"

	// ChannelPush is a mobile push notification (APNs, FCM), addressed by device
	// token. Tokens rotate, so a permanent failure usually means the token should
	// be discarded rather than that the user is gone.
	ChannelPush Channel = "push"

	// ChannelEmail is electronic mail.
	ChannelEmail Channel = "email"

	// ChannelSMS is a text message. Each attempt is billable and content is
	// heavily constrained, which is what Capabilities exists for.
	ChannelSMS Channel = "sms"

	// ChannelWebhook is an HTTP callback to a customer-controlled endpoint. The
	// only channel whose recipient is a URL rather than a person.
	ChannelWebhook Channel = "webhook"
)

var channels = [...]Channel{
	ChannelInApp,
	ChannelPush,
	ChannelEmail,
	ChannelSMS,
	ChannelWebhook,
}

// AllChannels returns the channels this package defines. It is not the set a
// deployment has registered; use channel.Registry.Channels for that.
func AllChannels() []Channel {
	out := make([]Channel, len(channels))
	copy(out, channels[:])
	return out
}

// Valid reports whether c is one of the channels defined by this package. A
// false result does not mean c is unusable: a deployment may have registered a
// custom transport.
func (c Channel) Valid() bool {
	for _, v := range channels {
		if c == v {
			return true
		}
	}
	return false
}

func (c Channel) String() string { return string(c) }

// IsInteractive reports whether the channel is expected to reach a human
// promptly.
func (c Channel) IsInteractive() bool {
	switch c {
	case ChannelInApp, ChannelPush:
		return true
	default:
		return false
	}
}

// IsMachineToMachine reports whether the channel terminates at a system rather
// than a person. Webhook is the only such built-in channel.
func (c Channel) IsMachineToMachine() bool { return c == ChannelWebhook }

// ParseChannel converts a string to a Channel, accepting the built-in names
// case-insensitively. Custom channels are accepted as-is when allowCustom is
// true, since this package cannot enumerate what a deployment has registered.
func ParseChannel(s string, allowCustom bool) (Channel, error) {
	c := Channel(strings.ToLower(strings.TrimSpace(s)))
	if c == "" {
		return "", fmt.Errorf("message: empty channel")
	}
	if c.Valid() {
		return c, nil
	}
	if allowCustom {
		return c, nil
	}
	return "", fmt.Errorf("message: unknown channel %q", s)
}

// AddressForm identifies the kind of address a recipient is reached at.
//
// A Recipient holds a set of (form, value) pairs rather than one typed field per
// kind, and each Sender declares the forms it accepts via Sender.Accepts. The
// dispatcher therefore picks a matching address without knowing which channel
// wants what, and a new channel that needs a new address kind adds a constant
// here and nothing else.
type AddressForm string

const (
	// FormUserID addresses a user within the message center's own identity
	// space. Used by in-app delivery, and by channels that resolve an address
	// themselves from an identity.
	FormUserID AddressForm = "user_id"

	// FormEmail addresses a mailbox.
	FormEmail AddressForm = "email"

	// FormPhone addresses a phone number in E.164 form. E.164 is required rather
	// than recommended: providers reject local formats inconsistently.
	FormPhone AddressForm = "phone"

	// FormDeviceToken addresses a push endpoint: an APNs token or an FCM
	// registration token. A token addresses a device, not a person, so one user
	// with several devices has several addresses.
	FormDeviceToken AddressForm = "device_token"

	// FormWebhookURL addresses an HTTP endpoint.
	FormWebhookURL AddressForm = "webhook_url"
)

var forms = [...]AddressForm{
	FormUserID,
	FormEmail,
	FormPhone,
	FormDeviceToken,
	FormWebhookURL,
}

// AllAddressForms returns every address form this package defines.
func AllAddressForms() []AddressForm {
	out := make([]AddressForm, len(forms))
	copy(out, forms[:])
	return out
}

// Valid reports whether f is a recognised address form.
func (f AddressForm) Valid() bool {
	for _, v := range forms {
		if f == v {
			return true
		}
	}
	return false
}

func (f AddressForm) String() string { return string(f) }

// IsPersonallyIdentifiable reports whether values of this form must be masked in
// logs and API responses. FormUserID is a pseudonymous key and is not PII;
// FormWebhookURL addresses a system rather than a person.
func (f AddressForm) IsPersonallyIdentifiable() bool {
	return f != FormUserID && f != FormWebhookURL
}

// ChannelSet is a set of channels.
//
// The zero value is not usable for writes; construct with NewChannelSet. A nil
// ChannelSet is safe to read, since a nil map is a valid empty map.
type ChannelSet map[Channel]struct{}

// NewChannelSet builds a set, dropping duplicates and empty values.
func NewChannelSet(chs ...Channel) ChannelSet {
	s := make(ChannelSet, len(chs))
	for _, c := range chs {
		if c == "" {
			continue
		}
		s[c] = struct{}{}
	}
	return s
}

// Add inserts channels into the set. It panics on a nil set, which is the
// correct outcome for a forgotten constructor.
func (s ChannelSet) Add(chs ...Channel) {
	if s == nil {
		panic("message: ChannelSet.Add on a nil set; use NewChannelSet")
	}
	for _, c := range chs {
		if c != "" {
			s[c] = struct{}{}
		}
	}
}

// Remove deletes channels from the set. Removing from a nil set is a no-op.
func (s ChannelSet) Remove(chs ...Channel) {
	for _, c := range chs {
		delete(s, c)
	}
}

// Has reports whether c is in the set.
func (s ChannelSet) Has(c Channel) bool {
	_, ok := s[c]
	return ok
}

// Len returns the number of channels in the set.
func (s ChannelSet) Len() int { return len(s) }

// IsEmpty reports whether the set has no channels.
func (s ChannelSet) IsEmpty() bool { return len(s) == 0 }

// Channels returns the members in a stable order: built-in channels in their
// canonical order, then custom channels in lexical order. Stability matters
// because fan-out order and response bodies must not permute between runs.
func (s ChannelSet) Channels() []Channel {
	out := make([]Channel, 0, len(s))
	for _, c := range channels {
		if s.Has(c) {
			out = append(out, c)
		}
	}
	custom := make([]Channel, 0, len(s))
	for c := range s {
		if !c.Valid() {
			custom = append(custom, c)
		}
	}
	sort.Slice(custom, func(i, j int) bool { return custom[i] < custom[j] })
	return append(out, custom...)
}

// Strings returns the members as strings, in the same order as Channels.
func (s ChannelSet) Strings() []string {
	chs := s.Channels()
	out := make([]string, len(chs))
	for i, c := range chs {
		out[i] = string(c)
	}
	return out
}

// Equal reports whether two sets contain the same channels.
func (s ChannelSet) Equal(o ChannelSet) bool {
	if len(s) != len(o) {
		return false
	}
	for c := range s {
		if !o.Has(c) {
			return false
		}
	}
	return true
}

// Union returns a new set containing every channel in either set.
func (s ChannelSet) Union(o ChannelSet) ChannelSet {
	out := make(ChannelSet, len(s)+len(o))
	for c := range s {
		out[c] = struct{}{}
	}
	for c := range o {
		out[c] = struct{}{}
	}
	return out
}

// Intersect returns a new set containing only the channels in both sets.
func (s ChannelSet) Intersect(o ChannelSet) ChannelSet {
	out := make(ChannelSet)
	for c := range s {
		if o.Has(c) {
			out[c] = struct{}{}
		}
	}
	return out
}

// Without returns a new set with the given channels removed. It never mutates
// the receiver, so it is safe on a set read from a stored Message.
func (s ChannelSet) Without(chs ...Channel) ChannelSet {
	drop := NewChannelSet(chs...)
	out := make(ChannelSet, len(s))
	for c := range s {
		if !drop.Has(c) {
			out[c] = struct{}{}
		}
	}
	return out
}

// Clone returns an independent copy. A nil set clones to an empty, writable set.
func (s ChannelSet) Clone() ChannelSet {
	out := make(ChannelSet, len(s))
	for c := range s {
		out[c] = struct{}{}
	}
	return out
}

// String renders the set as an ordered, comma-separated list.
func (s ChannelSet) String() string { return strings.Join(s.Strings(), ",") }
