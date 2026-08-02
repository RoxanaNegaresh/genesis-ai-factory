package crypto

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/genesis-ai-factory/control-plane/internal/domain"
)

// JWTIssuer mints HS256 access tokens.
//
// We implement JWT directly rather than pulling a dependency: the surface we
// need is one algorithm and five claims, and hand-rolling it keeps the
// algorithm fixed (no "alg: none" confusion class of bug, because we never read
// the algorithm from the token to decide how to verify it).
type JWTIssuer struct {
	secret   []byte
	issuer   string
	audience string
	now      func() time.Time
}

// NewJWTIssuer constructs an issuer.
func NewJWTIssuer(secret string, now func() time.Time) *JWTIssuer {
	if now == nil {
		now = time.Now
	}
	return &JWTIssuer{
		secret:   []byte(secret),
		issuer:   "genesis-ai-factory",
		audience: "genesis-desktop",
		now:      now,
	}
}

type jwtHeader struct {
	Alg string `json:"alg"`
	Typ string `json:"typ"`
}

type jwtClaims struct {
	Sub   string `json:"sub"`
	Email string `json:"email"`
	Role  string `json:"role"`
	Iss   string `json:"iss"`
	Aud   string `json:"aud"`
	Jti   string `json:"jti"`
	Iat   int64  `json:"iat"`
	Nbf   int64  `json:"nbf"`
	Exp   int64  `json:"exp"`
}

// Issue mints a signed access token for a principal.
func (j *JWTIssuer) Issue(p domain.Principal, ttl time.Duration) (string, time.Time, error) {
	if p.UserID.IsZero() {
		return "", time.Time{}, fmt.Errorf("principal must have a user id")
	}
	now := j.now().UTC()
	exp := now.Add(ttl)

	header, err := json.Marshal(jwtHeader{Alg: "HS256", Typ: "JWT"})
	if err != nil {
		return "", time.Time{}, err
	}
	claims, err := json.Marshal(jwtClaims{
		Sub:   p.UserID.String(),
		Email: p.Email,
		Role:  string(p.Role),
		Iss:   j.issuer,
		Aud:   j.audience,
		Jti:   domain.NewID().String(),
		Iat:   now.Unix(),
		Nbf:   now.Add(-30 * time.Second).Unix(), // tolerate small clock skew
		Exp:   exp.Unix(),
	})
	if err != nil {
		return "", time.Time{}, err
	}

	signing := b64(header) + "." + b64(claims)
	return signing + "." + b64(j.sign(signing)), exp, nil
}

// Parse validates a token's signature and temporal claims and returns the
// principal it asserts.
func (j *JWTIssuer) Parse(token string) (domain.Principal, error) {
	var p domain.Principal

	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return p, domain.Unauthorized("malformed token")
	}

	signing := parts[0] + "." + parts[1]
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return p, domain.Unauthorized("malformed token signature")
	}
	// Verify before parsing anything from the payload: never trust unverified
	// bytes, including the header's algorithm field.
	if subtle.ConstantTimeCompare(sig, j.sign(signing)) != 1 {
		return p, domain.Unauthorized("invalid token signature")
	}

	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return p, domain.Unauthorized("malformed token payload")
	}
	var c jwtClaims
	if err := json.Unmarshal(raw, &c); err != nil {
		return p, domain.Unauthorized("malformed token claims")
	}

	now := j.now().UTC().Unix()
	switch {
	case c.Exp <= now:
		return p, domain.Unauthorized("token expired")
	case c.Nbf > now:
		return p, domain.Unauthorized("token not yet valid")
	case c.Iss != j.issuer:
		return p, domain.Unauthorized("unexpected token issuer")
	case c.Aud != j.audience:
		return p, domain.Unauthorized("unexpected token audience")
	}

	id, err := domain.ParseID(c.Sub)
	if err != nil {
		return p, domain.Unauthorized("invalid subject")
	}
	role := domain.Role(c.Role)
	if !role.Valid() {
		return p, domain.Unauthorized("invalid role claim")
	}
	return domain.Principal{UserID: id, Email: c.Email, Role: role}, nil
}

func (j *JWTIssuer) sign(msg string) []byte {
	m := hmac.New(sha256.New, j.secret)
	m.Write([]byte(msg))
	return m.Sum(nil)
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// NewOpaqueToken returns a high-entropy refresh token and its storage hash.
// Only the hash is persisted, so a database leak does not yield live sessions.
func NewOpaqueToken() (token string, hash string, err error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", "", fmt.Errorf("read entropy: %w", err)
	}
	token = base64.RawURLEncoding.EncodeToString(buf)
	return token, HashToken(token), nil
}

// HashToken computes the storage hash for an opaque token.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
