package sqlstore_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/genesis-ai-factory/control-plane/internal/domain"
	"github.com/genesis-ai-factory/control-plane/internal/infra/migrate"
	"github.com/genesis-ai-factory/control-plane/internal/infra/sqlstore"
	"github.com/genesis-ai-factory/control-plane/internal/port"
)

// backend describes one engine under test. SQLite always runs; Postgres runs
// only when GENESIS_TEST_POSTGRES_DSN is set. Both execute the identical suite,
// which is what prevents the two implementations from drifting.
type backend struct {
	name   string
	driver string
	dsn    func(t *testing.T) string
}

func backends(t *testing.T) []backend {
	out := []backend{{
		name:   "sqlite",
		driver: "sqlite",
		dsn: func(t *testing.T) string {
			path := filepath.Join(t.TempDir(), "test.db")
			return "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
		},
	}}
	if dsn := os.Getenv("GENESIS_TEST_POSTGRES_DSN"); dsn != "" {
		out = append(out, backend{name: "postgres", driver: "postgres", dsn: func(*testing.T) string { return dsn }})
	}
	return out
}

func newStore(t *testing.T, b backend) *sqlstore.Store {
	t.Helper()
	ctx := context.Background()
	st, err := sqlstore.Open(ctx, sqlstore.DefaultOptions(b.driver, b.dsn(t)))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	runner := migrate.NewRunner(st.DB(), migrate.Driver(b.driver), nil)
	if b.driver == "postgres" {
		// Start from a clean schema so repeated local runs are deterministic.
		for {
			v, err := runner.Version(ctx)
			if err != nil || v == 0 {
				break
			}
			if err := runner.Down(ctx); err != nil {
				t.Fatalf("reset postgres schema: %v", err)
			}
		}
	}
	if _, err := runner.Up(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return st
}

func eachBackend(t *testing.T, fn func(t *testing.T, st *sqlstore.Store)) {
	t.Helper()
	for _, b := range backends(t) {
		b := b
		t.Run(b.name, func(t *testing.T) {
			fn(t, newStore(t, b))
		})
	}
}

func seedUser(t *testing.T, st *sqlstore.Store, email string) *domain.User {
	t.Helper()
	u, err := domain.NewUser(email, "$argon2id$fake", "Dev", domain.RoleOwner, time.Now())
	if err != nil {
		t.Fatalf("new user: %v", err)
	}
	if err := sqlstore.NewUserRepo(st).Create(context.Background(), u); err != nil {
		t.Fatalf("create user: %v", err)
	}
	return u
}

func seedProject(t *testing.T, st *sqlstore.Store, owner domain.ID, name string) *domain.Project {
	t.Helper()
	p, err := domain.NewProject(owner, name, "Build "+name, domain.DefaultProjectSettings(), time.Now())
	if err != nil {
		t.Fatalf("new project: %v", err)
	}
	if err := sqlstore.NewProjectRepo(st).Create(context.Background(), p); err != nil {
		t.Fatalf("create project: %v", err)
	}
	return p
}

func TestMigrationsAreIdempotent(t *testing.T) {
	eachBackend(t, func(t *testing.T, st *sqlstore.Store) {
		ctx := context.Background()
		runner := migrate.NewRunner(st.DB(), migrate.Driver(st.Dialect()), nil)
		applied, err := runner.Up(ctx)
		if err != nil {
			t.Fatalf("second Up failed: %v", err)
		}
		if applied != 0 {
			t.Fatalf("expected no pending migrations, applied %d", applied)
		}
		v, err := runner.Version(ctx)
		if err != nil || v < 1 {
			t.Fatalf("unexpected version %d (err %v)", v, err)
		}
	})
}

func TestUserRepositoryRoundTrip(t *testing.T) {
	eachBackend(t, func(t *testing.T, st *sqlstore.Store) {
		ctx := context.Background()
		repo := sqlstore.NewUserRepo(st)

		u := seedUser(t, st, "dev@genesis.local")

		got, err := repo.ByID(ctx, u.ID)
		if err != nil {
			t.Fatalf("by id: %v", err)
		}
		if got.Email != u.Email || got.Role != u.Role || got.Status != domain.UserActive {
			t.Fatalf("round trip mismatch: %+v", got)
		}
		if !got.CreatedAt.Equal(u.CreatedAt.UTC().Truncate(time.Nanosecond)) {
			t.Fatalf("timestamp not preserved: %s vs %s", got.CreatedAt, u.CreatedAt)
		}

		byEmail, err := repo.ByEmail(ctx, "  DEV@Genesis.Local ")
		if err != nil {
			t.Fatalf("by email should normalise: %v", err)
		}
		if byEmail.ID != u.ID {
			t.Fatal("wrong user returned by email lookup")
		}

		got.DisplayName = "Renamed"
		got.Settings = domain.Settings{"theme": "dark"}
		got.UpdatedAt = time.Now()
		if err := repo.Update(ctx, got); err != nil {
			t.Fatalf("update: %v", err)
		}
		reloaded, _ := repo.ByID(ctx, u.ID)
		if reloaded.DisplayName != "Renamed" || reloaded.Settings["theme"] != "dark" {
			t.Fatalf("update not persisted: %+v", reloaded)
		}

		n, err := repo.Count(ctx)
		if err != nil || n != 1 {
			t.Fatalf("expected 1 user, got %d (err %v)", n, err)
		}
	})
}

func TestUserEmailUniqueness(t *testing.T) {
	eachBackend(t, func(t *testing.T, st *sqlstore.Store) {
		ctx := context.Background()
		repo := sqlstore.NewUserRepo(st)
		seedUser(t, st, "dup@genesis.local")

		other, _ := domain.NewUser("dup@genesis.local", "$argon2id$fake", "Other", domain.RoleMember, time.Now())
		err := repo.Create(ctx, other)
		if !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("expected conflict on duplicate email, got %v", err)
		}
	})
}

