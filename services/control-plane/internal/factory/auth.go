package factory

import (
	"fmt"
	"strings"
)

// Authentication for generated products.
//
// Until v1.2 the generator emitted a `users` table, a JWT_SECRET requirement
// and a CORS header advertising `Authorization`, but nothing that issued or
// checked a token. Every resource route was public. The contract test even
// listed the routes that "require auth" and skipped all of them.
//
// This file closes that. The design decisions worth stating:
//
//   - **Argon2id, not bcrypt.** bcrypt truncates at 72 bytes and has no memory
//     hardness, so a GPU attacks it cheaply. Argon2id is the current
//     recommendation and is in golang.org/x/crypto, which the generated module
//     already depends on.
//   - **Access token short, refresh token long and rotating.** A stolen access
//     token expires on its own; a stolen refresh token is detected because
//     reuse of a rotated token revokes the whole family.
//   - **The refresh token is stored hashed.** A database leak must not hand
//     the attacker working sessions.
//   - **Login does not say which half was wrong.** "No account with that
//     email" is an account-enumeration oracle.

// authEntity reports whether a blueprint carries the User entity that
// authentication is generated against.
func authEntity(bp Blueprint) (Entity, bool) {
	for _, e := range bp.Entities {
		if e.Name == "User" {
			return e, true
		}
	}
	return Entity{}, false
}

// userRoles returns the role values declared on the User entity, which become
// the vocabulary of the authorisation middleware.
func userRoles(e Entity) []string {
	for _, f := range e.Fields {
		if f.Name == "role" && len(f.Enum) > 0 {
			return f.Enum
		}
	}
	return []string{"admin", "member"}
}

// backendAuthDomain emits the identity entity and its invariants.
func backendAuthDomain(e Entity) string {
	roles := userRoles(e)
	quoted := make([]string, len(roles))
	for i, r := range roles {
		quoted[i] = fmt.Sprintf("%q", r)
	}

	var sb strings.Builder
	sb.WriteString(`package domain

import (
	"strings"
	"time"
)

// User is an authenticated principal.
//
// PasswordHash carries a json:"-" tag. A struct that is returned from a
// handler and also holds a credential will eventually leak it: someone adds a
// new endpoint, marshals the whole record, and the hash goes out over the
// wire. The tag makes that impossible rather than merely discouraged.
type User struct {
	ID           string     ` + "`json:\"id\"`" + `
	CreatedAt    time.Time  ` + "`json:\"created_at\"`" + `
	UpdatedAt    time.Time  ` + "`json:\"updated_at\"`" + `
	Email        string     ` + "`json:\"email\"`" + `
	DisplayName  string     ` + "`json:\"display_name\"`" + `
	Role         string     ` + "`json:\"role\"`" + `
	PasswordHash string     ` + "`json:\"-\"`" + `
	DeletedAt    *time.Time ` + "`json:\"deleted_at,omitempty\"`" + `
}

`)

	fmt.Fprintf(&sb, "// Roles enumerates every role this product recognises.\nvar Roles = []string{%s}\n\n", strings.Join(quoted, ", "))

	sb.WriteString(`// ValidRole reports whether v is a role this product recognises.
func ValidRole(v string) bool {
	for _, role := range Roles {
		if role == v {
			return true
		}
	}
	return false
}

// NormaliseEmail lower-cases and trims an address.
//
// Addresses are compared case-insensitively because users do not remember
// which capitalisation they signed up with, and a unique index on a
// case-sensitive column would happily accept both spellings as two accounts.
func NormaliseEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// Validate checks the invariants of a User before persistence.
func (u *User) Validate() error {
	v := NewValidation()
	if strings.TrimSpace(u.Email) == "" {
		v.Add("email", "is required")
	} else if !strings.Contains(u.Email, "@") || strings.HasPrefix(u.Email, "@") ||
		strings.HasSuffix(u.Email, "@") {
		v.Add("email", "is not a valid address")
	}
	if strings.TrimSpace(u.DisplayName) == "" {
		v.Add("display_name", "is required")
	}
	if !ValidRole(u.Role) {
		v.Add("role", "is not an allowed value")
	}
	if u.PasswordHash == "" {
		v.Add("password", "is required")
	}
	return v.Err()
}

// Archived reports whether the account has been soft deleted.
func (u *User) Archived() bool { return u.DeletedAt != nil }

// Session is a refresh-token family member.
//
// Rotation means each refresh mints a replacement and retires its predecessor.
// If a retired token is presented again, either the client is buggy or a token
// was stolen and replayed; neither is safe to serve, so the entire family is
// revoked and the real user is forced to sign in again.
type Session struct {
	ID        string
	UserID    string
	FamilyID  string
	TokenHash string
	ExpiresAt time.Time
	CreatedAt time.Time
	RevokedAt *time.Time
	UsedAt    *time.Time
}

// Active reports whether a session may still be exchanged.
func (s *Session) Active(now time.Time) bool {
	return s.RevokedAt == nil && s.UsedAt == nil && now.Before(s.ExpiresAt)
}

// PasswordPolicy is the minimum a password must satisfy.
//
// Length is the only requirement. Composition rules ("one uppercase, one
// digit, one symbol") measurably reduce entropy by pushing users toward
// predictable patterns like Password1!, and every current guideline has
// dropped them in favour of a length floor and a breach-list check.
const MinPasswordLength = 12

// ValidatePassword checks a plaintext password before it is hashed.
func ValidatePassword(password string) error {
	v := NewValidation()
	if len([]rune(password)) < MinPasswordLength {
		v.Add("password", "must be at least 12 characters")
	}
	if len(password) > 1024 {
		// Unbounded input into a deliberately slow hash is a denial-of-service
		// vector: the work factor is the point, so the input must be capped.
		v.Add("password", "must be at most 1024 bytes")
	}
	return v.Err()
}
`)
	return sb.String()
}

// backendAuthPort emits the inner-layer contracts for identity storage.
func backendAuthPort(module string) string {
	return fmt.Sprintf(`package port

import (
	"context"
	"time"

	%q
)

// UserRepository persists identities.
type UserRepository interface {
	Create(ctx context.Context, u *domain.User) error
	ByID(ctx context.Context, id string) (*domain.User, error)
	ByEmail(ctx context.Context, email string) (*domain.User, error)
	UpdatePassword(ctx context.Context, id, passwordHash string) error
}

// SessionRepository persists refresh-token families.
type SessionRepository interface {
	Create(ctx context.Context, s *domain.Session) error
	ByTokenHash(ctx context.Context, hash string) (*domain.Session, error)
	MarkUsed(ctx context.Context, id string, at time.Time) error
	RevokeFamily(ctx context.Context, familyID string, at time.Time) error
	DeleteExpired(ctx context.Context, before time.Time) (int64, error)
}

// PasswordHasher turns a plaintext password into a verifiable hash.
//
// It is a port rather than a direct call so the cost parameters can be lowered
// in tests. A correctly configured hasher takes tens of milliseconds by
// design, which is negligible in production and unbearable across a few
// hundred test cases.
type PasswordHasher interface {
	Hash(password string) (string, error)
	Verify(password, encoded string) (bool, error)
}

// TokenIssuer mints and verifies access tokens.
type TokenIssuer interface {
	Issue(userID, role string, ttl time.Duration) (string, error)
	Verify(token string) (Claims, error)
}

// Claims is the verified content of an access token.
type Claims struct {
	UserID    string
	Role      string
	ExpiresAt time.Time
}
`, module+"/internal/domain")
}

