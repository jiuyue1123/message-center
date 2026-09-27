package message

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
)

// ID is the identifier used by every entity in the message center: messages,
// deliveries, attempts, and templates.
//
// The encoding is a 26-character Crockford base32 ULID, comprising a 48-bit
// millisecond timestamp and 80 bits of randomness. Lexicographic order matches
// chronological order, so a B-tree primary key on ID is insert-ordered rather
// than random. It is also JSON- and URL-safe without escaping.
//
// IDs sharing a millisecond are ordered by their random component, not by
// generation order. IDs are not a source of sequencing, and nothing depends on
// them being one; use the stored created_at column for ordering.
//
// The 26-byte string form is not part of the contract. The MySQL store encodes
// IDs as BINARY(16) and nothing outside it may assume the string length.
type ID string

const (
	// encodedSize is the length of a canonical ULID string: 128 bits at 5 bits
	// per character, rounded up.
	encodedSize = 26

	// entropySize is the number of random bytes appended to the timestamp.
	entropySize = 10

	// maxTime is the largest millisecond value representable in the 48-bit
	// timestamp field.
	maxTime = 1<<48 - 1
)

// alphabet is Crockford base32: digits and uppercase letters excluding I, L, O,
// and U.
const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// decTable maps a byte to its 5-bit value, or -1 when the byte is not a valid
// character. Built once from alphabet so the encoder and decoder cannot drift.
var decTable [256]int8

func init() {
	for i := range decTable {
		decTable[i] = -1
	}
	for i := 0; i < len(alphabet); i++ {
		decTable[alphabet[i]] = int8(i)
		decTable[lower(alphabet[i])] = int8(i)
	}
	// Decoding accepts the ambiguous glyphs; encoding never emits them.
	for _, alias := range []struct {
		from byte
		to   byte
	}{
		{'I', '1'}, {'i', '1'},
		{'L', '1'}, {'l', '1'},
		{'O', '0'}, {'o', '0'},
	} {
		decTable[alias.from] = decTable[alias.to]
	}
}

// lower folds an ASCII uppercase letter to lowercase, leaving other bytes alone.
func lower(c byte) byte {
	if c >= 'A' && c <= 'Z' {
		return c + ('a' - 'A')
	}
	return c
}

// ErrInvalidID is returned by Parse for a malformed identifier.
var ErrInvalidID = errors.New("message: invalid id")

// entropy is the randomness source for the random component. It is a variable
// so tests can substitute a deterministic reader.
var entropy io.Reader = rand.Reader

// monotonic tracks the highest timestamp issued, so that a clock stepping
// backwards cannot mint an ID that sorts before one already issued.
var monotonic struct {
	sync.Mutex
	lastMS int64
	used   bool
}

// New returns a new ID for the current time.
func New() (ID, error) { return NewAt(time.Now(), entropy) }

// MustNew returns a new ID, panicking if the entropy source fails.
func MustNew() ID {
	id, err := New()
	if err != nil {
		panic(fmt.Sprintf("message: cannot generate id: %v", err))
	}
	return id
}

// NewAt returns a new ID stamped with t, reading the random component from src.
// A nil src uses the package entropy source.
func NewAt(t time.Time, src io.Reader) (ID, error) {
	if src == nil {
		src = entropy
	}

	ms := t.UnixMilli()

	monotonic.Lock()
	if monotonic.used && ms < monotonic.lastMS {
		ms = monotonic.lastMS
	}
	monotonic.lastMS = ms
	monotonic.used = true
	monotonic.Unlock()

	if ms < 0 || ms > maxTime {
		return "", fmt.Errorf("message: timestamp %s is outside the representable range", t.UTC())
	}

	var b [6 + entropySize]byte
	b[0] = byte(ms >> 40)
	b[1] = byte(ms >> 32)
	b[2] = byte(ms >> 24)
	b[3] = byte(ms >> 16)
	b[4] = byte(ms >> 8)
	b[5] = byte(ms)

	if _, err := io.ReadFull(src, b[6:]); err != nil {
		return "", fmt.Errorf("message: cannot read entropy: %w", err)
	}

	return encode(b), nil
}

// encode renders 16 bytes as 26 Crockford base32 characters.
func encode(b [16]byte) ID {
	var out [encodedSize]byte

	// Timestamp: 50 bits, of which the leading 2 are padding.
	out[0] = alphabet[(b[0]&224)>>5]
	out[1] = alphabet[b[0]&31]
	out[2] = alphabet[(b[1]&248)>>3]
	out[3] = alphabet[((b[1]&7)<<2)|((b[2]&192)>>6)]
	out[4] = alphabet[(b[2]&62)>>1]
	out[5] = alphabet[((b[2]&1)<<4)|((b[3]&240)>>4)]
	out[6] = alphabet[((b[3]&15)<<1)|((b[4]&128)>>7)]
	out[7] = alphabet[(b[4]&124)>>2]
	out[8] = alphabet[((b[4]&3)<<3)|((b[5]&224)>>5)]
	out[9] = alphabet[b[5]&31]

	// Randomness: 80 bits.
	out[10] = alphabet[(b[6]&248)>>3]
	out[11] = alphabet[((b[6]&7)<<2)|((b[7]&192)>>6)]
	out[12] = alphabet[(b[7]&62)>>1]
	out[13] = alphabet[((b[7]&1)<<4)|((b[8]&240)>>4)]
	out[14] = alphabet[((b[8]&15)<<1)|((b[9]&128)>>7)]
	out[15] = alphabet[(b[9]&124)>>2]
	out[16] = alphabet[((b[9]&3)<<3)|((b[10]&224)>>5)]
	out[17] = alphabet[b[10]&31]
	out[18] = alphabet[(b[11]&248)>>3]
	out[19] = alphabet[((b[11]&7)<<2)|((b[12]&192)>>6)]
	out[20] = alphabet[(b[12]&62)>>1]
	out[21] = alphabet[((b[12]&1)<<4)|((b[13]&240)>>4)]
	out[22] = alphabet[((b[13]&15)<<1)|((b[14]&128)>>7)]
	out[23] = alphabet[(b[14]&124)>>2]
	out[24] = alphabet[((b[14]&3)<<3)|((b[15]&224)>>5)]
	out[25] = alphabet[b[15]&31]

	return ID(out[:])
}