func TestUserNotFound(t *testing.T) {
	eachBackend(t, func(t *testing.T, st *sqlstore.Store) {
		_, err := sqlstore.NewUserRepo(st).ByID(context.Background(), domain.NewID())
		if !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("expected not found sentinel, got %v", err)
		}
	})
}

func TestRefreshTokenRotationAndFamilyRevocation(t *testing.T) {
	eachBackend(t, func(t *testing.T, st *sqlstore.Store) {
		ctx := context.Background()
		u := seedUser(t, st, "tokens@genesis.local")
		repo := sqlstore.NewRefreshTokenRepo(st)

		family := domain.NewID()
		now := time.Now()
		mk := func(hash string) *domain.RefreshToken {
			return &domain.RefreshToken{
				ID: domain.NewID(), UserID: u.ID, TokenHash: hash, FamilyID: family,
				ExpiresAt: now.Add(24 * time.Hour), CreatedAt: now,
			}
		}
		first, second := mk("hash-1"), mk("hash-2")
		if err := repo.Create(ctx, first); err != nil {
			t.Fatalf("create first: %v", err)
		}
		if err := repo.Create(ctx, second); err != nil {
			t.Fatalf("create second: %v", err)
		}

		got, err := repo.ByHash(ctx, "hash-1")
		if err != nil {
			t.Fatalf("by hash: %v", err)
		}
		if !got.Active(now) {
			t.Fatal("fresh token should be active")
		}

		if err := repo.Revoke(ctx, first.ID, &second.ID, now); err != nil {
			t.Fatalf("revoke: %v", err)
		}
		got, _ = repo.ByHash(ctx, "hash-1")
		if got.Active(now) {
			t.Fatal("revoked token still active")
		}
		if got.ReplacedBy == nil || *got.ReplacedBy != second.ID {
			t.Fatal("rotation link not recorded")
		}

		// Reuse detection kills the whole lineage.
		if err := repo.RevokeFamily(ctx, family, now); err != nil {
			t.Fatalf("revoke family: %v", err)
		}
		got, _ = repo.ByHash(ctx, "hash-2")
		if got.Active(now) {
			t.Fatal("family revocation did not invalidate sibling token")
		}
	})
}

func TestRefreshTokenExpiryCleanup(t *testing.T) {
	eachBackend(t, func(t *testing.T, st *sqlstore.Store) {
		ctx := context.Background()
		u := seedUser(t, st, "cleanup@genesis.local")
		repo := sqlstore.NewRefreshTokenRepo(st)
		now := time.Now()

		expired := &domain.RefreshToken{
			ID: domain.NewID(), UserID: u.ID, TokenHash: "old", FamilyID: domain.NewID(),
			ExpiresAt: now.Add(-time.Hour), CreatedAt: now.Add(-2 * time.Hour),
		}
		live := &domain.RefreshToken{
			ID: domain.NewID(), UserID: u.ID, TokenHash: "new", FamilyID: domain.NewID(),
			ExpiresAt: now.Add(time.Hour), CreatedAt: now,
		}
		_ = repo.Create(ctx, expired)
		_ = repo.Create(ctx, live)

		n, err := repo.DeleteExpired(ctx, now)
		if err != nil {
			t.Fatalf("delete expired: %v", err)
		}
		if n != 1 {
			t.Fatalf("expected 1 deletion, got %d", n)
		}
		if _, err := repo.ByHash(ctx, "new"); err != nil {
			t.Fatalf("live token was deleted: %v", err)
		}
	})
}