// backendAuthCrypto emits Argon2id password hashing and HMAC-signed tokens.
//
// The JWT is hand-rolled rather than pulled from a library. It is roughly
// eighty lines for the HS256 subset actually used here, it removes a
// dependency from generated projects, and — the real reason — the historical
// JWT vulnerabilities are all in the flexible parts: `alg: none`, algorithm
// confusion between HMAC and RSA, and unverified `kid` lookups. A verifier
// that accepts exactly one algorithm cannot be confused about which one it is.
func backendAuthCrypto(module string) string {
	return fmt.Sprintf(`// Package authcrypto implements password hashing and token signing.
package authcrypto

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"golang.org/x/crypto/argon2"

	%q
)

// Argon2id parameters.
//
// 64 MiB and one pass over three lanes is the configuration RFC 9106 gives for
// the memory-constrained case. Memory is what defeats GPU attacks: a card with
// thousands of cores cannot give each one 64 MiB, so the parallelism advantage
// that makes bcrypt cheap to attack does not materialise.
const (
	argonTime    = 1
	argonMemory  = 64 * 1024
	argonThreads = 3
	argonKeyLen  = 32
	argonSaltLen = 16
)

// Argon2Hasher implements port.PasswordHasher.
type Argon2Hasher struct{}

// NewArgon2Hasher constructs the hasher.
func NewArgon2Hasher() *Argon2Hasher { return &Argon2Hasher{} }

// Hash derives an encoded Argon2id hash with a fresh random salt.
//
// The output is the standard PHC string, which embeds the parameters. Storing
// them alongside the digest is what makes the cost factor upgradable: an old
// hash still verifies with its own parameters after the defaults are raised.
func (h *Argon2Hasher) Hash(password string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("read salt: %%w", err)
	}
	digest := argon2.IDKey([]byte(password), salt,
		argonTime, argonMemory, argonThreads, argonKeyLen)

	return fmt.Sprintf("$argon2id$v=%%d$m=%%d,t=%%d,p=%%d$%%s$%%s",
		argon2.Version, argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(digest)), nil
}

// Verify reports whether password matches an encoded hash.
func (h *Argon2Hasher) Verify(password, encoded string) (bool, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false, errors.New("the stored hash is not an argon2id hash")
	}

	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%%d", &version); err != nil {
		return false, errors.New("the stored hash has no version")
	}
	if version != argon2.Version {
		return false, fmt.Errorf("unsupported argon2 version %%d", version)
	}

	var memory uint32
	var iterations uint32
	var parallelism uint8
	if _, err := fmt.Sscanf(parts[3], "m=%%d,t=%%d,p=%%d",
		&memory, &iterations, &parallelism); err != nil {
		return false, errors.New("the stored hash has malformed parameters")
	}

	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false, errors.New("the stored salt is not valid base64")
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return false, errors.New("the stored digest is not valid base64")
	}

	// Verify with the parameters the hash was created with, not the current
	// defaults, so raising the cost factor does not lock out existing users.
	got := argon2.IDKey([]byte(password), salt,
		iterations, memory, parallelism, uint32(len(want)))

	// Constant time: a byte-by-byte comparison leaks how much of the digest
	// matched, which is enough to reconstruct it one byte at a time.
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// HMACIssuer implements port.TokenIssuer with HS256.
type HMACIssuer struct {
	secret []byte
}

// NewHMACIssuer constructs the issuer.
func NewHMACIssuer(secret string) *HMACIssuer {
	return &HMACIssuer{secret: []byte(secret)}
}

type jwtHeader struct {
	Alg string `+"`json:\"alg\"`"+`
	Typ string `+"`json:\"typ\"`"+`
}

type jwtPayload struct {
	Sub  string `+"`json:\"sub\"`"+`
	Role string `+"`json:\"role\"`"+`
	Exp  int64  `+"`json:\"exp\"`"+`
	Iat  int64  `+"`json:\"iat\"`"+`
	Jti  string `+"`json:\"jti\"`"+`
}

// Issue mints a signed access token.
func (i *HMACIssuer) Issue(userID, role string, ttl time.Duration) (string, error) {
	now := time.Now().UTC()
	header, err := json.Marshal(jwtHeader{Alg: "HS256", Typ: "JWT"})
	if err != nil {
		return "", err
	}
	// A unique token identifier. Without it two tokens minted for the same
	// subject within the same second are byte-identical, because iat and exp
	// have only second precision. That makes rotation unobservable to a
	// client and makes any per-token audit record ambiguous.
	nonce := make([]byte, 12)
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("read token nonce: %%w", err)
	}

	payload, err := json.Marshal(jwtPayload{
		Sub:  userID,
		Role: role,
		Exp:  now.Add(ttl).Unix(),
		Iat:  now.Unix(),
		Jti:  base64.RawURLEncoding.EncodeToString(nonce),
	})
	if err != nil {
		return "", err
	}

	signing := b64(header) + "." + b64(payload)
	return signing + "." + b64(i.sign(signing)), nil
}

// Verify checks a token's signature and expiry.
func (i *HMACIssuer) Verify(token string) (port.Claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return port.Claims{}, errors.New("the token is malformed")
	}

	// Signature first. Parsing untrusted claims before verifying them is how
	// alg:none and algorithm-confusion attacks get a foothold.
	signing := parts[0] + "." + parts[1]
	want := i.sign(signing)
	got, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return port.Claims{}, errors.New("the token signature is not valid base64")
	}
	if !hmac.Equal(got, want) {
		return port.Claims{}, errors.New("the token signature does not verify")
	}

	// The header is checked after the signature, so an attacker cannot use it
	// to steer verification. HS256 is the only algorithm accepted; there is no
	// negotiation to confuse.
	rawHeader, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return port.Claims{}, errors.New("the token header is not valid base64")
	}
	var header jwtHeader
	if err := json.Unmarshal(rawHeader, &header); err != nil {
		return port.Claims{}, errors.New("the token header is not valid JSON")
	}
	if header.Alg != "HS256" {
		return port.Claims{}, fmt.Errorf("unsupported token algorithm %%q", header.Alg)
	}

	rawPayload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return port.Claims{}, errors.New("the token payload is not valid base64")
	}
	var payload jwtPayload
	if err := json.Unmarshal(rawPayload, &payload); err != nil {
		return port.Claims{}, errors.New("the token payload is not valid JSON")
	}

	expiry := time.Unix(payload.Exp, 0).UTC()
	if time.Now().UTC().After(expiry) {
		return port.Claims{}, errors.New("the token has expired")
	}
	if payload.Sub == "" {
		return port.Claims{}, errors.New("the token has no subject")
	}

	return port.Claims{UserID: payload.Sub, Role: payload.Role, ExpiresAt: expiry}, nil
}

func (i *HMACIssuer) sign(signing string) []byte {
	mac := hmac.New(sha256.New, i.secret)
	mac.Write([]byte(signing))
	return mac.Sum(nil)
}

func b64(raw []byte) string { return base64.RawURLEncoding.EncodeToString(raw) }

// NewRefreshToken returns a random opaque token and its storage hash.
//
// The refresh token is opaque rather than a JWT because it must be revocable,
// and a self-contained token cannot be revoked without a lookup — at which
// point being self-contained bought nothing.
//
// Only the hash is stored. A database leak then yields no usable sessions.
// SHA-256 is right here where Argon2id is right for passwords: this input is
// 256 bits of entropy from a CSPRNG, so brute force is infeasible regardless,
// and refresh happens on a hot path where a deliberately slow hash would hurt.
func NewRefreshToken() (token string, hash string, err error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", "", fmt.Errorf("read token: %%w", err)
	}
	token = base64.RawURLEncoding.EncodeToString(raw)
	return token, HashRefreshToken(token), nil
}

// HashRefreshToken derives the storage hash of a refresh token.
func HashRefreshToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
`, module+"/internal/port")
}

