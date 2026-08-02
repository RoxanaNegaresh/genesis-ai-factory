package usecase

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/genesis-ai-factory/control-plane/internal/domain"
	"github.com/genesis-ai-factory/control-plane/internal/infra/crypto"
	"github.com/genesis-ai-factory/control-plane/internal/port"
)

// AuthConfig carries token lifetimes.
type AuthConfig struct {
	AccessTTL  time.Duration
	RefreshTTL time.Duration
	SingleUser bool
}

// Auth implements registration, login, refresh-token rotation and logout.
type Auth struct {
	users  port.UserRepository
	tokens port.RefreshTokenRepository
	hasher port.Hasher
	issuer port.TokenIssuer
	clock  port.Clock
	tx     port.TxManager
	cfg    AuthConfig
	log    *slog.Logger
}

// NewAuth constructs the authentication service.
func NewAuth(
	users port.UserRepository,
	tokens port.RefreshTokenRepository,
	hasher port.Hasher,
	issuer port.TokenIssuer,
	clock port.Clock,
	tx port.TxManager,
	cfg AuthConfig,
	log *slog.Logger,
) *Auth {
	if log == nil {
		log = slog.Default()
	}
	if cfg.AccessTTL <= 0 {
		cfg.AccessTTL = 15 * time.Minute
	}
	if cfg.RefreshTTL <= 0 {
		cfg.RefreshTTL = 30 * 24 * time.Hour
	}
	return &Auth{users: users, tokens: tokens, hasher: hasher, issuer: issuer,
		clock: clock, tx: tx, cfg: cfg, log: log}
}

// Session is the credential pair returned to a client.
type Session struct {
	AccessToken  string
	ExpiresAt    time.Time
	RefreshToken string
	RefreshExp   time.Time
	User         *domain.User
}

// ClientInfo describes the caller, recorded on refresh tokens for auditing.
type ClientInfo struct {
	UserAgent string
	IP        string
}

// Register creates an account. The first account on an installation becomes the
// owner: a fresh desktop install should not require a separate bootstrap step.
func (a *Auth) Register(ctx context.Context, email, password, displayName string, client ClientInfo) (*Session, error) {
	email = domain.NormalizeEmail(email)
	if err := domain.ValidateEmail(email); err != nil {
		return nil, err
	}
	if err := domain.ValidatePassword(password); err != nil {
		return nil, err
	}

	count, err := a.users.Count(ctx)
	if err != nil {
		return nil, err
	}
	role := domain.RoleMember
	if count == 0 {
		role = domain.RoleOwner
	}

	hash, err := a.hasher.Hash(password)
	if err != nil {
		return nil, err
	}
	user, err := domain.NewUser(email, hash, displayName, role, a.clock.Now())
	if err != nil {
		return nil, err
	}

	var session *Session
	err = a.tx.WithTx(ctx, func(ctx context.Context) error {
		if err := a.users.Create(ctx, user); err != nil {
			return err
		}
		session, err = a.issueSession(ctx, user, domain.NewID(), client)
		return err
	})
	if err != nil {
		return nil, err
	}
	a.log.Info("account registered", "user_id", user.ID.String(), "role", string(role))
	return session, nil
}

// Login authenticates with email and password.
func (a *Auth) Login(ctx context.Context, email, password string, client ClientInfo) (*Session, error) {
	user, err := a.users.ByEmail(ctx, email)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			// Burn comparable CPU on a missing account so response timing does
			// not reveal whether an email is registered.
			_, _, _ = a.hasher.Verify(password, dummyHash)
			return nil, domain.Unauthorized("invalid email or password")
		}
		return nil, err
	}

	ok, needsRehash, err := a.hasher.Verify(password, user.PasswordHash)
	if err != nil {
		a.log.Warn("password verification error", "user_id", user.ID.String(), "error", err)
		return nil, domain.Unauthorized("invalid email or password")
	}
	if !ok {
		return nil, domain.Unauthorized("invalid email or password")
	}
	if !user.CanAuthenticate() {
		return nil, domain.Forbidden("this account is not permitted to sign in")
	}

	// Transparently upgrade a hash that predates the current cost policy.
	if needsRehash {
		if newHash, err := a.hasher.Hash(password); err == nil {
			user.PasswordHash = newHash
			user.UpdatedAt = a.clock.Now()
			if err := a.users.Update(ctx, user); err != nil {
				a.log.Warn("password rehash failed", "user_id", user.ID.String(), "error", err)
			}
		}
	}

	var session *Session
	err = a.tx.WithTx(ctx, func(ctx context.Context) error {
		var err error
		session, err = a.issueSession(ctx, user, domain.NewID(), client)
		return err
	})
	if err != nil {
		return nil, err
	}
	return session, nil
}