func TestProjectRepositoryCRUDAndFilters(t *testing.T) {
	eachBackend(t, func(t *testing.T, st *sqlstore.Store) {
		ctx := context.Background()
		u := seedUser(t, st, "proj@genesis.local")
		repo := sqlstore.NewProjectRepo(st)

		crm := seedProject(t, st, u.ID, "CRM System")
		pm := seedProject(t, st, u.ID, "Jira Competitor")

		got, err := repo.ByID(ctx, crm.ID)
		if err != nil {
			t.Fatalf("by id: %v", err)
		}
		if got.Slug != "crm-system" || got.Settings.Autonomy != domain.AutonomyCheckpointed {
			t.Fatalf("project round trip wrong: %+v", got)
		}

		bySlug, err := repo.BySlug(ctx, u.ID, "jira-competitor")
		if err != nil || bySlug.ID != pm.ID {
			t.Fatalf("by slug failed: %v", err)
		}

		dup, _ := domain.NewProject(u.ID, "CRM System", "another crm", domain.DefaultProjectSettings(), time.Now())
		if err := repo.Create(ctx, dup); !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("expected slug conflict, got %v", err)
		}

		all, total, err := repo.List(ctx, port.ProjectFilter{OwnerID: u.ID})
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if total != 2 || len(all) != 2 {
			t.Fatalf("expected 2 projects, got %d/%d", len(all), total)
		}
		// Newest first.
		if all[0].ID != pm.ID {
			t.Fatal("listing is not ordered by recency")
		}

		found, _, err := repo.List(ctx, port.ProjectFilter{OwnerID: u.ID, Query: "jira"})
		if err != nil || len(found) != 1 || found[0].ID != pm.ID {
			t.Fatalf("search failed: %v %+v", err, found)
		}

		if err := repo.SoftDelete(ctx, crm.ID, time.Now()); err != nil {
			t.Fatalf("soft delete: %v", err)
		}
		remaining, total, _ := repo.List(ctx, port.ProjectFilter{OwnerID: u.ID})
		if total != 1 || len(remaining) != 1 {
			t.Fatalf("soft-deleted project still listed: %d", total)
		}
		withDeleted, _, _ := repo.List(ctx, port.ProjectFilter{OwnerID: u.ID, IncludeDeleted: true})
		if len(withDeleted) != 2 {
			t.Fatalf("include_deleted should return 2, got %d", len(withDeleted))
		}
		if err := repo.SoftDelete(ctx, crm.ID, time.Now()); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("double delete should be not found, got %v", err)
		}
	})
}

func TestProjectListPagination(t *testing.T) {
	eachBackend(t, func(t *testing.T, st *sqlstore.Store) {
		ctx := context.Background()
		u := seedUser(t, st, "page@genesis.local")
		repo := sqlstore.NewProjectRepo(st)
		for i := 0; i < 7; i++ {
			seedProject(t, st, u.ID, fmt.Sprintf("Product %d", i))
		}
		first, total, err := repo.List(ctx, port.ProjectFilter{OwnerID: u.ID, Limit: 3})
		if err != nil || total != 7 || len(first) != 3 {
			t.Fatalf("page 1 wrong: len=%d total=%d err=%v", len(first), total, err)
		}
		second, _, _ := repo.List(ctx, port.ProjectFilter{OwnerID: u.ID, Limit: 3, Offset: 3})
		if len(second) != 3 || second[0].ID == first[0].ID {
			t.Fatal("pagination returned overlapping pages")
		}
	})
}

