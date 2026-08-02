package factory

import (
	"context"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/genesis-ai-factory/control-plane/internal/domain"
	"github.com/genesis-ai-factory/control-plane/internal/port"
)

// MemoryService is the long-term memory of the factory.
//
// Design note on the retrieval strategy: this uses hybrid scoring — lexical
// overlap always, plus cosine similarity when an embedder is configured. A
// pure-vector store would make memory unavailable on a machine with no
// embedding model, which contradicts the local-first requirement; pure lexical
// retrieval misses paraphrase. Combining them degrades gracefully in exactly
// the right direction: always useful, better with a model.
type MemoryService struct {
	store    port.MemoryStore
	embedder port.Embedder
	clock    func() time.Time
}

// NewMemoryService constructs the service. The embedder may be nil.
func NewMemoryService(store port.MemoryStore, embedder port.Embedder) *MemoryService {
	return &MemoryService{store: store, embedder: embedder, clock: time.Now}
}

// Remember stores a memory, embedding it when possible.
func (m *MemoryService) Remember(ctx context.Context, memory port.Memory) (port.Memory, error) {
	if m == nil || m.store == nil {
		return memory, nil
	}
	if memory.ID == "" {
		memory.ID = domain.NewID().String()
	}
	if memory.CreatedAt.IsZero() {
		memory.CreatedAt = m.clock().UTC()
	}
	if memory.Importance == 0 {
		memory.Importance = 0.5
	}

	if m.embedder != nil {
		if vectors, err := m.embedder.Embed(ctx, []string{memory.Title + "\n" + memory.Content}); err == nil && len(vectors) == 1 {
			memory.Vector = vectors[0]
		}
	}
	return m.store.Remember(ctx, memory)
}

// Recall retrieves relevant memories.
func (m *MemoryService) Recall(ctx context.Context, query port.MemoryQuery) ([]port.MemoryHit, error) {
	if m == nil || m.store == nil {
		return nil, nil
	}
	if m.embedder != nil && strings.TrimSpace(query.Text) != "" {
		// The store scores against the vector when one is supplied; computing
		// it here keeps the embedder out of the storage layer.
		if vectors, err := m.embedder.Embed(ctx, []string{query.Text}); err == nil && len(vectors) == 1 {
			ctx = withQueryVector(ctx, vectors[0])
		}
	}
	return m.store.Recall(ctx, query)
}

type queryVectorKey struct{}

func withQueryVector(ctx context.Context, vector []float32) context.Context {
	return context.WithValue(ctx, queryVectorKey{}, vector)
}

func queryVectorFrom(ctx context.Context) []float32 {
	v, _ := ctx.Value(queryVectorKey{}).([]float32)
	return v
}

// InMemoryStore is a dependency-free memory store.
//
// It is the default so that memory works out of the box with no Qdrant
// container. Qdrant becomes worthwhile at a scale a single desktop project does
// not reach; shipping a required external service for a feature that a map
// serves correctly would be misplaced engineering.
type InMemoryStore struct {
	mu       sync.RWMutex
	memories map[string]port.Memory
}

// NewInMemoryStore constructs an empty store.
func NewInMemoryStore() *InMemoryStore {
	return &InMemoryStore{memories: map[string]port.Memory{}}
}

var _ port.MemoryStore = (*InMemoryStore)(nil)

// Remember stores or updates a memory, deduplicating near-identical content.
func (s *InMemoryStore) Remember(ctx context.Context, memory port.Memory) (port.Memory, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Deduplicate: an agent that records the same decision on every run would
	// otherwise flood retrieval with copies and crowd out everything else.
	normalized := normalizeText(memory.Title + " " + memory.Content)
	for id, existing := range s.memories {
		if existing.Scope != memory.Scope || existing.ProjectID != memory.ProjectID {
			continue
		}
		if normalizeText(existing.Title+" "+existing.Content) == normalized {
			existing.UseCount++
			existing.Importance = maxFloat32(existing.Importance, memory.Importance)
			s.memories[id] = existing
			return existing, nil
		}
	}

	if memory.ID == "" {
		memory.ID = domain.NewID().String()
	}
	s.memories[memory.ID] = memory
	return memory, nil
}

