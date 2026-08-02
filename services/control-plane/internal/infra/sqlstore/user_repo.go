package sqlstore

import (
	"context"
	"database/sql"
	"time"

	"github.com/genesis-ai-factory/control-plane/internal/domain"
	"github.com/genesis-ai-factory/control-plane/internal/port"
)

// UserRepo implements port.UserRepository.
type UserRepo struct{ s *Store }

// NewUserRepo constructs the repository.
func NewUserRepo(s *Store) *UserRepo { return &UserRepo{s: s} }

var _ port.UserRepository = (*UserRepo)(nil)

const userColumns = `id, email, password_hash, display_name, role, status, settings,
	created_at, updated_at, deleted_at`

func (r *UserRepo) Create(ctx context.Context, u *domain.User) error {
	settings, err := encodeJSON(u.Settings)
	if err != nil {
		return err
	}
	_, err = r.s.conn(ctx).ExecContext(ctx, r.s.rebind(`
		INSERT INTO users (`+userColumns+`)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`),
		u.ID.String(), u.Email, u.PasswordHash, u.DisplayName, string(u.Role), string(u.Status),
		settings, encodeTime(u.CreatedAt), encodeTime(u.UpdatedAt), encodeTimePtr(u.DeletedAt))
	if isUniqueViolation(err) {
		return domain.Conflict("email_taken", "an account with this email already exists")
	}
	return err
}

func (r *UserRepo) Update(ctx context.Context, u *domain.User) error {
	settings, err := encodeJSON(u.Settings)
	if err != nil {
		return err
	}
	res, err := r.s.conn(ctx).ExecContext(ctx, r.s.rebind(`
		UPDATE users SET email=$1, password_hash=$2, display_name=$3, role=$4,
			status=$5, settings=$6, updated_at=$7, deleted_at=$8
		WHERE id=$9`),
		u.Email, u.PasswordHash, u.DisplayName, string(u.Role), string(u.Status),
		settings, encodeTime(u.UpdatedAt), encodeTimePtr(u.DeletedAt), u.ID.String())
	if isUniqueViolation(err) {
		return domain.Conflict("email_taken", "an account with this email already exists")
	}
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return domain.NotFound("user")
	}
	return nil
}

func (r *UserRepo) ByID(ctx context.Context, id domain.ID) (*domain.User, error) {
	row := r.s.conn(ctx).QueryRowContext(ctx,
		r.s.rebind(`SELECT `+userColumns+` FROM users WHERE id=$1`), id.String())
	return scanUser(row)
}

func (r *UserRepo) ByEmail(ctx context.Context, email string) (*domain.User, error) {
	row := r.s.conn(ctx).QueryRowContext(ctx,
		r.s.rebind(`SELECT `+userColumns+` FROM users WHERE email=$1`), domain.NormalizeEmail(email))
	return scanUser(row)
}

func (r *UserRepo) Count(ctx context.Context) (int64, error) {
	var n int64
	err := r.s.conn(ctx).QueryRowContext(ctx,
		`SELECT COUNT(*) FROM users WHERE deleted_at IS NULL`).Scan(&n)
	return n, err
}

type rowScanner interface{ Scan(dest ...any) error }

func scanUser(row rowScanner) (*domain.User, error) {
	var (
		u         domain.User
		id        string
		role      string
		status    string
		settings  string
		createdAt string
		updatedAt string
		deletedAt sql.NullString
	)
	err := row.Scan(&id, &u.Email, &u.PasswordHash, &u.DisplayName, &role, &status,
		&settings, &createdAt, &updatedAt, &deletedAt)
	if err != nil {
		return nil, notFound(err, "user")
	}
	u.ID = domain.ID(id)
	u.Role = domain.Role(role)
	u.Status = domain.UserStatus(status)
	if u.Settings, err = decodeSettings(settings); err != nil {
		return nil, err
	}
	if u.CreatedAt, err = decodeTime(createdAt); err != nil {
		return nil, err
	}
	if u.UpdatedAt, err = decodeTime(updatedAt); err != nil {
		return nil, err
	}
	if u.DeletedAt, err = decodeTimePtr(deletedAt); err != nil {
		return nil, err
	}
	return &u, nil
}