// Refresh rotates a refresh token and issues a new access token.
//
// Reuse detection: presenting a token that was already rotated means either it
// was stolen or the client is broken. Both cases warrant killing the entire
// token family rather than guessing which holder is legitimate.
func (a *Auth) Refresh(ctx context.Context, refreshToken string, client ClientInfo) (*Session, error) {
	if refreshToken == "" {
		return nil, domain.Unauthorized("refresh token is required")
	}
	stored, err := a.tokens.ByHash(ctx, crypto.HashToken(refreshToken))
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return nil, domain.Unauthorized("invalid refresh token")
		}
		return nil, err
	}

	now := a.clock.Now()
	if stored.RevokedAt != nil {
		a.log.Warn("refresh token reuse detected; revoking family",
			"user_id", stored.UserID.String(), "family_id", stored.FamilyID.String())
		if err := a.tokens.RevokeFamily(ctx, stored.FamilyID, now); err != nil {
			a.log.Error("family revocation failed", "error", err)
		}
		return nil, domain.Unauthorized("refresh token has been revoked")
	}
	if !stored.Active(now) {
		return nil, domain.Unauthorized("refresh token has expired")
	}

	user, err := a.users.ByID(ctx, stored.UserID)
	if err != nil {
		return nil, domain.Unauthorized("invalid refresh token")
	}
	if !user.CanAuthenticate() {
		return nil, domain.Forbidden("this account is not permitted to sign in")
	}

	var session *Session
	err = a.tx.WithTx(ctx, func(ctx context.Context) error {
		next, err := a.issueSession(ctx, user, stored.FamilyID, client)
		if err != nil {
			return err
		}
		// Link the old token to its replacement so an audit can reconstruct the
		// rotation chain.
		newID, err := a.tokenIDFor(ctx, next.RefreshToken)
		if err != nil {
			return err
		}
		if err := a.tokens.Revoke(ctx, stored.ID, &newID, now); err != nil {
			return err
		}
		session = next
		return nil
	})
	if err != nil {
		return nil, err
	}
	return session, nil
}

// Logout revokes the presented refresh token. It is deliberately forgiving:
// signing out must always appear to succeed.
func (a *Auth) Logout(ctx context.Context, refreshToken string) error {
	if refreshToken == "" {
		return nil
	}
	stored, err := a.tokens.ByHash(ctx, crypto.HashToken(refreshToken))
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return nil
		}
		return err
	}
	return a.tokens.Revoke(ctx, stored.ID, nil, a.clock.Now())
}

// Me returns the authenticated account.
func (a *Auth) Me(ctx context.Context, userID domain.ID) (*domain.User, error) {
	return a.users.ByID(ctx, userID)
}

// EnsureLocalOwner bootstraps the single-user desktop account so a local
// install has an identity without showing a login wall. The password is random
// and never used: the desktop authenticates with the returned session.
func (a *Auth) EnsureLocalOwner(ctx context.Context) (*domain.User, error) {
	const localEmail = "local@genesis"

	user, err := a.users.ByEmail(ctx, localEmail)
	if err == nil {
		return user, nil
	}
	if !errors.Is(err, domain.ErrNotFound) {
		return nil, err
	}

	password, _, err := crypto.NewOpaqueToken()
	if err != nil {
		return nil, err
	}
	hash, err := a.hasher.Hash(password)
	if err != nil {
		return nil, err
	}
	user, err = domain.NewUser(localEmail, hash, "Local Owner", domain.RoleOwner, a.clock.Now())
	if err != nil {
		return nil, err
	}
	if err := a.users.Create(ctx, user); err != nil {
		// A concurrent boot may have won the race; adopt its account.
		if errors.Is(err, domain.ErrConflict) {
			return a.users.ByEmail(ctx, localEmail)
		}
		return nil, err
	}
	a.log.Info("bootstrapped local owner account", "user_id", user.ID.String())
	return user, nil
}

// IssueFor mints a session for an already-authenticated user, used by the
// desktop sidecar handshake.
func (a *Auth) IssueFor(ctx context.Context, user *domain.User, client ClientInfo) (*Session, error) {
	var session *Session
	err := a.tx.WithTx(ctx, func(ctx context.Context) error {
		var err error
		session, err = a.issueSession(ctx, user, domain.NewID(), client)
		return err
	})
	return session, err
}

// issueSession mints an access token plus a fresh refresh token in a family.
func (a *Auth) issueSession(ctx context.Context, user *domain.User, familyID domain.ID, client ClientInfo) (*Session, error) {
	now := a.clock.Now()
	principal := domain.Principal{UserID: user.ID, Email: user.Email, Role: user.Role}

	access, accessExp, err := a.issuer.Issue(principal, a.cfg.AccessTTL)
	if err != nil {
		return nil, err
	}
	raw, hash, err := crypto.NewOpaqueToken()
	if err != nil {
		return nil, err
	}
	refresh := &domain.RefreshToken{
		ID:        domain.NewID(),
		UserID:    user.ID,
		TokenHash: hash,
		FamilyID:  familyID,
		ExpiresAt: now.Add(a.cfg.RefreshTTL),
		UserAgent: truncate(client.UserAgent, 400),
		IP:        truncate(client.IP, 64),
		CreatedAt: now,
	}
	if err := a.tokens.Create(ctx, refresh); err != nil {
		return nil, err
	}
	return &Session{
		AccessToken:  access,
		ExpiresAt:    accessExp,
		RefreshToken: raw,
		RefreshExp:   refresh.ExpiresAt,
		User:         user,
	}, nil
}

func (a *Auth) tokenIDFor(ctx context.Context, rawToken string) (domain.ID, error) {
	t, err := a.tokens.ByHash(ctx, crypto.HashToken(rawToken))
	if err != nil {
		return domain.Nil, err
	}
	return t.ID, nil
}

// dummyHash is a real Argon2id hash of an unguessable value, used to equalise
// login timing for unknown accounts.
const dummyHash = "$argon2id$v=19$m=65536,t=3,p=2$c2FsdHNhbHRzYWx0c2Ex$aGFzaGhhc2hoYXNoaGFzaGhhc2hoYXNoaGFzaGhhcw"

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
