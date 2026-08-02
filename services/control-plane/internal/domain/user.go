package domain

import (
	"net/mail"
	"strings"
	"time"
)

// Role is the account-level authorization role.
type Role string

const (
	RoleOwner  Role = "owner"
	RoleAdmin  Role = "admin"
	RoleMember Role = "member"
	RoleViewer Role = "viewer"
)

func (r Role) Valid() bool {
	switch r {
	case RoleOwner, RoleAdmin, RoleMember, RoleViewer:
		return true
	}
	return false
}

// rank orders roles for privilege comparison. Higher wins.
func (r Role) rank() int {
	switch r {
	case RoleOwner:
		return 40
	case RoleAdmin:
		return 30
	case RoleMember:
		return 20
	case RoleViewer:
		return 10
	}
	return 0
}

// AtLeast reports whether r carries at least the privileges of other.
func (r Role) AtLeast(other Role) bool { return r.rank() >= other.rank() }

// UserStatus controls whether an account may authenticate.
type UserStatus string

const (
	UserActive    UserStatus = "active"
	UserSuspended UserStatus = "suspended"
)

// User is an account. PasswordHash never leaves the server: DTO mapping in the
// HTTP layer has no field for it, so it cannot be leaked by accident.
type User struct {
	ID           ID
	Email        string
	PasswordHash string
	DisplayName  string
	Role         Role
	Status       UserStatus
	Settings     Settings
	CreatedAt    time.Time
	UpdatedAt    time.Time
	DeletedAt    *time.Time
}

// Settings is an open key/value bag persisted as JSON. It is intentionally
// schemaless: UI preferences change far more often than we want to migrate.
type Settings map[string]any

// NormalizeEmail lowercases and trims an address so uniqueness is meaningful.
func NormalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// ValidateEmail checks syntax using the stdlib parser rather than a regex.
func ValidateEmail(email string) error {
	if email == "" {
		return Invalid("email_required", "email is required")
	}
	if len(email) > 254 {
		return Invalid("email_too_long", "email must be at most 254 characters")
	}
	if _, err := mail.ParseAddress(email); err != nil {
		return Invalid("email_invalid", "email is not a valid address")
	}
	return nil
}

// MinPasswordLength is deliberately length-based rather than composition-based:
// NIST 800-63B guidance favours length and breach screening over character
// class rules, which mostly produce predictable substitutions.
const MinPasswordLength = 10

// ValidatePassword enforces the password policy.
func ValidatePassword(pw string) error {
	if len([]rune(pw)) < MinPasswordLength {
		return Invalid("password_too_short", "password must be at least 10 characters")
	}
	if len(pw) > 1024 {
		return Invalid("password_too_long", "password must be at most 1024 bytes")
	}
	return nil
}

// NewUser constructs a validated user aggregate. The caller supplies an
// already-hashed password: the domain layer must not depend on a hashing
// implementation.
func NewUser(email, passwordHash, displayName string, role Role, now time.Time) (*User, error) {
	email = NormalizeEmail(email)
	if err := ValidateEmail(email); err != nil {
		return nil, err
	}
	if passwordHash == "" {
		return nil, Invalid("password_required", "password hash is required")
	}
	displayName = strings.TrimSpace(displayName)
	if displayName == "" {
		displayName = email[:strings.Index(email, "@")]
	}
	if len([]rune(displayName)) > 80 {
		return nil, Invalid("display_name_too_long", "display name must be at most 80 characters")
	}
	if !role.Valid() {
		return nil, Invalid("role_invalid", "unknown role")
	}
	return &User{
		ID:           NewID(),
		Email:        email,
		PasswordHash: passwordHash,
		DisplayName:  displayName,
		Role:         role,
		Status:       UserActive,
		Settings:     Settings{},
		CreatedAt:    now.UTC(),
		UpdatedAt:    now.UTC(),
	}, nil
}

// CanAuthenticate reports whether the account is allowed to log in.
func (u *User) CanAuthenticate() bool {
	return u.Status == UserActive && u.DeletedAt == nil
}

// RefreshToken is a rotating opaque credential. Only its SHA-256 hash is
// stored, so a database disclosure does not yield usable sessions.
type RefreshToken struct {
	ID         ID
	UserID     ID
	TokenHash  string
	FamilyID   ID
	ExpiresAt  time.Time
	RevokedAt  *time.Time
	ReplacedBy *ID
	UserAgent  string
	IP         string
	CreatedAt  time.Time
}

// Active reports whether the token may still be exchanged.
func (t *RefreshToken) Active(now time.Time) bool {
	return t.RevokedAt == nil && now.Before(t.ExpiresAt)
}

// Principal is the authenticated identity attached to a request context.
type Principal struct {
	UserID ID
	Email  string
	Role   Role
}
