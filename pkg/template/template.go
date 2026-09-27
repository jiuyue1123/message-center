// Package template resolves and renders the content of a message.
//
// Resolution and rendering happen once, when a message is accepted, and the
// result is snapshotted onto each Delivery. Retries resend the snapshot and
// never re-render, so a template edited between the first attempt and the third
// cannot change what the recipient receives.
package template

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jiuyue1123/message-center/pkg/mcerr"
	"github.com/jiuyue1123/message-center/pkg/message"
)

// Template is a versioned, channel-specific, localised message body.
//
// A template is identified by four coordinates, of which a caller supplies at
// most three: BizType, Channel, Locale, and Version. Resolution searches the
// lattice from most to least specific; see Query.Candidates.
//
// Templates are immutable by version. An edit creates a new version rather than
// mutating an existing one, which is what keeps the snapshot of an in-flight
// message reproducible.
type Template struct {
	// ID identifies this version of this template.
	ID message.ID `json:"id"`

	// TenantID scopes the template.
	TenantID string `json:"tenant_id,omitempty"`

	// BizType is the caller's event type this template serves.
	BizType string `json:"biz_type"`

	// Channel is the channel this template is written for, or empty for a
	// channel-agnostic template used as a fallback.
	Channel message.Channel `json:"channel,omitempty"`

	// Locale is a BCP 47 language tag, or empty for language-neutral.
	Locale string `json:"locale,omitempty"`

	// Version is the revision number, starting at 1 and increasing.
	Version int `json:"version"`

	// SubjectTemplate and BodyTemplate are Go text/template sources.
	//
	// text/template rather than html/template: the output is not necessarily
	// HTML, and contextual escaping is applied per channel at render time by the
	// channel's own rules.
	SubjectTemplate string `json:"subject_template,omitempty"`
	BodyTemplate    string `json:"body_template"`

	// BodyFormat declares how the rendered body should be interpreted.
	BodyFormat message.BodyFormat `json:"body_format,omitempty"`

	// DefaultData supplies values for variables the caller does not provide. A
	// caller-supplied value wins.
	DefaultData map[string]string `json:"default_data,omitempty"`

	// RequiredVars lists variables the caller must supply.
	//
	// Declared explicitly rather than inferred from the template source: a
	// missing variable renders as "<no value>" rather than raising an error, so
	// without this list the failure reaches the recipient instead of the caller.
	RequiredVars []string `json:"required_vars,omitempty"`

	// URLTemplate and ImageURLTemplate are optional templates for the
	// corresponding Content fields.
	URLTemplate      string `json:"url_template,omitempty"`
	ImageURLTemplate string `json:"image_url_template,omitempty"`

	// Enabled gates resolution. A disabled template is skipped entirely, so a
	// deployment can retire a template without deleting its history.
	Enabled bool `json:"enabled"`

	// CreatedAt and UpdatedAt are bookkeeping.
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Key is the identity of a template's coordinate triple, excluding version. Two
// templates with the same Key are versions of one another.
func (t *Template) Key() Key {
	return Key{TenantID: t.TenantID, BizType: t.BizType, Channel: t.Channel, Locale: t.Locale}
}

// Validate checks the template's structure.
func (t *Template) Validate() error {
	if t.BizType == "" {
		return mcerr.ErrInvalidArgument.WithMessage("template biz_type is required")
	}
	if t.BodyTemplate == "" {
		return mcerr.ErrInvalidArgument.WithMessage("template body is required")
	}
	if t.Version < 1 {
		return mcerr.ErrInvalidArgument.WithMessage("template version must be at least 1")
	}
	if f := t.BodyFormat.Normalize(); !f.Valid() {
		return mcerr.ErrInvalidArgument.WithMessage("unknown body format %q", t.BodyFormat)
	}
	for i, v := range t.RequiredVars {
		if strings.TrimSpace(v) == "" {
			return mcerr.ErrInvalidArgument.WithMessage("required variable at position %d is empty", i)
		}
	}
	return nil
}

// Key identifies a set of template versions.
type Key struct {
	TenantID string          `json:"tenant_id,omitempty"`
	BizType  string          `json:"biz_type"`
	Channel  message.Channel `json:"channel,omitempty"`
	Locale   string          `json:"locale,omitempty"`
}

// String renders the key for a log line.
func (k Key) String() string {
	return fmt.Sprintf("%s/%s/%s/%s",
		orDefault(k.TenantID, "-"),
		k.BizType,
		orDefault(string(k.Channel), "any"),
		orDefault(k.Locale, "any"))
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// Query describes a template resolution request.
type Query struct {
	// TenantID scopes the search.
	TenantID string `json:"tenant_id,omitempty"`

	// BizType is the caller's event type. Required.
	BizType string `json:"biz_type"`

	// Channel is the channel being rendered for.
	Channel message.Channel `json:"channel,omitempty"`

	// Locale is the recipient's language tag. Empty means language-neutral.
	Locale string `json:"locale,omitempty"`

	// Version pins a specific version. Zero means the highest available.
	Version int `json:"version,omitempty"`
}

// Validate checks the query.
func (q Query) Validate() error {
	if q.BizType == "" {
		return mcerr.ErrInvalidArgument.WithMessage("template query requires a biz_type")
	}
	if q.Version < 0 {
		return mcerr.ErrInvalidArgument.WithMessage("template version cannot be negative")
	}
	return nil
}

// Candidates returns the coordinates to try, in order from most to least
// specific: channel+locale, channel, locale, then neither.
//
// Channel precedes locale. A deployment with a per-channel template and a
// per-locale generic body would rather have channel-correct content in the wrong
// language than language-correct content in a format the channel cannot carry: an
// SMS containing an email's HTML body is mangled for every reader, while an
// English push to a Chinese reader is merely suboptimal.
func (q Query) Candidates() []Key {
	base := Key{TenantID: q.TenantID, BizType: q.BizType}
	out := make([]Key, 0, 4)

	withChannel := base
	withChannel.Channel = q.Channel
	withLocale := base
	withLocale.Locale = q.Locale
	both := withChannel
	both.Locale = q.Locale

	// Duplicates are dropped so that an empty locale or channel does not run the
	// same lookup twice.
	seen := make(map[Key]struct{}, 4)
	for _, k := range []Key{both, withChannel, withLocale, base} {
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, k)
	}
	return out
}

// Renderer turns a template and a variable set into channel-ready content.
type Renderer interface {
	// Render resolves and renders the template for q, using vars.
	//
	// It returns a permanent error when no template matches, when a required
	// variable is missing, or when the template fails to execute. All three are
	// conditions under which retrying cannot help.
	Render(ctx context.Context, q Query, vars map[string]string) (message.Content, error)
}

// Resolver looks up the template a Query resolves to.
//
// It is separate from Renderer so that a transient store failure is retryable
// while a template syntax error is not, without any layer having to guess which
// it is looking at.
type Resolver interface {
	// Resolve returns the best matching enabled template.
	//
	// It returns mcerr.ErrNotFound when the search exhausts every candidate.
	Resolve(ctx context.Context, q Query) (*Template, error)

	// ListVersions returns every version of a key, newest first.
	ListVersions(ctx context.Context, k Key) ([]Template, error)
}

// Service combines resolution and rendering behind one call.
type Service struct {
	// Resolver supplies templates. Required.
	Resolver Resolver

	// Renderer renders them. Required.
	Renderer Renderer

	// Defaults are variables applied to every render, below both the template's
	// own DefaultData and the caller's vars.
	Defaults map[string]string
}

// Render resolves and renders in one call. vars may be nil.
//
// Variable layers are merged narrowest-wins: Defaults, then the template's
// DefaultData, then vars.
func (s *Service) Render(ctx context.Context, q Query, vars map[string]string) (message.Content, error) {
	if s.Resolver == nil || s.Renderer == nil {
		return message.Content{}, mcerr.ErrInternal.WithMessage("template service is not fully configured")
	}
	if err := q.Validate(); err != nil {
		return message.Content{}, err
	}
	tmpl, err := s.Resolver.Resolve(ctx, q)
	if err != nil {
		return message.Content{}, err
	}

	merged := make(map[string]string, len(s.Defaults)+len(tmpl.DefaultData)+len(vars))
	for k, v := range s.Defaults {
		merged[k] = v
	}
	for k, v := range tmpl.DefaultData {
		merged[k] = v
	}
	for k, v := range vars {
		merged[k] = v
	}

	var missing []string
	for _, name := range tmpl.RequiredVars {
		if _, ok := merged[name]; !ok {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return message.Content{}, mcerr.ErrInvalidArgument.WithMessage(
			"template %s requires variables that were not supplied: %s",
			tmpl.Key(), strings.Join(missing, ", "))
	}

	return s.Renderer.Render(ctx, q, merged)
}