// RefreshTokenRepo implements port.RefreshTokenRepository.
type RefreshTokenRepo struct{ s *Store }

// NewRefreshTokenRepo constructs the repository.
func NewRefreshTokenRepo(s *Store) *RefreshTokenRepo { return &RefreshTokenRepo{s: s} }

var _ port.RefreshTokenRepository = (*RefreshTokenRepo)(nil)

func (r *RefreshTokenRepo) Create(ctx context.Context, t *domain.RefreshToken) error {
	_, err := r.s.conn(ctx).ExecContext(ctx, r.s.rebind(`
		INSERT INTO refresh_tokens (id, user_id, token_hash, family_id, expires_at,
			revoked_at, replaced_by, user_agent, ip, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`),
		t.ID.String(), t.UserID.String(), t.TokenHash, t.FamilyID.String(),
		encodeTime(t.ExpiresAt), encodeTimePtr(t.RevokedAt), idPtrValue(t.ReplacedBy),
		t.UserAgent, t.IP, encodeTime(t.CreatedAt))
	if isUniqueViolation(err) {
		return domain.Conflict("token_exists", "refresh token already issued")
	}
	return err
}

func (r *RefreshTokenRepo) ByHash(ctx context.Context, hash string) (*domain.RefreshToken, error) {
	row := r.s.conn(ctx).QueryRowContext(ctx, r.s.rebind(`
		SELECT id, user_id, token_hash, family_id, expires_at, revoked_at,
			replaced_by, user_agent, ip, created_at
		FROM refresh_tokens WHERE token_hash=$1`), hash)

	var (
		t          domain.RefreshToken
		id         string
		userID     string
		familyID   string
		expiresAt  string
		revokedAt  sql.NullString
		replacedBy sql.NullString
		createdAt  string
	)
	err := row.Scan(&id, &userID, &t.TokenHash, &familyID, &expiresAt, &revokedAt,
		&replacedBy, &t.UserAgent, &t.IP, &createdAt)
	if err != nil {
		return nil, notFound(err, "refresh_token")
	}
	t.ID = domain.ID(id)
	t.UserID = domain.ID(userID)
	t.FamilyID = domain.ID(familyID)
	t.ReplacedBy = idPtr(replacedBy)
	if t.ExpiresAt, err = decodeTime(expiresAt); err != nil {
		return nil, err
	}
	if t.RevokedAt, err = decodeTimePtr(revokedAt); err != nil {
		return nil, err
	}
	if t.CreatedAt, err = decodeTime(createdAt); err != nil {
		return nil, err
	}
	return &t, nil
}

func (r *RefreshTokenRepo) Revoke(ctx context.Context, id domain.ID, replacedBy *domain.ID, at time.Time) error {
	_, err := r.s.conn(ctx).ExecContext(ctx, r.s.rebind(`
		UPDATE refresh_tokens SET revoked_at=$1, replaced_by=$2 WHERE id=$3 AND revoked_at IS NULL`),
		encodeTime(at), idPtrValue(replacedBy), id.String())
	return err
}

// RevokeFamily invalidates an entire token lineage. This is the response to
// refresh-token reuse: if a token that was already rotated is presented again,
// either it was stolen or the client is broken, and in both cases every
// descendant session must die.
func (r *RefreshTokenRepo) RevokeFamily(ctx context.Context, familyID domain.ID, at time.Time) error {
	_, err := r.s.conn(ctx).ExecContext(ctx, r.s.rebind(`
		UPDATE refresh_tokens SET revoked_at=$1 WHERE family_id=$2 AND revoked_at IS NULL`),
		encodeTime(at), familyID.String())
	return err
}

func (r *RefreshTokenRepo) DeleteExpired(ctx context.Context, before time.Time) (int64, error) {
	res, err := r.s.conn(ctx).ExecContext(ctx, r.s.rebind(`
		DELETE FROM refresh_tokens WHERE expires_at < $1`), encodeTime(before))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
