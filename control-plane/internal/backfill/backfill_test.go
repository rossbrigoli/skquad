package backfill

import (
	"context"
	"errors"
	"testing"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
)

type fakeStore struct {
	pending []*domain.AgentMemory
	stored  map[string][]float64
	models  map[string]string
	setErr  error
}

func (f *fakeStore) ListMemoriesNeedingEmbedding(_ context.Context, currentModel string, limit int) ([]*domain.AgentMemory, error) {
	out := make([]*domain.AgentMemory, 0, limit)
	for _, m := range f.pending {
		if f.models[m.ID] == currentModel {
			continue // already current
		}
		out = append(out, m)
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

func (f *fakeStore) SetAgentMemoryEmbedding(_ context.Context, id string, embedding []float64, model string) error {
	if f.setErr != nil {
		return f.setErr
	}
	f.stored[id] = embedding
	f.models[id] = model
	return nil
}

type fakeEmbedder struct {
	calls int
	err   error
}

func (f *fakeEmbedder) Embed(_ context.Context, _ string) ([]float64, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return []float64{float64(f.calls), 0.5}, nil
}

func newStore(ids ...string) *fakeStore {
	s := &fakeStore{stored: map[string][]float64{}, models: map[string]string{}}
	for _, id := range ids {
		s.pending = append(s.pending, &domain.AgentMemory{ID: id, Content: "content " + id, EmbeddingModel: "old-model"})
		s.models[id] = "old-model"
	}
	return s
}

func TestRunEmbedsAllPendingRows(t *testing.T) {
	store := newStore("a", "b", "c")
	emb := &fakeEmbedder{}
	stats, err := Run(context.Background(), store, emb, "new-model", 32)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if stats.Scanned != 3 || stats.Embedded != 3 || stats.Failed != 0 {
		t.Fatalf("stats = %+v", stats)
	}
	for _, id := range []string{"a", "b", "c"} {
		if store.models[id] != "new-model" || len(store.stored[id]) != 2 {
			t.Fatalf("row %s not embedded: model=%q vec=%v", id, store.models[id], store.stored[id])
		}
	}
}

func TestRunIsIdempotent(t *testing.T) {
	store := newStore("a", "b")
	emb := &fakeEmbedder{}
	if _, err := Run(context.Background(), store, emb, "new-model", 32); err != nil {
		t.Fatalf("first run: %v", err)
	}
	callsAfterFirst := emb.calls
	stats2, err := Run(context.Background(), store, emb, "new-model", 32)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if stats2.Scanned != 0 || stats2.Embedded != 0 {
		t.Fatalf("second run should be a no-op, got %+v", stats2)
	}
	if emb.calls != callsAfterFirst {
		t.Fatalf("second run embedded again (calls %d -> %d)", callsAfterFirst, emb.calls)
	}
}

func TestRunBatchesAndDrains(t *testing.T) {
	store := newStore("a", "b", "c", "d", "e")
	emb := &fakeEmbedder{}
	stats, err := Run(context.Background(), store, emb, "new-model", 2)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if stats.Scanned != 5 || stats.Embedded != 5 {
		t.Fatalf("stats = %+v", stats)
	}
}

func TestRunProgressGuardStopsOnTotalFailure(t *testing.T) {
	store := newStore("a", "b", "c", "d") // batch=2, all rows fail
	emb := &fakeEmbedder{err: errors.New("embedder down")}
	stats, err := Run(context.Background(), store, emb, "new-model", 2)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	// First batch of 2 fails, guard trips immediately — no infinite loop.
	if stats.Scanned != 2 || stats.Embedded != 0 || stats.Failed != 2 {
		t.Fatalf("stats = %+v", stats)
	}
}

func TestRunCountsStoreFailuresAndContinues(t *testing.T) {
	store := newStore("a", "b")
	store.setErr = errors.New("db write failed")
	emb := &fakeEmbedder{}
	stats, err := Run(context.Background(), store, emb, "new-model", 32)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if stats.Scanned != 2 || stats.Embedded != 0 || stats.Failed != 2 {
		t.Fatalf("stats = %+v", stats)
	}
}

func TestRunRespectsContextCancellation(t *testing.T) {
	store := newStore("a", "b", "c", "d", "e", "f")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Run(ctx, store, &fakeEmbedder{}, "new-model", 2)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context canceled", err)
	}
}