func TestRunRepositoryLifecycle(t *testing.T) {
	eachBackend(t, func(t *testing.T, st *sqlstore.Store) {
		ctx := context.Background()
		u := seedUser(t, st, "runs@genesis.local")
		p := seedProject(t, st, u.ID, "Run Target")
		repo := sqlstore.NewRunRepo(st)

		now := time.Now()
		run, err := domain.NewRun(p.ID, u.ID, domain.RunBuild, domain.Settings{"prompt": p.Prompt}, 1000, now)
		if err != nil {
			t.Fatalf("new run: %v", err)
		}
		phases := domain.NewRunPhases(run.ID, now)
		if err := repo.Create(ctx, run, phases); err != nil {
			t.Fatalf("create run: %v", err)
		}

		got, err := repo.ByID(ctx, run.ID)
		if err != nil {
			t.Fatalf("by id: %v", err)
		}
		if len(got.Phases) != len(domain.PhaseOrder) {
			t.Fatalf("expected %d phases, got %d", len(domain.PhaseOrder), len(got.Phases))
		}
		if got.Input["prompt"] != p.Prompt {
			t.Fatal("run input snapshot not preserved")
		}

		// Phase transition.
		ph := got.Phases[0]
		start := time.Now()
		ph.Status = domain.PhaseRunning
		ph.StartedAt = &start
		ph.Summary = domain.Settings{"agents": 2}
		ph.UpdatedAt = start
		if err := repo.UpdatePhase(ctx, &ph); err != nil {
			t.Fatalf("update phase: %v", err)
		}

		// Run transition.
		_ = got.Start(time.Now())
		if err := repo.Update(ctx, got); err != nil {
			t.Fatalf("update run: %v", err)
		}
		active, err := repo.ActiveRuns(ctx)
		if err != nil || len(active) != 1 {
			t.Fatalf("expected 1 active run, got %d (err %v)", len(active), err)
		}

		_ = got.Fail(domain.RunError{Code: "boom", Message: "exploded", Phase: domain.PhaseBuild, Agent: domain.RoleBackend}, time.Now())
		if err := repo.Update(ctx, got); err != nil {
			t.Fatalf("fail run: %v", err)
		}
		reloaded, _ := repo.ByID(ctx, run.ID)
		if reloaded.Error == nil || reloaded.Error.Code != "boom" || reloaded.Error.Agent != domain.RoleBackend {
			t.Fatalf("typed error not persisted: %+v", reloaded.Error)
		}
		if reloaded.Phases[0].Status != domain.PhaseRunning || reloaded.Phases[0].Summary["agents"] == nil {
			t.Fatalf("phase update lost: %+v", reloaded.Phases[0])
		}
		active, _ = repo.ActiveRuns(ctx)
		if len(active) != 0 {
			t.Fatalf("terminal run still reported active: %d", len(active))
		}
	})
}

func TestEventLogOrderingAndCursor(t *testing.T) {
	eachBackend(t, func(t *testing.T, st *sqlstore.Store) {
		ctx := context.Background()
		u := seedUser(t, st, "events@genesis.local")
		p := seedProject(t, st, u.ID, "Event Source")
		runRepo := sqlstore.NewRunRepo(st)
		run, _ := domain.NewRun(p.ID, u.ID, domain.RunBuild, nil, 0, time.Now())
		_ = runRepo.Create(ctx, run, domain.NewRunPhases(run.ID, time.Now()))

		repo := sqlstore.NewEventRepo(st)
		const n = 25
		for i := 0; i < n; i++ {
			e := domain.NewEvent(domain.RunTopic(run.ID), domain.EventLog, domain.LevelInfo,
				fmt.Sprintf("message %d", i)).For(run.ID, p.ID).With("i", i)
			if err := repo.Append(ctx, e); err != nil {
				t.Fatalf("append %d: %v", i, err)
			}
			if e.Seq == 0 {
				t.Fatalf("sequence not assigned for event %d", i)
			}
		}

		all, err := repo.Query(ctx, port.EventQuery{RunID: run.ID, Limit: 100})
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		if len(all) != n {
			t.Fatalf("expected %d events, got %d", n, len(all))
		}
		for i := 1; i < len(all); i++ {
			if all[i].Seq <= all[i-1].Seq {
				t.Fatalf("sequence not monotonic at %d: %d <= %d", i, all[i].Seq, all[i-1].Seq)
			}
		}
		if all[0].Payload["i"] == nil || all[0].Message != "message 0" {
			t.Fatalf("payload lost: %+v", all[0])
		}

		// Cursor resume must be gapless and duplicate-free.
		cursor := all[9].Seq
		rest, err := repo.Query(ctx, port.EventQuery{RunID: run.ID, AfterSeq: cursor, Limit: 100})
		if err != nil {
			t.Fatalf("cursor query: %v", err)
		}
		if len(rest) != n-10 {
			t.Fatalf("expected %d events after cursor, got %d", n-10, len(rest))
		}
		if rest[0].Seq != all[10].Seq {
			t.Fatal("cursor resume skipped or repeated an event")
		}

		filtered, _ := repo.Query(ctx, port.EventQuery{RunID: run.ID, Types: []domain.EventType{domain.EventRunCreated}})
		if len(filtered) != 0 {
			t.Fatalf("type filter returned unrelated events: %d", len(filtered))
		}

		head, err := repo.LatestSeq(ctx)
		if err != nil || head != all[len(all)-1].Seq {
			t.Fatalf("latest seq wrong: %d (err %v)", head, err)
		}
	})
}