// backendAuthUseCase emits registration, login, refresh and logout.
func backendAuthUseCase(module string, defaultRole string) string {
	return fmt.Sprintf(`package usecase

import (
	"context"
	"errors"
	"time"

	%q
	%q
)

// Token lifetimes.
//
// The access token is short because it cannot be revoked: it is verified by
// signature alone, so the only bound on a stolen one is its expiry. The
// refresh token is long because it can be revoked, and rotation makes theft
// detectable.
const (
	AccessTokenTTL  = 15 * time.Minute
	RefreshTokenTTL = 30 * 24 * time.Hour
)

// AuthService implements registration and session management.
type AuthService struct {
	users    port.UserRepository
	sessions port.SessionRepository
	hasher   port.PasswordHasher
	tokens   port.TokenIssuer
	uow      port.UnitOfWork
	now      func() time.Time
}

// NewAuthService constructs the service.
func NewAuthService(
	users port.UserRepository,
	sessions port.SessionRepository,
	hasher port.PasswordHasher,
	tokens port.TokenIssuer,
	uow port.UnitOfWork,
) *AuthService {
	return &AuthService{
		users: users, sessions: sessions, hasher: hasher,
		tokens: tokens, uow: uow, now: func() time.Time { return time.Now().UTC() },
	}
}

// TokenPair is what a successful authentication returns.
type TokenPair struct {
	AccessToken  string    `+"`json:\"access_token\"`"+`
	RefreshToken string    `+"`json:\"refresh_token\"`"+`
	ExpiresAt    time.Time `+"`json:\"expires_at\"`"+`
	TokenType    string    `+"`json:\"token_type\"`"+`
}

// Register creates an account and signs it in.
func (s *AuthService) Register(ctx context.Context, email, displayName, password string) (*domain.User, *TokenPair, error) {
	if err := domain.ValidatePassword(password); err != nil {
		return nil, nil, err
	}

	hash, err := s.hasher.Hash(password)
	if err != nil {
		return nil, nil, err
	}

	user := &domain.User{
		Email:        domain.NormaliseEmail(email),
		DisplayName:  displayName,
		Role:         %q,
		PasswordHash: hash,
	}
	if err := user.Validate(); err != nil {
		return nil, nil, err
	}

	var pair *TokenPair
	err = s.uow.Within(ctx, func(ctx context.Context) error {
		if err := s.users.Create(ctx, user); err != nil {
			return err
		}
		pair, err = s.issue(ctx, user, "")
		return err
	})
	if err != nil {
		return nil, nil, err
	}
	return user, pair, nil
}

// Login exchanges credentials for a token pair.
//
// Every failure returns the same error. Distinguishing "no such account" from
// "wrong password" tells an attacker which addresses are registered, which is
// the first step of a credential-stuffing run.
func (s *AuthService) Login(ctx context.Context, email, password string) (*domain.User, *TokenPair, error) {
	invalid := domain.Unauthorized("invalid_credentials", "the email or password is incorrect")

	user, err := s.users.ByEmail(ctx, domain.NormaliseEmail(email))
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			// Hash anyway. Returning immediately makes a missing account
			// measurably faster than a wrong password, and that timing
			// difference is the enumeration oracle the uniform error was
			// meant to close.
			_, _ = s.hasher.Hash(password)
			return nil, nil, invalid
		}
		return nil, nil, err
	}

	ok, err := s.hasher.Verify(password, user.PasswordHash)
	if err != nil || !ok {
		return nil, nil, invalid
	}

	pair, err := s.issue(ctx, user, "")
	if err != nil {
		return nil, nil, err
	}
	return user, pair, nil
}

// Refresh rotates a refresh token, returning a new pair.
//
// Presenting an already-used token means the token was replayed: either the
// client is broken or it was stolen. Both are unsafe to serve, so the whole
// family is revoked and the legitimate user has to sign in again. That is the
// point — a silent theft becomes a visible logout.
func (s *AuthService) Refresh(ctx context.Context, refreshToken string) (*TokenPair, error) {
	invalid := domain.Unauthorized("invalid_refresh_token", "the refresh token is not valid")
	hash := hashRefresh(refreshToken)

	// Replay detection happens outside the rotation transaction, and this is
	// not incidental. Revoking the family and then returning an error from
	// inside Within would roll the revocation back with everything else: the
	// caller would see "all sessions have been revoked" while every stolen
	// token kept working. The revocation must commit on its own.
	session, err := s.sessions.ByTokenHash(ctx, hash)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return nil, invalid
		}
		return nil, err
	}

	now := s.now()
	if session.UsedAt != nil {
		if err := s.sessions.RevokeFamily(ctx, session.FamilyID, now); err != nil {
			return nil, err
		}
		return nil, domain.Unauthorized("refresh_token_reused",
			"this refresh token was already used; all sessions have been revoked")
	}

	var pair *TokenPair
	err = s.uow.Within(ctx, func(ctx context.Context) error {
		// Re-read inside the transaction. The check above was made on an
		// unlocked row, so between then and now another request could have
		// exchanged the same token. MarkUsed is the real guard — it updates
		// only while used_at is still NULL — but re-reading keeps the
		// not-active and revoked cases from reaching it at all.
		session, err := s.sessions.ByTokenHash(ctx, hash)
		if err != nil {
			if errors.Is(err, domain.ErrNotFound) {
				return invalid
			}
			return err
		}
		if !session.Active(s.now()) {
			return invalid
		}

		user, err := s.users.ByID(ctx, session.UserID)
		if err != nil {
			return invalid
		}
		if err := s.sessions.MarkUsed(ctx, session.ID, s.now()); err != nil {
			return err
		}

		pair, err = s.issue(ctx, user, session.FamilyID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return pair, nil
}

// Logout revokes the family a refresh token belongs to.
func (s *AuthService) Logout(ctx context.Context, refreshToken string) error {
	session, err := s.sessions.ByTokenHash(ctx, hashRefresh(refreshToken))
	if err != nil {
		// Logging out an unknown token is not an error worth reporting: the
		// caller wanted no session and has no session.
		if errors.Is(err, domain.ErrNotFound) {
			return nil
		}
		return err
	}
	return s.sessions.RevokeFamily(ctx, session.FamilyID, s.now())
}

// Me returns the authenticated principal.
func (s *AuthService) Me(ctx context.Context, userID string) (*domain.User, error) {
	return s.users.ByID(ctx, userID)
}

// issue mints an access token and a rotated refresh token.
func (s *AuthService) issue(ctx context.Context, user *domain.User, familyID string) (*TokenPair, error) {
	access, err := s.tokens.Issue(user.ID, user.Role, AccessTokenTTL)
	if err != nil {
		return nil, err
	}

	refresh, refreshHash, err := newRefresh()
	if err != nil {
		return nil, err
	}

	now := s.now()
	session := &domain.Session{
		UserID:    user.ID,
		FamilyID:  familyID,
		TokenHash: refreshHash,
		ExpiresAt: now.Add(RefreshTokenTTL),
	}
	if err := s.sessions.Create(ctx, session); err != nil {
		return nil, err
	}

	return &TokenPair{
		AccessToken:  access,
		RefreshToken: refresh,
		ExpiresAt:    now.Add(AccessTokenTTL),
		TokenType:    "Bearer",
	}, nil
}
`, module+"/internal/domain", module+"/internal/port", defaultRole)
}

