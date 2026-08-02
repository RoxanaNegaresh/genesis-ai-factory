// Package port declares the interfaces that inner layers own and outer layers
// implement. Dependencies point inward: usecase depends on port, infra depends
// on port, and port depends only on domain.
package port

import (
	"context"
	"time"

	"github.com/genesis-ai-factory/control-plane/internal/domain"
)

// Clock abstracts time so use cases are testable without sleeping.
type Clock interface {
	Now() time.Time
}

// Hasher hashes and verifies passwords.
type Hasher interface {
	Hash(password string) (string, error)
	Verify(password, encoded string) (ok bool, needsRehash bool, err error)
}

// TokenIssuer mints and validates access tokens.
type TokenIssuer interface {
	Issue(p domain.Principal, ttl time.Duration) (token string, expiresAt time.Time, err error)
	Parse(token string) (domain.Principal, error)
}

// Tx is a unit of work. A use case that must be atomic runs inside one.
type Tx interface {
	Commit() error
	Rollback() error
}

// TxManager starts transactions. The callback style guarantees that a
// transaction cannot be leaked by an early return.
type TxManager interface {
	WithTx(ctx context.Context, fn func(ctx context.Context) error) error
}

// UserRepository persists accounts.
type UserRepository interface {
	Create(ctx context.Context, u *domain.User) error
	Update(ctx context.Context, u *domain.User) error
	ByID(ctx context.Context, id domain.ID) (*domain.User, error)
	ByEmail(ctx context.Context, email string) (*domain.User, error)
	Count(ctx context.Context) (int64, error)
}

// RefreshTokenRepository persists rotating refresh credentials.
type RefreshTokenRepository interface {
	Create(ctx context.Context, t *domain.RefreshToken) error
	ByHash(ctx context.Context, hash string) (*domain.RefreshToken, error)
	Revoke(ctx context.Context, id domain.ID, replacedBy *domain.ID, at time.Time) error
	RevokeFamily(ctx context.Context, familyID domain.ID, at time.Time) error
	DeleteExpired(ctx context.Context, before time.Time) (int64, error)
}

// ProjectFilter narrows a project listing.
type ProjectFilter struct {
	OwnerID        domain.ID
	Status         domain.ProjectStatus
	Category       domain.ProjectCategory
	Query          string
	IncludeDeleted bool
	Limit          int
	Offset         int
}

// ProjectRepository persists products under construction.
type ProjectRepository interface {
	Create(ctx context.Context, p *domain.Project) error
	Update(ctx context.Context, p *domain.Project) error
	ByID(ctx context.Context, id domain.ID) (*domain.Project, error)
	BySlug(ctx context.Context, ownerID domain.ID, slug string) (*domain.Project, error)
	List(ctx context.Context, f ProjectFilter) ([]*domain.Project, int64, error)
	SoftDelete(ctx context.Context, id domain.ID, at time.Time) error
}

// RunFilter narrows a run listing.
type RunFilter struct {
	ProjectID domain.ID
	Status    domain.RunStatus
	Limit     int
	Offset    int
}

// RunRepository persists builds and their phases.
type RunRepository interface {
	Create(ctx context.Context, r *domain.Run, phases []domain.RunPhase) error
	Update(ctx context.Context, r *domain.Run) error
	ByID(ctx context.Context, id domain.ID) (*domain.Run, error)
	List(ctx context.Context, f RunFilter) ([]*domain.Run, int64, error)
	Phases(ctx context.Context, runID domain.ID) ([]domain.RunPhase, error)
	UpdatePhase(ctx context.Context, p *domain.RunPhase) error
	ActiveRuns(ctx context.Context) ([]*domain.Run, error)
}

// TaskRepository persists the agent work DAG.
type TaskRepository interface {
	CreateBatch(ctx context.Context, tasks []*domain.Task) error
	Update(ctx context.Context, t *domain.Task) error
	ByRun(ctx context.Context, runID domain.ID) ([]*domain.Task, error)
	ByID(ctx context.Context, id domain.ID) (*domain.Task, error)
}

// EventQuery is a cursor-based read over the append-only log.
type EventQuery struct {
	RunID     domain.ID
	ProjectID domain.ID
	AfterSeq  int64
	Types     []domain.EventType
	Limit     int
}

// EventRepository appends to and reads from the event log.
type EventRepository interface {
	// Append assigns a monotonic sequence number and persists the event.
	Append(ctx context.Context, e *domain.Event) error
	Query(ctx context.Context, q EventQuery) ([]*domain.Event, error)
	LatestSeq(ctx context.Context) (int64, error)
}

// ArtifactRepository persists agent outputs.
type ArtifactRepository interface {
	Create(ctx context.Context, a *domain.Artifact) error
	ByID(ctx context.Context, id domain.ID) (*domain.Artifact, error)
	ByRun(ctx context.Context, runID domain.ID) ([]*domain.Artifact, error)
	ByProjectKind(ctx context.Context, projectID domain.ID, kind domain.ArtifactKind) (*domain.Artifact, error)
	ExistsBySHA(ctx context.Context, projectID domain.ID, sha string) (*domain.Artifact, error)
}

// Publisher broadcasts events to live subscribers. Publishing is best-effort
// and must never block the caller: durability is the event log's job, delivery
// is the bus's job.
type Publisher interface {
	Publish(e *domain.Event)
}

// Subscription is a live event feed for one client.
type Subscription interface {
	Events() <-chan *domain.Event
	Gaps() <-chan Gap
	Subscribe(topics ...string)
	Unsubscribe(topics ...string)
	Close()
}

// Gap tells a client that events were dropped due to backpressure so it can
// refetch the range over REST. Dropping with a marker is strictly better than
// blocking the factory on a slow UI.
type Gap struct {
	Topic string `json:"topic"`
	From  int64  `json:"from"`
	To    int64  `json:"to"`
}

// Bus is the fan-out hub.
type Bus interface {
	Publisher
	Subscribe(buffer int, topics ...string) Subscription
	SubscriberCount() int
	Close()
}

// Recorder writes durable events and publishes them in one call. Use cases
// depend on this rather than on the repository plus the bus, so it is
// impossible to persist an event and forget to announce it.
type Recorder interface {
	Record(ctx context.Context, e *domain.Event) error
}