func TestEventAppendIsConcurrencySafe(t *testing.T) {
	eachBackend(t, func(t *testing.T, st *sqlstore.Store) {
		ctx := context.Background()
		u := seedUser(t, st, "concurrent@genesis.local")
		p := seedProject(t, st, u.ID, "Concurrent")
		runRepo := sqlstore.NewRunRepo(st)
		run, _ := domain.NewRun(p.ID, u.ID, domain.RunBuild, nil, 0, time.Now())
		_ = runRepo.Create(ctx, run, domain.NewRunPhases(run.ID, time.Now()))

		repo := sqlstore.NewEventRepo(st)
		const writers, each = 8, 20

		var wg sync.WaitGroup
		errCh := make(chan error, writers*each)
		for w := 0; w < writers; w++ {
			wg.Add(1)
			go func(w int) {
				defer wg.Done()
				for i := 0; i < each; i++ {
					e := domain.NewEvent(domain.RunTopic(run.ID), domain.EventLog, domain.LevelInfo,
						fmt.Sprintf("w%d-%d", w, i)).For(run.ID, p.ID)
					if err := repo.Append(ctx, e); err != nil {
						errCh <- err
						return
					}
				}
			}(w)
		}
		wg.Wait()
		close(errCh)
		for err := range errCh {
			t.Fatalf("concurrent append failed: %v", err)
		}

		all, _ := repo.Query(ctx, port.EventQuery{RunID: run.ID, Limit: 1000})
		if len(all) != writers*each {
			t.Fatalf("expected %d events, got %d", writers*each, len(all))
		}
		seen := map[int64]bool{}
		for _, e := range all {
			if seen[e.Seq] {
				t.Fatalf("duplicate sequence %d", e.Seq)
			}
			seen[e.Seq] = true
		}
	})
}

func TestArtifactDedupAndLookup(t *testing.T) {
	eachBackend(t, func(t *testing.T, st *sqlstore.Store) {
		ctx := context.Background()
		u := seedUser(t, st, "art@genesis.local")
		p := seedProject(t, st, u.ID, "Artifacts")
		runRepo := sqlstore.NewRunRepo(st)
		run, _ := domain.NewRun(p.ID, u.ID, domain.RunBuild, nil, 0, time.Now())
		_ = runRepo.Create(ctx, run, domain.NewRunPhases(run.ID, time.Now()))

		repo := sqlstore.NewArtifactRepo(st)
		a := domain.NewArtifact(p.ID, run.ID, nil, domain.ArtifactPRD, "prd.md", "text/markdown", "# PRD\ncontent", time.Now())
		if err := repo.Create(ctx, a); err != nil {
			t.Fatalf("create artifact: %v", err)
		}

		dup := domain.NewArtifact(p.ID, run.ID, nil, domain.ArtifactPRD, "prd.md", "text/markdown", "# PRD\ncontent", time.Now())
		if err := repo.Create(ctx, dup); !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("expected dedup conflict, got %v", err)
		}

		existing, err := repo.ExistsBySHA(ctx, p.ID, a.SHA256)
		if err != nil || existing == nil || existing.ID != a.ID {
			t.Fatalf("dedup lookup failed: %v %+v", err, existing)
		}
		missing, err := repo.ExistsBySHA(ctx, p.ID, strings.Repeat("0", 64))
		if err != nil || missing != nil {
			t.Fatalf("expected nil for unknown sha, got %+v (err %v)", missing, err)
		}

		latest := domain.NewArtifact(p.ID, run.ID, nil, domain.ArtifactPRD, "prd.md", "text/markdown", "# PRD v2", time.Now().Add(time.Second))
		if err := repo.Create(ctx, latest); err != nil {
			t.Fatalf("create v2: %v", err)
		}
		got, err := repo.ByProjectKind(ctx, p.ID, domain.ArtifactPRD)
		if err != nil || got.ID != latest.ID {
			t.Fatalf("ByProjectKind should return newest: %v %+v", err, got)
		}

		list, err := repo.ByRun(ctx, run.ID)
		if err != nil || len(list) != 2 {
			t.Fatalf("expected 2 artifacts for run, got %d (err %v)", len(list), err)
		}
	})
}