// backendAuthTokenBridge emits the indirection that keeps usecase free of a
// direct dependency on the crypto package.
//
// The architecture rule is that usecase depends on port and domain only.
// Refresh-token hashing is a cryptographic detail, so it enters through a
// variable that the composition root sets, in the same spirit as a port but
// without an interface for two functions.
func backendAuthTokenBridge(module string) string {
	return `package usecase

// newRefresh and hashRefresh are supplied by the composition root so this
// package does not import the crypto implementation directly. They are
// variables rather than an interface because two free functions do not justify
// a type, and a test can substitute them without a mock.
var (
	newRefresh  func() (token string, hash string, err error)
	hashRefresh func(token string) string
)

// SetRefreshTokenFuncs installs the refresh-token primitives. The composition
// root calls this once at startup.
func SetRefreshTokenFuncs(
	generate func() (string, string, error),
	hash func(string) string,
) {
	newRefresh = generate
	hashRefresh = hash
}
`
}

// backendAuthRepository emits the PostgreSQL identity and session stores.
func backendAuthRepository(module string) string {
	return fmt.Sprintf(`package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	%q
)

// UserRepo implements port.UserRepository.
type UserRepo struct{ db *pgxpool.Pool }

// NewUserRepo constructs the repository.
func NewUserRepo(db *pgxpool.Pool) *UserRepo { return &UserRepo{db: db} }

func (r *UserRepo) q(ctx context.Context) Querier {
	if tx, ok := txFrom(ctx); ok {
		return tx
	}
	return r.db
}

// Create inserts an account.
func (r *UserRepo) Create(ctx context.Context, u *domain.User) error {
	const q = `+"`"+`INSERT INTO users (email, display_name, role, password_hash)
		VALUES ($1, $2, $3, $4)
		RETURNING id::text, created_at, updated_at`+"`"+`

	err := r.q(ctx).QueryRow(ctx, q, u.Email, u.DisplayName, u.Role, u.PasswordHash).
		Scan(&u.ID, &u.CreatedAt, &u.UpdatedAt)
	if err != nil {
		return wrapWriteError("user", err)
	}
	return nil
}

// ByID loads an account.
func (r *UserRepo) ByID(ctx context.Context, id string) (*domain.User, error) {
	const q = `+"`"+`SELECT id::text, created_at, updated_at, email, display_name, role, password_hash, deleted_at
		FROM users WHERE id = $1::uuid AND deleted_at IS NULL`+"`"+`

	u := &domain.User{}
	err := r.q(ctx).QueryRow(ctx, q, id).Scan(&u.ID, &u.CreatedAt, &u.UpdatedAt,
		&u.Email, &u.DisplayName, &u.Role, &u.PasswordHash, &u.DeletedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.NotFound("user")
	}
	if err != nil {
		return nil, wrapReadError("user", err)
	}
	return u, nil
}

// ByEmail loads an account by address.
//
// The comparison is on the normalised value the row was written with, so this
// is an index lookup rather than a scan with lower() applied per row.
func (r *UserRepo) ByEmail(ctx context.Context, email string) (*domain.User, error) {
	const q = `+"`"+`SELECT id::text, created_at, updated_at, email, display_name, role, password_hash, deleted_at
		FROM users WHERE email = $1 AND deleted_at IS NULL`+"`"+`

	u := &domain.User{}
	err := r.q(ctx).QueryRow(ctx, q, email).Scan(&u.ID, &u.CreatedAt, &u.UpdatedAt,
		&u.Email, &u.DisplayName, &u.Role, &u.PasswordHash, &u.DeletedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.NotFound("user")
	}
	if err != nil {
		return nil, wrapReadError("user", err)
	}
	return u, nil
}

// UpdatePassword replaces the stored credential.
func (r *UserRepo) UpdatePassword(ctx context.Context, id, passwordHash string) error {
	const q = `+"`"+`UPDATE users SET password_hash = $1, updated_at = now()
		WHERE id = $2::uuid AND deleted_at IS NULL`+"`"+`

	tag, err := r.q(ctx).Exec(ctx, q, passwordHash, id)
	if err != nil {
		return wrapWriteError("user", err)
	}
	if tag.RowsAffected() == 0 {
		return domain.NotFound("user")
	}
	return nil
}

// SessionRepo implements port.SessionRepository.
type SessionRepo struct{ db *pgxpool.Pool }

// NewSessionRepo constructs the repository.
func NewSessionRepo(db *pgxpool.Pool) *SessionRepo { return &SessionRepo{db: db} }

func (r *SessionRepo) q(ctx context.Context) Querier {
	if tx, ok := txFrom(ctx); ok {
		return tx
	}
	return r.db
}

// Create stores a refresh-token family member.
//
// An empty family identifier means this is the first token of a new family,
// and the row's own id becomes the family id. Doing that in SQL with COALESCE
// avoids a second round trip to write back what the database just generated.
func (r *SessionRepo) Create(ctx context.Context, s *domain.Session) error {
	const q = `+"`"+`INSERT INTO sessions (user_id, family_id, token_hash, expires_at)
		VALUES ($1::uuid, NULLIF($2, '')::uuid, $3, $4)
		RETURNING id::text, COALESCE(family_id, id)::text, created_at`+"`"+`

	err := r.q(ctx).QueryRow(ctx, q, s.UserID, s.FamilyID, s.TokenHash, s.ExpiresAt).
		Scan(&s.ID, &s.FamilyID, &s.CreatedAt)
	if err != nil {
		return wrapWriteError("session", err)
	}

	// A first-generation row stores NULL and reports its own id as the family.
	// Persisting that makes every later query uniform.
	if _, err := r.q(ctx).Exec(ctx,
		`+"`"+`UPDATE sessions SET family_id = $1::uuid WHERE id = $1::uuid AND family_id IS NULL`+"`"+`,
		s.ID); err != nil {
		return wrapWriteError("session", err)
	}
	return nil
}

// ByTokenHash loads a session by the hash of its refresh token.
func (r *SessionRepo) ByTokenHash(ctx context.Context, hash string) (*domain.Session, error) {
	const q = `+"`"+`SELECT id::text, user_id::text, COALESCE(family_id, id)::text,
		       token_hash, expires_at, created_at, revoked_at, used_at
		FROM sessions WHERE token_hash = $1`+"`"+`

	s := &domain.Session{}
	err := r.q(ctx).QueryRow(ctx, q, hash).Scan(&s.ID, &s.UserID, &s.FamilyID,
		&s.TokenHash, &s.ExpiresAt, &s.CreatedAt, &s.RevokedAt, &s.UsedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.NotFound("session")
	}
	if err != nil {
		return nil, wrapReadError("session", err)
	}
	return s, nil
}

// MarkUsed records that a refresh token has been exchanged.
//
// The WHERE clause requires used_at to still be NULL, so two concurrent
// refreshes with the same token cannot both succeed: the second updates zero
// rows. Checking in Go and then writing would leave a window between the two.
func (r *SessionRepo) MarkUsed(ctx context.Context, id string, at time.Time) error {
	const q = `+"`"+`UPDATE sessions SET used_at = $1
		WHERE id = $2::uuid AND used_at IS NULL`+"`"+`

	tag, err := r.q(ctx).Exec(ctx, q, at, id)
	if err != nil {
		return wrapWriteError("session", err)
	}
	if tag.RowsAffected() == 0 {
		return domain.Conflict("session_already_used", "this session was already exchanged")
	}
	return nil
}

// RevokeFamily revokes every session descended from one login.
func (r *SessionRepo) RevokeFamily(ctx context.Context, familyID string, at time.Time) error {
	const q = `+"`"+`UPDATE sessions SET revoked_at = $1
		WHERE COALESCE(family_id, id) = $2::uuid AND revoked_at IS NULL`+"`"+`

	if _, err := r.q(ctx).Exec(ctx, q, at, familyID); err != nil {
		return wrapWriteError("session", err)
	}
	return nil
}

// DeleteExpired removes sessions that can no longer be exchanged.
//
// Without this the table grows without bound: every login adds a row and
// nothing removes it. Run it from a scheduled job.
func (r *SessionRepo) DeleteExpired(ctx context.Context, before time.Time) (int64, error) {
	const q = `+"`"+`DELETE FROM sessions WHERE expires_at < $1`+"`"+`

	tag, err := r.q(ctx).Exec(ctx, q, before)
	if err != nil {
		return 0, fmt.Errorf("delete expired sessions: %%w", err)
	}
	return tag.RowsAffected(), nil
}
`, module+"/internal/domain")
}

