package sqlstore

import (
	"context"
	"database/sql"
	"strconv"
	"strings"
	"time"

	"github.com/genesis-ai-factory/control-plane/internal/domain"
	"github.com/genesis-ai-factory/control-plane/internal/port"
)

// ProjectRepo implements port.ProjectRepository.
type ProjectRepo struct{ s *Store }

// NewProjectRepo constructs the repository.
func NewProjectRepo(s *Store) *ProjectRepo { return &ProjectRepo{s: s} }

var _ port.ProjectRepository = (*ProjectRepo)(nil)

const projectColumns = `id, owner_id, name, slug, prompt, description, category, status,
	workspace_path, stack, settings, created_at, updated_at, deleted_at`

func (r *ProjectRepo) Create(ctx context.Context, p *domain.Project) error {
	stack, err := encodeJSON(p.Stack)
	if err != nil {
		return err
	}
	settings, err := encodeJSON(p.Settings)
	if err != nil {
		return err
	}
	_, err = r.s.conn(ctx).ExecContext(ctx, r.s.rebind(`
		INSERT INTO projects (`+projectColumns+`)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`),
		p.ID.String(), p.OwnerID.String(), p.Name, p.Slug, p.Prompt, p.Description,
		string(p.Category), string(p.Status), p.WorkspacePath, stack, settings,
		encodeTime(p.CreatedAt), encodeTime(p.UpdatedAt), encodeTimePtr(p.DeletedAt))
	if isUniqueViolation(err) {
		return domain.Conflict("project_slug_taken", "a project with this name already exists")
	}
	return err
}

func (r *ProjectRepo) Update(ctx context.Context, p *domain.Project) error {
	stack, err := encodeJSON(p.Stack)
	if err != nil {
		return err
	}
	settings, err := encodeJSON(p.Settings)
	if err != nil {
		return err
	}
	res, err := r.s.conn(ctx).ExecContext(ctx, r.s.rebind(`
		UPDATE projects SET name=$1, slug=$2, prompt=$3, description=$4, category=$5,
			status=$6, workspace_path=$7, stack=$8, settings=$9, updated_at=$10, deleted_at=$11
		WHERE id=$12`),
		p.Name, p.Slug, p.Prompt, p.Description, string(p.Category), string(p.Status),
		p.WorkspacePath, stack, settings, encodeTime(p.UpdatedAt), encodeTimePtr(p.DeletedAt),
		p.ID.String())
	if isUniqueViolation(err) {
		return domain.Conflict("project_slug_taken", "a project with this name already exists")
	}
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return domain.NotFound("project")
	}
	return nil
}

func (r *ProjectRepo) ByID(ctx context.Context, id domain.ID) (*domain.Project, error) {
	row := r.s.conn(ctx).QueryRowContext(ctx,
		r.s.rebind(`SELECT `+projectColumns+` FROM projects WHERE id=$1`), id.String())
	return scanProject(row)
}

func (r *ProjectRepo) BySlug(ctx context.Context, ownerID domain.ID, slug string) (*domain.Project, error) {
	row := r.s.conn(ctx).QueryRowContext(ctx,
		r.s.rebind(`SELECT `+projectColumns+` FROM projects WHERE owner_id=$1 AND slug=$2`),
		ownerID.String(), slug)
	return scanProject(row)
}

func (r *ProjectRepo) List(ctx context.Context, f port.ProjectFilter) ([]*domain.Project, int64, error) {
	var (
		where []string
		args  []any
	)
	add := func(clause string, value any) {
		args = append(args, value)
		where = append(where, strings.Replace(clause, "?", "$"+strconv.Itoa(len(args)), 1))
	}

	if !f.OwnerID.IsZero() {
		add("owner_id = ?", f.OwnerID.String())
	}
	if f.Status != "" {
		add("status = ?", string(f.Status))
	}
	if f.Category != "" {
		add("category = ?", string(f.Category))
	}
	if q := strings.TrimSpace(f.Query); q != "" {
		// LIKE against a lowercased column works identically on both engines;
		// full-text search is a v0.3 concern once projects accumulate.
		//
		// The pattern is bound twice rather than referencing one placeholder
		// from two positions: PostgreSQL allows $1 to appear repeatedly, but
		// SQLite's positional `?` parameters do not, and a query that only
		// works on one engine defeats the purpose of a shared implementation.
		pattern := "%" + strings.ToLower(q) + "%"
		args = append(args, pattern)
		nameIdx := strconv.Itoa(len(args))
		args = append(args, pattern)
		promptIdx := strconv.Itoa(len(args))
		where = append(where, "(LOWER(name) LIKE $"+nameIdx+" OR LOWER(prompt) LIKE $"+promptIdx+")")
	}
	if !f.IncludeDeleted {
		where = append(where, "deleted_at IS NULL")
	}

	clause := ""
	if len(where) > 0 {
		clause = " WHERE " + strings.Join(where, " AND ")
	}

	var total int64
	if err := r.s.conn(ctx).QueryRowContext(ctx,
		r.s.rebind(`SELECT COUNT(*) FROM projects`+clause), args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	limit := clampLimit(f.Limit, 50, 200)
	offset := f.Offset
	if offset < 0 {
		offset = 0
	}
	args = append(args, limit, offset)
	query := `SELECT ` + projectColumns + ` FROM projects` + clause +
		` ORDER BY created_at DESC, id DESC LIMIT $` + strconv.Itoa(len(args)-1) +
		` OFFSET $` + strconv.Itoa(len(args))

	rows, err := r.s.conn(ctx).QueryContext(ctx, r.s.rebind(query), args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var out []*domain.Project
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, p)
	}
	return out, total, rows.Err()
}

func (r *ProjectRepo) SoftDelete(ctx context.Context, id domain.ID, at time.Time) error {
	ts := encodeTime(at)
	res, err := r.s.conn(ctx).ExecContext(ctx, r.s.rebind(`
		UPDATE projects SET deleted_at=$1, updated_at=$2, status=$3 WHERE id=$4 AND deleted_at IS NULL`),
		ts, ts, string(domain.ProjectArchived), id.String())
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return domain.NotFound("project")
	}
	return nil
}

func scanProject(row rowScanner) (*domain.Project, error) {
	var (
		p         domain.Project
		id        string
		ownerID   string
		category  string
		status    string
		stack     string
		settings  string
		createdAt string
		updatedAt string
		deletedAt sql.NullString
	)
	err := row.Scan(&id, &ownerID, &p.Name, &p.Slug, &p.Prompt, &p.Description,
		&category, &status, &p.WorkspacePath, &stack, &settings,
		&createdAt, &updatedAt, &deletedAt)
	if err != nil {
		return nil, notFound(err, "project")
	}
	p.ID = domain.ID(id)
	p.OwnerID = domain.ID(ownerID)
	p.Category = domain.ProjectCategory(category)
	p.Status = domain.ProjectStatus(status)
	if p.Stack, err = decodeSettings(stack); err != nil {
		return nil, err
	}
	p.Settings = domain.DefaultProjectSettings()
	if err := decodeInto(settings, &p.Settings); err != nil {
		return nil, err
	}
	if p.CreatedAt, err = decodeTime(createdAt); err != nil {
		return nil, err
	}
	if p.UpdatedAt, err = decodeTime(updatedAt); err != nil {
		return nil, err
	}
	if p.DeletedAt, err = decodeTimePtr(deletedAt); err != nil {
		return nil, err
	}
	return &p, nil
}