func TestTaskDAGPersistence(t *testing.T) {
	eachBackend(t, func(t *testing.T, st *sqlstore.Store) {
		ctx := context.Background()
		u := seedUser(t, st, "tasks@genesis.local")
		p := seedProject(t, st, u.ID, "Tasks")
		runRepo := sqlstore.NewRunRepo(st)
		now := time.Now()
		run, _ := domain.NewRun(p.ID, u.ID, domain.RunBuild, nil, 0, now)
		phases := domain.NewRunPhases(run.ID, now)
		_ = runRepo.Create(ctx, run, phases)

		repo := sqlstore.NewTaskRepo(st)
		vision := domain.NewTask(run.ID, phases[0].ID, domain.RoleCEO, "Vision", "define strategy", 100, now)
		prd := domain.NewTask(run.ID, phases[0].ID, domain.RolePM, "PRD", "write requirements", 90, now).
			DependsOnTasks(vision.ID)
		if err := repo.CreateBatch(ctx, []*domain.Task{vision, prd}); err != nil {
			t.Fatalf("create batch: %v", err)
		}

		tasks, err := repo.ByRun(ctx, run.ID)
		if err != nil || len(tasks) != 2 {
			t.Fatalf("expected 2 tasks, got %d (err %v)", len(tasks), err)
		}
		if tasks[0].Priority < tasks[1].Priority {
			t.Fatal("tasks not ordered by priority")
		}
		var loadedPRD *domain.Task
		for _, tk := range tasks {
			if tk.AgentRole == domain.RolePM {
				loadedPRD = tk
			}
		}
		if loadedPRD == nil || len(loadedPRD.DependsOn) != 1 || loadedPRD.DependsOn[0] != vision.ID {
			t.Fatalf("DAG edge lost: %+v", loadedPRD)
		}

		if err := loadedPRD.Claim(time.Now()); err != nil {
			t.Fatalf("claim: %v", err)
		}
		loadedPRD.Succeed(domain.Settings{"artifact": "prd.md"}, time.Now())
		if err := repo.Update(ctx, loadedPRD); err != nil {
			t.Fatalf("update task: %v", err)
		}
		reloaded, _ := repo.ByID(ctx, loadedPRD.ID)
		if reloaded.Status != domain.TaskSucceeded || reloaded.Output["artifact"] != "prd.md" || reloaded.AttemptCount != 1 {
			t.Fatalf("task update lost: %+v", reloaded)
		}
	})
}

func TestTransactionRollback(t *testing.T) {
	eachBackend(t, func(t *testing.T, st *sqlstore.Store) {
		ctx := context.Background()
		users := sqlstore.NewUserRepo(st)
		projects := sqlstore.NewProjectRepo(st)
		u := seedUser(t, st, "tx@genesis.local")

		sentinel := errors.New("rollback please")
		err := st.WithTx(ctx, func(ctx context.Context) error {
			p, _ := domain.NewProject(u.ID, "Doomed", "build something", domain.DefaultProjectSettings(), time.Now())
			if err := projects.Create(ctx, p); err != nil {
				return err
			}
			return sentinel
		})
		if !errors.Is(err, sentinel) {
			t.Fatalf("expected sentinel error, got %v", err)
		}
		_, total, _ := projects.List(ctx, port.ProjectFilter{OwnerID: u.ID})
		if total != 0 {
			t.Fatalf("rollback did not undo the insert: %d rows", total)
		}

		// Commit path.
		err = st.WithTx(ctx, func(ctx context.Context) error {
			p, _ := domain.NewProject(u.ID, "Committed", "build something", domain.DefaultProjectSettings(), time.Now())
			return projects.Create(ctx, p)
		})
		if err != nil {
			t.Fatalf("commit path failed: %v", err)
		}
		_, total, _ = projects.List(ctx, port.ProjectFilter{OwnerID: u.ID})
		if total != 1 {
			t.Fatalf("expected 1 committed project, got %d", total)
		}

		// Nested WithTx must join, not deadlock.
		done := make(chan error, 1)
		go func() {
			done <- st.WithTx(ctx, func(ctx context.Context) error {
				return st.WithTx(ctx, func(ctx context.Context) error {
					_, err := users.Count(ctx)
					return err
				})
			})
		}()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("nested transaction failed: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("nested transaction deadlocked")
		}
	})
}