// backendAuthMigration emits the sessions table.
func backendAuthMigration() string {
	return `
-- Refresh-token families.
--
-- family_id groups every token descended from one login, so detecting a
-- replayed token can revoke the whole chain rather than just the one presented.
CREATE TABLE sessions (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    user_id UUID NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    family_id UUID,
    token_hash TEXT NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    revoked_at TIMESTAMPTZ,
    used_at TIMESTAMPTZ
);

-- The refresh path looks a session up by hash on every call, so this index is
-- on the hot path. It is unique because two sessions sharing a token hash
-- would make revocation ambiguous.
CREATE UNIQUE INDEX ux_sessions_token_hash ON sessions (token_hash);
CREATE INDEX ix_sessions_family_id ON sessions (family_id);
CREATE INDEX ix_sessions_user_id ON sessions (user_id);
CREATE INDEX ix_sessions_expires_at ON sessions (expires_at);
`
}

// backendAuthHandler emits the authentication endpoints.
func backendAuthHandler(module string) string {
	return fmt.Sprintf(`package http

import (
	"strings"

	"github.com/gofiber/fiber/v2"

	%q
	%q
)

// AuthHandler exposes registration and session management over HTTP.
type AuthHandler struct {
	svc *usecase.AuthService
}

// NewAuthHandler constructs the handler.
func NewAuthHandler(svc *usecase.AuthService) *AuthHandler {
	return &AuthHandler{svc: svc}
}

// Register mounts the authentication routes. These are public by definition:
// a caller cannot present a token before obtaining one.
func (h *AuthHandler) Register(r fiber.Router) {
	g := r.Group("/auth")
	g.Post("/register", h.register)
	g.Post("/login", h.login)
	g.Post("/refresh", h.refresh)
	g.Post("/logout", h.logout)
}

// RegisterProtected mounts routes that require a valid access token.
func (h *AuthHandler) RegisterProtected(r fiber.Router) {
	r.Get("/auth/me", h.me)
}

type registerRequest struct {
	Email       string `+"`json:\"email\"`"+`
	DisplayName string `+"`json:\"display_name\"`"+`
	Password    string `+"`json:\"password\"`"+`
}

type loginRequest struct {
	Email    string `+"`json:\"email\"`"+`
	Password string `+"`json:\"password\"`"+`
}

type refreshRequest struct {
	RefreshToken string `+"`json:\"refresh_token\"`"+`
}

type authResponse struct {
	User   *domain.User       `+"`json:\"user\"`"+`
	Tokens *usecase.TokenPair `+"`json:\"tokens\"`"+`
}

func (h *AuthHandler) register(c *fiber.Ctx) error {
	var body registerRequest
	if err := c.BodyParser(&body); err != nil {
		return domain.Invalid("body_invalid", "request body is not valid JSON")
	}
	user, tokens, err := h.svc.Register(c.Context(), body.Email, body.DisplayName, body.Password)
	if err != nil {
		return err
	}
	return c.Status(fiber.StatusCreated).JSON(authResponse{User: user, Tokens: tokens})
}

func (h *AuthHandler) login(c *fiber.Ctx) error {
	var body loginRequest
	if err := c.BodyParser(&body); err != nil {
		return domain.Invalid("body_invalid", "request body is not valid JSON")
	}
	user, tokens, err := h.svc.Login(c.Context(), body.Email, body.Password)
	if err != nil {
		return err
	}
	return c.JSON(authResponse{User: user, Tokens: tokens})
}

func (h *AuthHandler) refresh(c *fiber.Ctx) error {
	var body refreshRequest
	if err := c.BodyParser(&body); err != nil {
		return domain.Invalid("body_invalid", "request body is not valid JSON")
	}
	if strings.TrimSpace(body.RefreshToken) == "" {
		return domain.Invalid("refresh_token_required", "a refresh token is required")
	}
	tokens, err := h.svc.Refresh(c.Context(), body.RefreshToken)
	if err != nil {
		return err
	}
	return c.JSON(tokens)
}

func (h *AuthHandler) logout(c *fiber.Ctx) error {
	var body refreshRequest
	if err := c.BodyParser(&body); err != nil {
		return domain.Invalid("body_invalid", "request body is not valid JSON")
	}
	if err := h.svc.Logout(c.Context(), body.RefreshToken); err != nil {
		return err
	}
	return c.SendStatus(fiber.StatusNoContent)
}

func (h *AuthHandler) me(c *fiber.Ctx) error {
	user, err := h.svc.Me(c.Context(), CurrentUserID(c))
	if err != nil {
		return err
	}
	return c.JSON(user)
}
`, module+"/internal/domain", module+"/internal/usecase")
}

