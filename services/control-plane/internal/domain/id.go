package domain

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// ID is a UUIDv7 rendered in canonical textual form.
//
// UUIDv7 embeds a millisecond Unix timestamp in the high bits, which makes IDs
// time-sortable. That gives us index locality on primary keys (append-only
// B-tree inserts instead of random ones) while remaining globally unique and
// non-enumerable. IDs are generated in the application layer so that SQLite and
// PostgreSQL behave identically.
type ID string

// Nil is the zero identifier.
const Nil ID = ""

var (
	idMu       sync.Mutex
	lastMillis int64
	lastSeq    uint16
)

// NewID returns a fresh UUIDv7.
func NewID() ID {
	return newIDAt(time.Now())
}

func newIDAt(t time.Time) ID {
	var b [16]byte

	ms := t.UTC().UnixMilli()

	// Monotonic counter guarantees ordering for IDs minted within the same
	// millisecond, which matters because we sort by ID in several queries.
	idMu.Lock()
	if ms == lastMillis {
		lastSeq++
	} else {
		lastMillis = ms
		lastSeq = 0
	}
	seq := lastSeq
	idMu.Unlock()

	// 48-bit millisecond timestamp in b[0:6], then version nibble + 12-bit
	// intra-millisecond counter in b[6:8].
	binary.BigEndian.PutUint64(b[0:8], uint64(ms)<<16)
	binary.BigEndian.PutUint16(b[6:8], 0x7000|(seq&0x0fff))

	if _, err := rand.Read(b[8:]); err != nil {
		// crypto/rand failure is unrecoverable; falling back to a predictable
		// value would silently weaken uniqueness guarantees.
		panic(fmt.Sprintf("genesis: entropy source unavailable: %v", err))
	}
	b[8] = (b[8] & 0x3f) | 0x80 // RFC 4122 variant

	return ID(format(b))
}

func format(b [16]byte) string {
	var dst [36]byte
	hex.Encode(dst[0:8], b[0:4])
	dst[8] = '-'
	hex.Encode(dst[9:13], b[4:6])
	dst[13] = '-'
	hex.Encode(dst[14:18], b[6:8])
	dst[18] = '-'
	hex.Encode(dst[19:23], b[8:10])
	dst[23] = '-'
	hex.Encode(dst[24:36], b[10:16])
	return string(dst[:])
}

// ErrInvalidID is returned when a string cannot be parsed as an identifier.
var ErrInvalidID = errors.New("invalid id")

// ParseID validates and normalises an identifier coming from an untrusted
// source (URL path, request body, CLI argument).
func ParseID(s string) (ID, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	if len(s) != 36 {
		return Nil, ErrInvalidID
	}
	for i, c := range s {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return Nil, ErrInvalidID
			}
		default:
			if !isHex(byte(c)) {
				return Nil, ErrInvalidID
			}
		}
	}
	return ID(s), nil
}

func isHex(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')
}

// String implements fmt.Stringer.
func (i ID) String() string { return string(i) }

// IsZero reports whether the identifier is unset.
func (i ID) IsZero() bool { return i == Nil }

// Time extracts the creation timestamp embedded in a UUIDv7.
func (i ID) Time() (time.Time, bool) {
	if len(i) != 36 {
		return time.Time{}, false
	}
	raw, err := hex.DecodeString(string(i[0:8]) + string(i[9:13]))
	if err != nil {
		return time.Time{}, false
	}
	ms := int64(binary.BigEndian.Uint32(raw[0:4]))<<16 | int64(binary.BigEndian.Uint16(raw[4:6]))
	return time.UnixMilli(ms).UTC(), true
}
