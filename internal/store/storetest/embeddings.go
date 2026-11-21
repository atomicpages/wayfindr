package storetest

import (
	"context"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/atomicpages/wayfindr/internal/store"
)

func pendingNames(t *testing.T, s store.Store, userID, modelID string, limit int) []string {
	t.Helper()
	tools, err := s.PendingEmbeddings(context.Background(), userID, modelID, limit)

	if err != nil {
		t.Fatalf("PendingEmbeddings: %v", err)
	}

	var names []string

	for _, tl := range tools {
		names = append(names, tl.Connector+"."+tl.Name)
	}

	return names
}

func testNewToolsArePendingInConnectorAndNameOrder(t *testing.T, newDatabase NewDatabase) {
	s, user := newStore(t, newDatabase)
	apply(t, s, user, "linear", tool(t, "linear", "b", "B.", 1), tool(t, "linear", "a", "A.", 2))
	apply(t, s, user, "github", tool(t, "github", "z", "Z.", 3))

	if diff := cmp.Diff([]string{"github.z", "linear.a", "linear.b"}, pendingNames(t, s, user, "m1", 10)); diff != "" {
		t.Errorf("pending (-want +got):\n%s", diff)
	}

	if diff := cmp.Diff([]string{"github.z", "linear.a"}, pendingNames(t, s, user, "m1", 2)); diff != "" {
		t.Errorf("pending with limit 2 (-want +got):\n%s", diff)
	}
}

func testSetEmbeddingsStoresVectorAndClearsPending(t *testing.T, newDatabase NewDatabase) {
	ctx := context.Background()
	s, user := newStore(t, newDatabase)
	apply(t, s, user, "linear", tool(t, "linear", "a", "A.", 1), tool(t, "linear", "b", "B.", 2))

	err := s.SetEmbeddings(ctx, user, "m1", []store.ToolVector{
		{Connector: "linear", Name: "a", ContentHash: []byte{1}, Vector: []float32{0.25, -1, 3.5}},
	})

	if err != nil {
		t.Fatalf("SetEmbeddings: %v", err)
	}

	got, err := s.GetTool(ctx, user, "linear", "a")

	if err != nil {
		t.Fatalf("GetTool: %v", err)
	}

	if diff := cmp.Diff([]float32{0.25, -1, 3.5}, got.Embedding); diff != "" {
		t.Errorf("Embedding (-want +got):\n%s", diff)
	}

	if got.EmbeddingModel != "m1" {
		t.Errorf("EmbeddingModel = %q, want m1", got.EmbeddingModel)
	}

	if diff := cmp.Diff([]string{"linear.b"}, pendingNames(t, s, user, "m1", 10)); diff != "" {
		t.Errorf("pending for m1 (-want +got):\n%s", diff)
	}

	// A different model identity makes every row pending again.
	if diff := cmp.Diff([]string{"linear.a", "linear.b"}, pendingNames(t, s, user, "m2", 10)); diff != "" {
		t.Errorf("pending for m2 (-want +got):\n%s", diff)
	}
}

func testChangedHashClearsEmbedding(t *testing.T, newDatabase NewDatabase) {
	ctx := context.Background()
	s, user := newStore(t, newDatabase)
	apply(t, s, user, "linear", tool(t, "linear", "a", "A.", 1), tool(t, "linear", "b", "B.", 2))

	if err := s.SetEmbeddings(ctx, user, "m1", []store.ToolVector{
		{Connector: "linear", Name: "a", ContentHash: []byte{1}, Vector: []float32{1}},
		{Connector: "linear", Name: "b", ContentHash: []byte{2}, Vector: []float32{2}},
	}); err != nil {
		t.Fatalf("SetEmbeddings: %v", err)
	}

	apply(t, s, user, "linear", tool(t, "linear", "a", "A changed.", 7), tool(t, "linear", "b", "B.", 2))

	if diff := cmp.Diff([]string{"linear.a"}, pendingNames(t, s, user, "m1", 10)); diff != "" {
		t.Errorf("pending (-want +got):\n%s", diff)
	}

	a, err := s.GetTool(ctx, user, "linear", "a")

	if err != nil {
		t.Fatalf("GetTool(a): %v", err)
	}

	if a.Embedding != nil || a.EmbeddingModel != "" {
		t.Errorf("changed tool kept embedding %v from %q, want none", a.Embedding, a.EmbeddingModel)
	}

	b, err := s.GetTool(ctx, user, "linear", "b")

	if err != nil {
		t.Fatalf("GetTool(b): %v", err)
	}

	if diff := cmp.Diff([]float32{2}, b.Embedding); diff != "" {
		t.Errorf("unchanged tool embedding (-want +got):\n%s", diff)
	}
}

func testSetEmbeddingsSkipsStaleVectors(t *testing.T, newDatabase NewDatabase) {
	ctx := context.Background()
	s, user := newStore(t, newDatabase)
	apply(t, s, user, "linear", tool(t, "linear", "a", "A.", 1))
	// The tool changes after the embedder read it with hash 1.
	apply(t, s, user, "linear", tool(t, "linear", "a", "A changed.", 2))

	err := s.SetEmbeddings(ctx, user, "m1", []store.ToolVector{
		{Connector: "linear", Name: "a", ContentHash: []byte{1}, Vector: []float32{1}},
		{Connector: "linear", Name: "gone", ContentHash: []byte{9}, Vector: []float32{1}},
	})

	if err != nil {
		t.Fatalf("SetEmbeddings: %v", err)
	}

	if diff := cmp.Diff([]string{"linear.a"}, pendingNames(t, s, user, "m1", 10)); diff != "" {
		t.Errorf("pending (-want +got):\n%s", diff)
	}
}

func testPendingEmbeddingsRejectsNonPositiveLimit(t *testing.T, newDatabase NewDatabase) {
	s, user := newStore(t, newDatabase)

	if _, err := s.PendingEmbeddings(context.Background(), user, "m1", 0); err == nil {
		t.Error("PendingEmbeddings(limit 0): err = nil, want error")
	}
}