// backendAuthMiddleware emits token verification and role enforcement.
func backendAuthMiddleware(module string) string {
	return fmt.Sprintf(`package http

import (
	"strings"

	"github.com/gofiber/fiber/v2"

	%q
	%q
)

// Context keys for the authenticated principal.
const (
	contextUserID = "auth_user_id"
	contextRole   = "auth_role"
)

// RequireAuth rejects any request without a valid access token.
//
// This is applied to a router group rather than listed per route. Opt-in
// protection fails open: the day someone adds a handler and forgets the
// middleware, that endpoint is public and nothing complains. Group-level
// application means a new route under the group is protected by default.
func RequireAuth(tokens port.TokenIssuer) fiber.Handler {
	return func(c *fiber.Ctx) error {
		header := c.Get("Authorization")
		if header == "" {
			return domain.Unauthorized("token_missing", "an access token is required")
		}

		scheme, token, found := strings.Cut(header, " ")
		if !found || !strings.EqualFold(scheme, "Bearer") {
			return domain.Unauthorized("token_malformed",
				"the Authorization header must be 'Bearer <token>'")
		}

		claims, err := tokens.Verify(strings.TrimSpace(token))
		if err != nil {
			// The reason is deliberately not echoed. "expired" versus
			// "signature invalid" tells an attacker probing with forged
			// tokens which part they got right.
			return domain.Unauthorized("token_invalid", "the access token is not valid")
		}

		c.Locals(contextUserID, claims.UserID)
		c.Locals(contextRole, claims.Role)
		return c.Next()
	}
}

// RequireRole rejects an authenticated caller whose role is not permitted.
//
// It must be mounted after RequireAuth. A missing role means the chain was
// assembled wrongly, and that is treated as forbidden rather than ignored: an
// authorisation check that silently passes when misconfigured is worse than no
// check, because it looks like protection.
func RequireRole(allowed ...string) fiber.Handler {
	permitted := make(map[string]bool, len(allowed))
	for _, role := range allowed {
		permitted[role] = true
	}

	return func(c *fiber.Ctx) error {
		role, _ := c.Locals(contextRole).(string)
		if role == "" {
			return domain.Forbidden("role_unknown", "the caller has no role")
		}
		if !permitted[role] {
			return domain.Forbidden("role_insufficient",
				"this operation requires one of: "+strings.Join(allowed, ", "))
		}
		return c.Next()
	}
}

// CurrentUserID returns the authenticated principal's identifier, or the empty
// string on an unauthenticated route.
func CurrentUserID(c *fiber.Ctx) string {
	id, _ := c.Locals(contextUserID).(string)
	return id
}

// CurrentRole returns the authenticated principal's role.
func CurrentRole(c *fiber.Ctx) string {
	role, _ := c.Locals(contextRole).(string)
	return role
}
`, module+"/internal/domain", module+"/internal/port")
}

// defaultRole picks the role a self-registered account receives.
//
// It is the least privileged role the blueprint declares, chosen by position:
// enum values are listed from most to least privileged by convention, so the
// last one is the safe default. Registering users as "admin" because it
// happened to be first would be a privilege-escalation hole opened by
// alphabetical accident.
func defaultRole(e Entity) string {
	roles := userRoles(e)
	if len(roles) == 0 {
		return "member"
	}

	// Prefer an explicitly recognisable low-privilege name when present.
	for _, candidate := range []string{"viewer", "member", "user", "customer", "employee"} {
		for _, role := range roles {
			if role == candidate {
				return role
			}
		}
	}
	return roles[len(roles)-1]
}