// Parse validates s and returns it as an ID.
//
// Parsing accepts lowercase input and the Crockford alias characters, but does
// not normalise: a lowercase ID remains lowercase and does not compare equal to
// its canonical form. Use Canonical before comparing or storing an ID that came
// from an untrusted source.
func Parse(s string) (ID, error) {
	if len(s) != encodedSize {
		return "", fmt.Errorf("%w: length %d, want %d", ErrInvalidID, len(s), encodedSize)
	}
	// The leading 2 bits of the encoding are unused, so the first character may
	// take only the 8 values that fit in 2 bits. Rejecting the rest keeps a typo
	// from decoding to a different timestamp.
	if v := decTable[s[0]]; v < 0 || v > 7 {
		return "", fmt.Errorf("%w: %q is not a valid ULID", ErrInvalidID, s)
	}
	for i := 1; i < encodedSize; i++ {
		if decTable[s[i]] < 0 {
			return "", fmt.Errorf("%w: %q is not a valid ULID", ErrInvalidID, s)
		}
	}
	return ID(s), nil
}

// Canonical returns the uppercase form of the ID. Unrecognised bytes are passed
// through unchanged, so an invalid ID stays invalid and still fails Valid.
func (id ID) Canonical() ID {
	out := make([]byte, 0, len(id))
	for i := 0; i < len(id); i++ {
		if v := decTable[id[i]]; v >= 0 {
			out = append(out, alphabet[v])
		} else {
			out = append(out, id[i])
		}
	}
	return ID(out)
}

// Valid reports whether the ID is well-formed. Values from untrusted sources
// must be checked before reaching a query: on MySQL a malformed ID matches
// nothing, turning invalid input into "not found".
func (id ID) Valid() bool {
	_, err := Parse(string(id))
	return err == nil
}

// IsZero reports whether the ID is empty. The zero ID is never valid and never
// appears in storage.
func (id ID) IsZero() bool { return id == "" }

// String returns the ID as a string.
func (id ID) String() string { return string(id) }

// Time returns the timestamp encoded in the ID, for display and debugging.
// It is the generating host's wall clock and is subject to skew between
// replicas; ordering and retention must use the stored created_at column.
//
// A malformed ID yields the zero time.
func (id ID) Time() time.Time {
	if len(id) != encodedSize {
		return time.Time{}
	}
	var ms uint64
	for i := 0; i < 10; i++ {
		v := decTable[id[i]]
		if v < 0 {
			return time.Time{}
		}
		ms = ms<<5 | uint64(v)
	}
	ms &= maxTime
	return time.UnixMilli(int64(ms)).UTC()
}

// MarshalText implements encoding.TextMarshaler, so the ID encodes identically
// in JSON, YAML, TOML, and as a map key.
func (id ID) MarshalText() ([]byte, error) {
	if id == "" {
		return []byte{}, nil
	}
	if !id.Valid() {
		return nil, fmt.Errorf("%w: %q", ErrInvalidID, id)
	}
	return []byte(id), nil
}

// UnmarshalText implements encoding.TextUnmarshaler, validating at the boundary
// so that no code downstream of a decoded ID needs to re-check it.
func (id *ID) UnmarshalText(b []byte) error {
	if len(b) == 0 {
		*id = ""
		return nil
	}
	parsed, err := Parse(string(b))
	if err != nil {
		return err
	}
	*id = parsed
	return nil
}

// Bytes returns the 16 bytes the ID encodes, for storage in a BINARY(16)
// column. Only the MySQL store needs this.
func (id ID) Bytes() ([16]byte, error) {
	var out [16]byte
	if !id.Valid() {
		return out, fmt.Errorf("%w: %q", ErrInvalidID, id)
	}

	// Shift all 26 characters into a 128-bit register, 5 bits at a time. The
	// first character contributes 3 bits, since the top 2 bits of the 130-bit
	// encoding are padding; masking them off means exactly 128 bits are shifted
	// in and nothing is lost off the top.
	var hi, lo uint64
	for i := 0; i < encodedSize; i++ {
		v := uint64(decTable[id[i]])
		if i == 0 {
			v &= 7
		}
		hi = hi<<5 | lo>>59
		lo = lo<<5 | v
	}

	binary.BigEndian.PutUint64(out[0:8], hi)
	binary.BigEndian.PutUint64(out[8:16], lo)
	return out, nil
}
