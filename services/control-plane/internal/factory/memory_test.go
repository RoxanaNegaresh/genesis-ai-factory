package factory_test

import (
	"context"
	"testing"

	"github.com/genesis-ai-factory/control-plane/internal/factory"
	"github.com/genesis-ai-factory/control-plane/internal/port"
)

// fakeEmbedder produces deterministic vectors from token hashes.
//
// Real embeddings are not needed to test retrieval mechanics; what must be
// verified is that similar text scores higher than dissimilar text and that
// scope isolation holds. A deterministic stand-in makes those assertions exact
// instead of probabilistic.
type fakeEmbedder struct{ dims int }

func (f *fakeEmbedder) Dimensions() int             { return f.dims }
func (f *fakeEmbedder) Ready(context.Context) error { return nil }

func (f *fakeEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, 0, len(texts))
	for _, text := range texts {
		vector := make([]float32, f.dims)
		for _, word := range splitWords(text) {
			vector[hashWord(word)%uint32(f.dims)] += 1
		}
		out = append(out, vector)
	}
	return out, nil
}

func splitWords(s string) []string {
	var (
		words   []string
		current []rune
	)
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
			current = append(current, r|0x20)
			continue
		}
		if len(current) > 0 {
			words = append(words, string(current))
			current = nil
		}
	}
	if len(current) > 0 {
		words = append(words, string(current))
	}
	return words
}

func hashWord(s string) uint32 {
	var h uint32 = 2166136261
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= 16777619
	}
	return h
}

func newMemory(t *testing.T, withEmbedder bool) *factory.MemoryService {
	t.Helper()
	var embedder port.Embedder
	if withEmbedder {
		embedder = &fakeEmbedder{dims: 64}
	}
	return factory.NewMemoryService(factory.NewInMemoryStore(), embedder)
}

func TestMemoryStoresAndRecalls(t *testing.T) {
	ctx := context.Background()
	memory := newMemory(t, true)

	stored, err := memory.Remember(ctx, port.Memory{
		Scope: port.ScopeProject, ProjectID: "p1", Kind: port.KindDecision,
		Title: "Database choice", Content: "Use PostgreSQL for relational integrity and JSONB support",
	})
	if err != nil {
		t.Fatalf("remember: %v", err)
	}
	if stored.ID == "" || stored.CreatedAt.IsZero() {
		t.Fatalf("memory not initialised: %+v", stored)
	}

	hits, err := memory.Recall(ctx, port.MemoryQuery{
		Text: "which database should we use", ProjectID: "p1",
		Scopes: []port.MemoryScope{port.ScopeProject}, Limit: 5,
	})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("stored memory was not recalled")
	}
	if hits[0].Memory.Title != "Database choice" {
		t.Fatalf("wrong memory recalled: %+v", hits[0].Memory)
	}
}

func TestMemoryRanksRelevantResultsFirst(t *testing.T) {
	ctx := context.Background()
	memory := newMemory(t, true)

	for _, m := range []port.Memory{
		{Scope: port.ScopeProject, ProjectID: "p1", Kind: port.KindDecision,
			Title: "Database choice", Content: "PostgreSQL chosen for relational integrity"},
		{Scope: port.ScopeProject, ProjectID: "p1", Kind: port.KindPreference,
			Title: "Button colour", Content: "Primary buttons use indigo"},
		{Scope: port.ScopeProject, ProjectID: "p1", Kind: port.KindDecision,
			Title: "Deployment target", Content: "Kubernetes with a managed ingress"},
	} {
		if _, err := memory.Remember(ctx, m); err != nil {
			t.Fatalf("remember: %v", err)
		}
	}

	hits, err := memory.Recall(ctx, port.MemoryQuery{
		Text: "PostgreSQL relational database integrity", ProjectID: "p1", Limit: 3,
	})
	if err != nil || len(hits) == 0 {
		t.Fatalf("recall failed: %v (%d hits)", err, len(hits))
	}
	if hits[0].Memory.Title != "Database choice" {
		t.Fatalf("most relevant memory not ranked first, got %q", hits[0].Memory.Title)
	}
	// Scores must be ordered, or "top-k" is meaningless.
	for i := 1; i < len(hits); i++ {
		if hits[i].Score > hits[i-1].Score {
			t.Fatalf("results are not ordered by score: %v", hits)
		}
	}
}