// backendAuthCryptoTest emits tests for the hashing and token primitives.
//
// These need no database, so they run everywhere and are the fastest place to
// catch a regression in the parts that matter most.
func backendAuthCryptoTest() string {
	return `package authcrypto

import (
	"strings"
	"testing"
	"time"
)

func TestPasswordHashVerifies(t *testing.T) {
	h := NewArgon2Hasher()

	encoded, err := h.Hash("correct horse battery staple")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}

	ok, err := h.Verify("correct horse battery staple", encoded)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !ok {
		t.Error("the correct password did not verify")
	}

	ok, err = h.Verify("wrong password entirely", encoded)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if ok {
		t.Error("an incorrect password verified")
	}
}

// Two hashes of the same password must differ, or the salt is not doing its
// job and identical passwords become visible to anyone reading the table.
func TestPasswordHashIsSalted(t *testing.T) {
	h := NewArgon2Hasher()

	first, err := h.Hash("same password")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	second, err := h.Hash("same password")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}

	if first == second {
		t.Error("two hashes of the same password are identical; the salt is not random")
	}
	if !strings.HasPrefix(first, "$argon2id$") {
		t.Errorf("the hash is not in PHC format: %s", first)
	}
}

func TestVerifyRejectsMalformedHashes(t *testing.T) {
	h := NewArgon2Hasher()

	for name, encoded := range map[string]string{
		"empty":            "",
		"not a hash":       "plaintext",
		"wrong algorithm":  "$bcrypt$v=19$m=65536,t=1,p=3$c2FsdA$aGFzaA",
		"missing sections": "$argon2id$v=19$m=65536",
		"bad base64":       "$argon2id$v=19$m=65536,t=1,p=3$!!!$!!!",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := h.Verify("password", encoded); err == nil {
				t.Errorf("a malformed hash was accepted: %q", encoded)
			}
		})
	}
}

// Two tokens for the same subject must differ even when minted in the same
// second, or a client cannot tell that rotation happened.
func TestTokensAreUniquePerIssue(t *testing.T) {
	issuer := NewHMACIssuer("a-test-signing-key-of-sufficient-length")

	first, err := issuer.Issue("user-123", "admin", time.Hour)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	second, err := issuer.Issue("user-123", "admin", time.Hour)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if first == second {
		t.Error("two tokens issued in the same second are identical")
	}
}

func TestTokenRoundTrip(t *testing.T) {
	issuer := NewHMACIssuer("a-test-signing-key-of-sufficient-length")

	token, err := issuer.Issue("user-123", "admin", time.Hour)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	claims, err := issuer.Verify(token)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if claims.UserID != "user-123" {
		t.Errorf("subject changed: got %q", claims.UserID)
	}
	if claims.Role != "admin" {
		t.Errorf("role changed: got %q", claims.Role)
	}
}

// A token signed with a different key must not verify. This is the property
// the whole scheme rests on.
func TestTokenRejectsAForeignSignature(t *testing.T) {
	mint := NewHMACIssuer("the-real-signing-key-of-sufficient-length")
	forge := NewHMACIssuer("an-attackers-signing-key-of-enough-length")

	token, err := forge.Issue("user-123", "admin", time.Hour)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if _, err := mint.Verify(token); err == nil {
		t.Error("a token signed with a different key verified")
	}
}

// An expired token must be refused even though its signature is valid.
func TestTokenRejectsExpiry(t *testing.T) {
	issuer := NewHMACIssuer("a-test-signing-key-of-sufficient-length")

	token, err := issuer.Issue("user-123", "admin", -time.Minute)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if _, err := issuer.Verify(token); err == nil {
		t.Error("an expired token verified")
	}
}

// The classic JWT attack: strip the signature and declare alg:none.
func TestTokenRejectsTheNoneAlgorithm(t *testing.T) {
	issuer := NewHMACIssuer("a-test-signing-key-of-sufficient-length")

	// {"alg":"none","typ":"JWT"} . {"sub":"attacker","role":"admin","exp":...}
	forged := "eyJhbGciOiJub25lIiwidHlwIjoiSldUIn0." +
		"eyJzdWIiOiJhdHRhY2tlciIsInJvbGUiOiJhZG1pbiIsImV4cCI6NDEwMjQ0NDgwMH0."

	if _, err := issuer.Verify(forged); err == nil {
		t.Error("a token with alg:none was accepted")
	}
}

// Tampering with the payload must invalidate the signature.
func TestTokenRejectsTamperedPayload(t *testing.T) {
	issuer := NewHMACIssuer("a-test-signing-key-of-sufficient-length")

	token, err := issuer.Issue("user-123", "viewer", time.Hour)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("unexpected token shape: %s", token)
	}
	// Replace the payload with one claiming admin.
	parts[1] = "eyJzdWIiOiJ1c2VyLTEyMyIsInJvbGUiOiJhZG1pbiIsImV4cCI6NDEwMjQ0NDgwMH0"

	if _, err := issuer.Verify(strings.Join(parts, ".")); err == nil {
		t.Error("a tampered payload verified")
	}
}

func TestRefreshTokenIsRandomAndHashable(t *testing.T) {
	first, firstHash, err := NewRefreshToken()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	second, _, err := NewRefreshToken()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	if first == second {
		t.Error("two refresh tokens are identical")
	}
	if HashRefreshToken(first) != firstHash {
		t.Error("hashing a token did not reproduce its stored hash")
	}
	if strings.Contains(firstHash, first) {
		t.Error("the stored hash contains the token itself")
	}
}
`
}

// backendAuthServiceTest emits integration tests for the session lifecycle.
//
// These need a real database. Refresh rotation is a concurrency protocol, and
// the bug this suite exists to catch — revoking a token family inside the same
// transaction that then rolls back, so the revocation never commits — is
// invisible to any test that does not actually commit.
func backendAuthServiceTest(module string) string {
	return fmt.Sprintf(`package postgres_test

import (
	"context"
	"testing"
	"time"

	%q
	%q
	%q
	%q
)

func newAuthService(t *testing.T) (*usecase.AuthService, *postgres.SessionRepo) {
	t.Helper()
	db := newTestDB(t)

	usecase.SetRefreshTokenFuncs(authcrypto.NewRefreshToken, authcrypto.HashRefreshToken)
	sessions := postgres.NewSessionRepo(db)

	return usecase.NewAuthService(
		postgres.NewUserRepo(db),
		sessions,
		authcrypto.NewArgon2Hasher(),
		authcrypto.NewHMACIssuer("a-test-signing-key-of-sufficient-length"),
		postgres.NewUnitOfWork(db),
	), sessions
}

func uniqueEmail() string {
	return "user-" + time.Now().Format("20060102150405.000000000") + "@example.test"
}

// Replaying a used refresh token must revoke the whole family, and the
// revocation must survive the error return. A revocation rolled back with the
// transaction tells the caller everything was revoked while every stolen token
// keeps working — worse than not detecting the replay at all.
func TestRefreshTokenReuseRevokesTheFamilyDurably(t *testing.T) {
	svc, _ := newAuthService(t)
	ctx := context.Background()

	_, first, err := svc.Register(ctx, uniqueEmail(), "Replay Subject", "a-sufficiently-long-password")
	if err != nil {
		t.Fatalf("register: %%v", err)
	}

	second, err := svc.Refresh(ctx, first.RefreshToken)
	if err != nil {
		t.Fatalf("first refresh: %%v", err)
	}
	if second.RefreshToken == first.RefreshToken {
		t.Fatal("the refresh token was not rotated")
	}

	// Replay the original, already-exchanged token.
	if _, err := svc.Refresh(ctx, first.RefreshToken); err == nil {
		t.Fatal("a replayed refresh token was accepted")
	}

	// The token minted by the legitimate refresh must now be dead too.
	if _, err := svc.Refresh(ctx, second.RefreshToken); err == nil {
		t.Error("the family was not revoked: a descendant token still works")
	}
}

func TestRefreshRotatesAndTheOldTokenStops(t *testing.T) {
	svc, _ := newAuthService(t)
	ctx := context.Background()

	_, pair, err := svc.Register(ctx, uniqueEmail(), "Rotation Subject", "a-sufficiently-long-password")
	if err != nil {
		t.Fatalf("register: %%v", err)
	}

	rotated, err := svc.Refresh(ctx, pair.RefreshToken)
	if err != nil {
		t.Fatalf("refresh: %%v", err)
	}
	if rotated.AccessToken == pair.AccessToken {
		t.Error("the access token was not reissued")
	}
}

func TestLoginRejectsTheWrongPasswordAndUnknownAccountsIdentically(t *testing.T) {
	svc, _ := newAuthService(t)
	ctx := context.Background()

	email := uniqueEmail()
	if _, _, err := svc.Register(ctx, email, "Login Subject", "a-sufficiently-long-password"); err != nil {
		t.Fatalf("register: %%v", err)
	}

	_, _, wrongPassword := svc.Login(ctx, email, "the-wrong-password-entirely")
	_, _, unknownUser := svc.Login(ctx, uniqueEmail(), "a-sufficiently-long-password")

	if wrongPassword == nil || unknownUser == nil {
		t.Fatal("a bad credential was accepted")
	}
	// Identical messages: a different one for each case is an account
	// enumeration oracle.
	if wrongPassword.Error() != unknownUser.Error() {
		t.Errorf("the errors differ and leak which accounts exist:\n  wrong password: %%v\n  unknown user:   %%v",
			wrongPassword, unknownUser)
	}
}

func TestLogoutRevokesTheSession(t *testing.T) {
	svc, _ := newAuthService(t)
	ctx := context.Background()

	_, pair, err := svc.Register(ctx, uniqueEmail(), "Logout Subject", "a-sufficiently-long-password")
	if err != nil {
		t.Fatalf("register: %%v", err)
	}
	if err := svc.Logout(ctx, pair.RefreshToken); err != nil {
		t.Fatalf("logout: %%v", err)
	}
	if _, err := svc.Refresh(ctx, pair.RefreshToken); err == nil {
		t.Error("a refresh token still worked after logout")
	}
}

// Expired sessions must be reapable, or the table grows forever.
func TestExpiredSessionsAreDeletable(t *testing.T) {
	_, sessions := newAuthService(t)
	ctx := context.Background()

	if _, err := sessions.DeleteExpired(ctx, time.Now().UTC().Add(-365*24*time.Hour)); err != nil {
		t.Fatalf("delete expired: %%v", err)
	}
	_ = domain.MinPasswordLength
}
`, module+"/internal/domain", module+"/internal/infra/authcrypto",
		module+"/internal/infra/postgres", module+"/internal/usecase")
}

