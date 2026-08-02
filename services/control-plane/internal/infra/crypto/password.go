// Package crypto provides password hashing and token issuance. It is an
// infrastructure adapter: use cases depend on port.Hasher / port.TokenIssuer,
// never on this package directly.
package crypto

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"runtime"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Argon2Params are the cost parameters. They are encoded into every hash, so
// they can be raised later and existing users transparently upgraded on their
// next successful login (see the needsRehash return of Verify).
type Argon2Params struct {
	Time    uint32
	Memory  uint32 // KiB
	Threads uint8
	KeyLen  uint32
	SaltLen uint32
}

// DefaultParams targets roughly 100 ms on a modern desktop core, which is the
// usual balance between login latency and offline-cracking cost.
func DefaultParams() Argon2Params {
	threads := uint8(2)
	if n := runtime.NumCPU(); n < 2 {
		threads = 1
	}
	return Argon2Params{
		Time:    3,
		Memory:  64 * 1024,
		Threads: threads,
		KeyLen:  32,
		SaltLen: 16,
	}
}

// TestParams are deliberately weak; only for tests, where 64 MiB per hash would
// dominate the suite runtime.
func TestParams() Argon2Params {
	return Argon2Params{Time: 1, Memory: 8 * 1024, Threads: 1, KeyLen: 32, SaltLen: 16}
}

// Argon2Hasher implements port.Hasher using Argon2id.
type Argon2Hasher struct {
	params Argon2Params
}

// NewArgon2Hasher builds a hasher with the given cost parameters.
func NewArgon2Hasher(p Argon2Params) *Argon2Hasher {
	if p.KeyLen == 0 {
		p = DefaultParams()
	}
	return &Argon2Hasher{params: p}
}

var (
	// ErrInvalidHash indicates a stored hash that this version cannot parse.
	ErrInvalidHash = errors.New("invalid password hash format")
	// ErrIncompatibleVersion indicates a hash produced by a newer Argon2.
	ErrIncompatibleVersion = errors.New("incompatible argon2 version")
)

// Hash returns a PHC-formatted Argon2id hash.
func (h *Argon2Hasher) Hash(password string) (string, error) {
	if password == "" {
		return "", errors.New("password must not be empty")
	}
	salt := make([]byte, h.params.SaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("read entropy: %w", err)
	}
	key := argon2.IDKey([]byte(password), salt, h.params.Time, h.params.Memory, h.params.Threads, h.params.KeyLen)

	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, h.params.Memory, h.params.Time, h.params.Threads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	), nil
}

// Verify checks a password against an encoded hash in constant time. It also
// reports whether the stored hash used weaker parameters than the current
// policy, so the caller can silently upgrade it.
func (h *Argon2Hasher) Verify(password, encoded string) (bool, bool, error) {
	p, salt, want, err := decodeHash(encoded)
	if err != nil {
		return false, false, err
	}
	got := argon2.IDKey([]byte(password), salt, p.Time, p.Memory, p.Threads, uint32(len(want)))
	if subtle.ConstantTimeCompare(got, want) != 1 {
		return false, false, nil
	}
	needsRehash := p.Time < h.params.Time || p.Memory < h.params.Memory || uint32(len(want)) < h.params.KeyLen
	return true, needsRehash, nil
}

func decodeHash(encoded string) (Argon2Params, []byte, []byte, error) {
	var p Argon2Params
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return p, nil, nil, ErrInvalidHash
	}

	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return p, nil, nil, ErrInvalidHash
	}
	if version != argon2.Version {
		return p, nil, nil, ErrIncompatibleVersion
	}
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.Memory, &p.Time, &p.Threads); err != nil {
		return p, nil, nil, ErrInvalidHash
	}

	salt, err := base64.RawStdEncoding.Strict().DecodeString(parts[4])
	if err != nil {
		return p, nil, nil, ErrInvalidHash
	}
	key, err := base64.RawStdEncoding.Strict().DecodeString(parts[5])
	if err != nil {
		return p, nil, nil, ErrInvalidHash
	}
	p.SaltLen = uint32(len(salt))
	p.KeyLen = uint32(len(key))
	return p, salt, key, nil
}