// Cross-project leakage would let one product's decisions contaminate another.
func TestMemoryIsolatesProjects(t *testing.T) {
	ctx := context.Background()
	memory := newMemory(t, true)

	_, _ = memory.Remember(ctx, port.Memory{
		Scope: port.ScopeProject, ProjectID: "alpha", Kind: port.KindDecision,
		Title: "Alpha secret", Content: "Alpha uses event sourcing throughout",
	})
	_, _ = memory.Remember(ctx, port.Memory{
		Scope: port.ScopeProject, ProjectID: "beta", Kind: port.KindDecision,
		Title: "Beta choice", Content: "Beta uses simple CRUD",
	})

	hits, err := memory.Recall(ctx, port.MemoryQuery{
		Text: "event sourcing", ProjectID: "beta",
		Scopes: []port.MemoryScope{port.ScopeProject}, Limit: 10,
	})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	for _, hit := range hits {
		if hit.Memory.ProjectID != "beta" {
			t.Fatalf("memory leaked across projects: %+v", hit.Memory)
		}
	}
}

func TestMemoryDeduplicates(t *testing.T) {
	ctx := context.Background()
	memory := newMemory(t, false)

	entry := port.Memory{
		Scope: port.ScopeProject, ProjectID: "p1", Kind: port.KindDecision,
		Title: "Repeated", Content: "The same decision recorded on every run",
	}
	for i := 0; i < 5; i++ {
		if _, err := memory.Remember(ctx, entry); err != nil {
			t.Fatalf("remember: %v", err)
		}
	}

	store := factory.NewInMemoryStore()
	service := factory.NewMemoryService(store, nil)
	for i := 0; i < 5; i++ {
		_, _ = service.Remember(ctx, entry)
	}
	count, err := store.Count(ctx)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Fatalf("identical memories must be deduplicated, found %d", count)
	}
}

// Memory must work with no embedding model available, or the local-first
// requirement fails on a machine without one.
func TestMemoryWorksWithoutEmbedder(t *testing.T) {
	ctx := context.Background()
	memory := newMemory(t, false)

	_, err := memory.Remember(ctx, port.Memory{
		Scope: port.ScopeProject, ProjectID: "p1", Kind: port.KindLesson,
		Title: "Migration ordering", Content: "Foreign keys require dependency-ordered table creation",
	})
	if err != nil {
		t.Fatalf("remember without embedder: %v", err)
	}

	hits, err := memory.Recall(ctx, port.MemoryQuery{
		Text: "foreign keys dependency ordering", ProjectID: "p1", Limit: 5,
	})
	if err != nil {
		t.Fatalf("recall without embedder: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("lexical retrieval must still work without an embedder")
	}
}

func TestMemoryRespectsMinScore(t *testing.T) {
	ctx := context.Background()
	memory := newMemory(t, false)

	_, _ = memory.Remember(ctx, port.Memory{
		Scope: port.ScopeProject, ProjectID: "p1", Kind: port.KindDecision,
		Title: "Completely unrelated", Content: "Zebras graze on savannah grassland",
	})

	hits, err := memory.Recall(ctx, port.MemoryQuery{
		Text:      "database indexing strategy for the sales pipeline",
		ProjectID: "p1", Limit: 5, MinScore: 0.5,
	})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("irrelevant memory passed the score threshold: %+v", hits)
	}
}

func TestNilMemoryServiceIsSafe(t *testing.T) {
	// Agents hold a possibly-nil memory service; it must never panic.
	var service *factory.MemoryService
	if _, err := service.Remember(context.Background(), port.Memory{Title: "x"}); err != nil {
		t.Fatalf("nil service should be a no-op, got %v", err)
	}
	hits, err := service.Recall(context.Background(), port.MemoryQuery{Text: "x"})
	if err != nil || hits != nil {
		t.Fatalf("nil service recall should be empty, got %v / %v", hits, err)
	}
}