// backendAuthContractTest emits HTTP-level tests for the auth surface.
//
// The most valuable assertion here is that protected routes reject an
// unauthenticated caller. Before v1.2 this test existed and skipped every
// case, which is the worst possible state: a suite that reports green while
// asserting nothing about the property it names.
func backendAuthContractTest(module string, resources []Entity) string {
	var routes strings.Builder
	shown := 0
	for _, e := range resources {
		if shown >= 6 {
			break
		}
		fmt.Fprintf(&routes, "\t\t{http.MethodGet, \"/api/v1/%s\"},\n", routePath(e))
		fmt.Fprintf(&routes, "\t\t{http.MethodPost, \"/api/v1/%s\"},\n", routePath(e))
		shown++
	}

	return fmt.Sprintf(`package http_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"

	apphttp %q
	%q
	%q
	%q
)

// newProtectedApp builds a router with the same auth middleware as production.
func newProtectedApp(t *testing.T) (*fiber.App, port.TokenIssuer) {
	t.Helper()
	issuer := authcrypto.NewHMACIssuer("a-test-signing-key-of-sufficient-length")

	app := fiber.New(fiber.Config{ErrorHandler: httpx.ErrorHandler})
	api := app.Group("/api/v1")

	protected := api.Group("", apphttp.RequireAuth(issuer))
	protected.Get("/ping", func(c *fiber.Ctx) error {
		return c.JSON(fiber.Map{"user": apphttp.CurrentUserID(c), "role": apphttp.CurrentRole(c)})
	})
	protected.Get("/admin", apphttp.RequireRole("admin"), func(c *fiber.Ctx) error {
		return c.SendStatus(fiber.StatusOK)
	})
	return app, issuer
}

// TestProtectedRoutesRejectAnonymousCallers is the single most valuable
// contract test in the suite. An endpoint accidentally left public is
// invisible in manual testing, because the developer is always signed in.
func TestProtectedRoutesRejectAnonymousCallers(t *testing.T) {
	app, _ := newProtectedApp(t)

	res, err := app.Test(httptest.NewRequest(http.MethodGet, "/api/v1/ping", nil))
	if err != nil {
		t.Fatalf("request: %%v", err)
	}
	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("an anonymous request got %%d, expected 401", res.StatusCode)
	}
}

func TestProtectedRoutesAcceptAValidToken(t *testing.T) {
	app, issuer := newProtectedApp(t)

	token, err := issuer.Issue("user-1", "admin", time.Hour)
	if err != nil {
		t.Fatalf("issue: %%v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/ping", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := app.Test(req)
	if err != nil {
		t.Fatalf("request: %%v", err)
	}
	if res.StatusCode != http.StatusOK {
		t.Errorf("a valid token got %%d, expected 200", res.StatusCode)
	}
}

// Malformed Authorization headers must be refused rather than misparsed.
func TestMalformedAuthorizationHeadersAreRejected(t *testing.T) {
	app, issuer := newProtectedApp(t)
	token, err := issuer.Issue("user-1", "admin", time.Hour)
	if err != nil {
		t.Fatalf("issue: %%v", err)
	}

	for name, header := range map[string]string{
		"no scheme":       token,
		"wrong scheme":    "Basic " + token,
		"empty bearer":    "Bearer ",
		"garbage token":   "Bearer not-a-real-token",
		"only the scheme": "Bearer",
	} {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/v1/ping", nil)
			req.Header.Set("Authorization", header)
			res, err := app.Test(req)
			if err != nil {
				t.Fatalf("request: %%v", err)
			}
			if res.StatusCode != http.StatusUnauthorized {
				t.Errorf("header %%q got %%d, expected 401", header, res.StatusCode)
			}
		})
	}
}

// An authenticated caller without the required role gets 403, not 401. The
// distinction matters: 401 tells a client to authenticate, which it already
// has, and a client that retries authentication forever is a support ticket.
func TestInsufficientRoleIsForbiddenNotUnauthorized(t *testing.T) {
	app, issuer := newProtectedApp(t)

	token, err := issuer.Issue("user-1", "viewer", time.Hour)
	if err != nil {
		t.Fatalf("issue: %%v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := app.Test(req)
	if err != nil {
		t.Fatalf("request: %%v", err)
	}
	if res.StatusCode != http.StatusForbidden {
		t.Errorf("an under-privileged caller got %%d, expected 403", res.StatusCode)
	}
}

// Every resource route must sit behind authentication. This enumerates the
// real generated routes rather than skipping them.
func TestResourceRoutesRequireAuthentication(t *testing.T) {
	protected := []struct {
		method string
		path   string
	}{
%s	}

	app := newResourceAppUnderAuth(t)
	for _, tc := range protected {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			res, err := app.Test(httptest.NewRequest(tc.method, tc.path, strings.NewReader("{}")))
			if err != nil {
				t.Fatalf("request: %%v", err)
			}
			if res.StatusCode != http.StatusUnauthorized {
				t.Errorf("%%s %%s got %%d, expected 401", tc.method, tc.path, res.StatusCode)
			}
		})
	}
}

// newResourceAppUnderAuth mirrors how cmd/server mounts the resource group:
// behind RequireAuth, so a route added later is protected by default.
func newResourceAppUnderAuth(t *testing.T) *fiber.App {
	t.Helper()
	issuer := authcrypto.NewHMACIssuer("a-test-signing-key-of-sufficient-length")

	app := fiber.New(fiber.Config{ErrorHandler: httpx.ErrorHandler})
	api := app.Group("/api/v1")
	guarded := api.Group("", apphttp.RequireAuth(issuer))

	// The handlers themselves need a database; the middleware runs first and
	// rejects before any of them is reached, which is exactly the property
	// under test.
	guarded.All("/*", func(c *fiber.Ctx) error { return c.SendStatus(fiber.StatusOK) })
	return app
}
`, module+"/internal/adapter/http", module+"/internal/infra/authcrypto",
		module+"/internal/httpx", module+"/internal/port", routes.String())
}
