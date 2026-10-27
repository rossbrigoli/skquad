package backfill

import (
	"context"
	"errors"
	"strings"
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

// contentEmbedder fails permanently (context-window) on rows whose
// content contains "oversize", embeds the rest.
type contentEmbedder struct{ calls int }

func (f *contentEmbedder) Embed(_ context.Context, text string) ([]float64, error) {
	f.calls++
	if strings.Contains(text, "oversize") {
		return nil, errors.New("gateway returned 400: litellm.ContextWindowExceededError: request (41419 tokens) exceeds the available context size (32768 tokens)")
	}
	return []float64{float64(f.calls), 0.5}, nil
}

func TestRunSkipsOversizedRowsWithoutFailing(t *testing.T) {
	store := newStore("ok1", "big", "ok2")
	store.pending[1].Content = "oversize transcript that will never fit"
	emb := &contentEmbedder{}
	stats, err := Run(context.Background(), store, emb, "new-model", 32)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if stats.Scanned != 3 || stats.Embedded != 2 || stats.Failed != 0 || stats.Skipped != 1 {
		t.Fatalf("stats = %+v", stats)
	}
	// The oversized row is NOT written; the good rows are.
	if _, stored := store.stored["big"]; stored {
		t.Fatalf("oversized row should not be stored")
	}
	if store.models["ok1"] != "new-model" || store.models["ok2"] != "new-model" {
		t.Fatalf("good rows not embedded: %+v", store.models)
	}
}

func TestRunAllOversizedStopsAndIsNotFatal(t *testing.T) {
	store := newStore("b1", "b2", "b3", "b4") // batch=2
	for i := range store.pending {
		store.pending[i].Content = "oversize " + store.pending[i].ID
	}
	emb := &contentEmbedder{}
	stats, err := Run(context.Background(), store, emb, "new-model", 2)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	// First batch of 2 both skipped → no embed progress → guard stops.
	if stats.Embedded != 0 || stats.Failed != 0 || stats.Skipped != 2 {
		t.Fatalf("stats = %+v", stats)
	}
}

func TestIsPermanentEmbedError(t *testing.T) {
	permanent := []string{
		"ContextWindowExceededError",
		"exceed_context_size_error: too big",
		"request exceeds the available context size",
		"maximum context length is 32768",
		"input is too long",
		"context_length_exceeded",
	}
	for _, s := range permanent {
		if !isPermanentEmbedError(errors.New(s)) {
			t.Fatalf("expected permanent: %q", s)
		}
	}
	transient := []string{
		"connection refused",
		"502 bad gateway",
		"embedder unreachable",
	}
	for _, s := range transient {
		if isPermanentEmbedError(errors.New(s)) {
			t.Fatalf("expected transient: %q", s)
		}
	}
	if isPermanentEmbedError(nil) {
		t.Fatalf("nil must not be permanent")
	}
}