// Recall scores every memory in scope and returns the best matches.
func (s *InMemoryStore) Recall(ctx context.Context, query port.MemoryQuery) ([]port.MemoryHit, error) {
	s.mu.RLock()
	candidates := make([]port.Memory, 0, len(s.memories))
	for _, memory := range s.memories {
		if !matchesScope(memory, query) {
			continue
		}
		candidates = append(candidates, memory)
	}
	s.mu.RUnlock()

	queryVector := queryVectorFrom(ctx)
	queryTerms := tokenize(query.Text)

	hits := make([]port.MemoryHit, 0, len(candidates))
	for _, memory := range candidates {
		lexical := lexicalScore(queryTerms, tokenize(memory.Title+" "+memory.Content))

		var semantic float32
		if len(queryVector) > 0 && len(memory.Vector) == len(queryVector) {
			semantic = cosineSimilarity(queryVector, memory.Vector)
		}

		// Weighting favours semantic similarity when available, because
		// lexical overlap alone rewards shared stopwords more than shared
		// meaning; importance breaks ties toward decisions the system marked
		// as consequential.
		score := lexical
		if semantic > 0 {
			score = 0.35*lexical + 0.65*semantic
		}
		score += 0.05 * memory.Importance

		if score < query.MinScore {
			continue
		}
		hits = append(hits, port.MemoryHit{Memory: memory, Score: score})
	}

	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].Score != hits[j].Score {
			return hits[i].Score > hits[j].Score
		}
		// Deterministic tiebreak keeps recall reproducible across runs.
		return hits[i].Memory.ID < hits[j].Memory.ID
	})

	limit := query.Limit
	if limit <= 0 || limit > len(hits) {
		limit = len(hits)
	}
	hits = hits[:limit]

	// Record usage so unused memories can be pruned later; memory that is
	// never retrieved is what erodes retrieval precision as a project ages.
	if len(hits) > 0 {
		now := time.Now().UTC()
		s.mu.Lock()
		for _, hit := range hits {
			if stored, ok := s.memories[hit.Memory.ID]; ok {
				stored.UseCount++
				stored.LastUsedAt = &now
				s.memories[hit.Memory.ID] = stored
			}
		}
		s.mu.Unlock()
	}
	return hits, nil
}

// Forget removes a memory.
func (s *InMemoryStore) Forget(ctx context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.memories, id)
	return nil
}

// Count reports how many memories are stored.
func (s *InMemoryStore) Count(ctx context.Context) (int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.memories), nil
}

func matchesScope(memory port.Memory, query port.MemoryQuery) bool {
	if len(query.Scopes) > 0 {
		found := false
		for _, scope := range query.Scopes {
			if memory.Scope == scope {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	if len(query.Kinds) > 0 {
		found := false
		for _, kind := range query.Kinds {
			if memory.Kind == kind {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	// A project-scoped memory must never leak into a different project.
	if memory.Scope == port.ScopeProject && query.ProjectID != "" && memory.ProjectID != query.ProjectID {
		return false
	}
	if memory.Scope == port.ScopeUser && query.UserID != "" && memory.UserID != query.UserID {
		return false
	}
	return true
}

// stopwords are excluded from lexical scoring: matching on "the" is noise.
var stopwords = map[string]bool{
	"the": true, "a": true, "an": true, "and": true, "or": true, "of": true,
	"to": true, "in": true, "for": true, "on": true, "with": true, "is": true,
	"are": true, "be": true, "it": true, "that": true, "this": true, "as": true,
	"at": true, "by": true, "from": true, "we": true, "should": true, "must": true,
}

func tokenize(text string) map[string]int {
	terms := map[string]int{}
	for _, raw := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9')
	}) {
		if len(raw) < 3 || stopwords[raw] {
			continue
		}
		terms[raw]++
	}
	return terms
}

// lexicalScore is a Jaccard-style overlap normalised by the query length, so a
// long memory does not outrank a precise one merely by containing more words.
func lexicalScore(query, document map[string]int) float32 {
	if len(query) == 0 {
		return 0
	}
	matched := 0
	for term := range query {
		if document[term] > 0 {
			matched++
		}
	}
	return float32(matched) / float32(len(query))
}

func cosineSimilarity(a, b []float32) float32 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, normA, normB float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		normA += float64(a[i]) * float64(a[i])
		normB += float64(b[i]) * float64(b[i])
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return float32(dot / (math.Sqrt(normA) * math.Sqrt(normB)))
}

func normalizeText(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(s)), " ")
}

func maxFloat32(a, b float32) float32 {
	if a > b {
		return a
	}
	return b
}
