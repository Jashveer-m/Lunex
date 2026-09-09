package db_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/jashveer/lifeos/backend/internal/chat"
	"github.com/jashveer/lifeos/backend/internal/memories"
)

// These cover the Phase 5 SQL: the memories table and its constraints, the
// owner-scoped similarity search with its three invisibility rules (disabled,
// expired, never embedded), the provenance link to conversations and the
// cascades. Nothing here talks to a model -- vectors are the same hand-built
// unit vectors the Phase 3 tests use, so the distances are arithmetic the test
// can predict.

func seedMemory(t *testing.T, repo *memories.Repository, owner uuid.UUID, in memories.CreateInput) memories.Memory {
	t.Helper()
	m, err := repo.Create(context.Background(), owner, in)
	if err != nil {
		t.Fatalf("create memory: %v", err)
	}
	return m
}

func fact(kind, content string, axis int) memories.CreateInput {
	return memories.CreateInput{
		Candidate: memories.Candidate{
			Type: kind, Content: content, Importance: 0.8, Confidence: 0.9,
		},
		Embedding: unit(axis),
	}
}

func TestMemoryRoundTrip(t *testing.T) {
	pool := testDB(t)
	repo := memories.NewRepository(pool)
	ctx := context.Background()
	alice := makeUser(t, pool, "alice@example.com")
	conv := seedConversation(t, chat.NewRepository(pool), alice, "How I study")

	in := fact(memories.TypePreference, "The user prefers studying in the morning.", 1)
	in.SourceConversationID = &conv.ID
	m := seedMemory(t, repo, alice, in)

	switch {
	case m.UserID != alice:
		t.Fatalf("owner = %v, want %v", m.UserID, alice)
	case m.Type != memories.TypePreference:
		t.Fatalf("type = %q", m.Type)
	case m.Content != in.Content:
		t.Fatalf("content = %q", m.Content)
	case !m.Enabled:
		t.Fatal("a new memory is disabled; the column default should be true")
	case m.ExpiresAt != nil:
		t.Fatalf("expires_at = %v, want NULL -- nothing sets it in this phase", m.ExpiresAt)
	case m.SourceConversationID == nil || *m.SourceConversationID != conv.ID:
		t.Fatalf("source = %v, want the conversation it came from", m.SourceConversationID)
	}
	// `real` is single precision, so the scores come back near rather than
	// exactly equal. The API rounds them; the column keeps what it was given.
	if diff := m.Importance - 0.8; diff > 1e-6 || diff < -1e-6 {
		t.Fatalf("importance = %v, want ~0.8", m.Importance)
	}

	read, err := repo.ByID(ctx, alice, m.ID)
	if err != nil {
		t.Fatalf("ByID: %v", err)
	}
	if read.ID != m.ID || read.Content != m.Content {
		t.Fatalf("read back %+v", read)
	}
}

// The embedding survives the driver round trip exactly: a memory searched
// against its own vector scores 1, not merely close to it.
func TestMemoryEmbeddingSurvivesTheDriverRoundTrip(t *testing.T) {
	pool := testDB(t)
	repo := memories.NewRepository(pool)
	alice := makeUser(t, pool, "alice@example.com")

	seedMemory(t, repo, alice, fact(memories.TypeSemantic, "The user knows Go.", 7))

	found, err := repo.Search(context.Background(), alice, unit(7), memories.SearchQuery{Limit: 5})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(found) != 1 {
		t.Fatalf("found %d memories, want 1", len(found))
	}
	if diff := found[0].Similarity - 1; diff > 1e-6 || diff < -1e-6 {
		t.Fatalf("a vector searched against itself scored %v, want 1", found[0].Similarity)
	}
}

// Cosine ordering is what the search reports, and the floor filters on the same
// metric -- the property the retrieval floor depends on being true.
func TestMemorySearchOrdersByCosineSimilarityAndAppliesTheFloor(t *testing.T) {
	pool := testDB(t)
	repo := memories.NewRepository(pool)
	ctx := context.Background()
	alice := makeUser(t, pool, "alice@example.com")

	seedMemory(t, repo, alice, fact(memories.TypeSemantic, "identical", 3))
	seedMemory(t, repo, alice, memories.CreateInput{
		Candidate: memories.Candidate{Type: memories.TypeSemantic, Content: "close", Importance: 0.5, Confidence: 0.5},
		Embedding: mix(3, 4, 0.8),
	})
	seedMemory(t, repo, alice, fact(memories.TypeSemantic, "orthogonal", 4))

	found, err := repo.Search(ctx, alice, unit(3), memories.SearchQuery{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 3 {
		t.Fatalf("found %d memories, want 3", len(found))
	}
	want := []struct {
		content string
		score   float64
	}{{"identical", 1}, {"close", 0.8}, {"orthogonal", 0}}
	for i, w := range want {
		if found[i].Content != w.content {
			t.Fatalf("result %d is %q, want %q -- the ordering is not by distance", i, found[i].Content, w.content)
		}
		if diff := found[i].Similarity - w.score; diff > 1e-6 || diff < -1e-6 {
			t.Fatalf("%q scored %v, want %v", w.content, found[i].Similarity, w.score)
		}
	}

	// The floor is the defence against an unrelated question retrieving the
	// nearest fact however far away it is.
	found, err = repo.Search(ctx, alice, unit(3), memories.SearchQuery{Limit: 10, MinSimilarity: 0.9})
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 || found[0].Content != "identical" {
		t.Fatalf("with a 0.9 floor the search returned %+v", found)
	}
}

// The three things retrieval cannot see, each pinned separately: they are the
// whole difference between "the user has this memory" and "the assistant will
// use it".
func TestMemorySearchSkipsDisabledExpiredAndUnembeddedRows(t *testing.T) {
	pool := testDB(t)
	repo := memories.NewRepository(pool)
	ctx := context.Background()
	alice := makeUser(t, pool, "alice@example.com")

	live := seedMemory(t, repo, alice, fact(memories.TypeSemantic, "live", 5))
	off := seedMemory(t, repo, alice, fact(memories.TypeSemantic, "disabled", 5))
	expired := seedMemory(t, repo, alice, fact(memories.TypeSemantic, "expired", 5))
	seedMemory(t, repo, alice, memories.CreateInput{
		Candidate: memories.Candidate{Type: memories.TypeSemantic, Content: "unembedded", Importance: 0.5, Confidence: 0.5},
	})

	disabled := false
	if _, err := repo.Update(ctx, alice, off.ID, memories.Patch{Enabled: &disabled}, nil); err != nil {
		t.Fatal(err)
	}
	// expires_at has no writer in this phase, so the test sets it directly --
	// which is the point: the retrieval filter is already correct for when one
	// arrives.
	if _, err := pool.Exec(`UPDATE memories SET expires_at = now() - interval '1 hour' WHERE id = $1`, expired.ID); err != nil {
		t.Fatal(err)
	}

	found, err := repo.Search(ctx, alice, unit(5), memories.SearchQuery{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 || found[0].ID != live.ID {
		t.Fatalf("search returned %+v, want only the live memory", found)
	}

	// None of them are gone: they are invisible to retrieval and present in the
	// list, which is what "disable without deleting" has to mean.
	listed, err := repo.List(ctx, alice, memories.Filter{Sort: memories.DefaultSort, Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 4 {
		t.Fatalf("the list holds %d memories, want all 4 -- live, disabled, expired and unembedded", len(listed))
	}
}

func TestMemorySearchNeverCrossesUsers(t *testing.T) {
	pool := testDB(t)
	repo := memories.NewRepository(pool)
	ctx := context.Background()
	alice := makeUser(t, pool, "alice@example.com")
	bob := makeUser(t, pool, "bob@example.com")

	seedMemory(t, repo, alice, fact(memories.TypeSemantic, "The user's launch code is quetzal seventeen.", 9))
	seedMemory(t, repo, bob, fact(memories.TypeSemantic, "The user keeps a list of birds.", 11))

	found, err := repo.Search(ctx, bob, unit(9), memories.SearchQuery{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range found {
		if strings.Contains(m.Content, "quetzal") {
			t.Fatalf("Bob's search returned Alice's memory: %+v", m)
		}
	}

	// Naming the row directly does not reach it either.
	alices := seedMemory(t, repo, alice, fact(memories.TypePreference, "Alice's preference", 9))
	if _, err := repo.ByID(ctx, bob, alices.ID); !errors.Is(err, memories.ErrNotFound) {
		t.Fatalf("ByID as the wrong user = %v, want ErrNotFound", err)
	}
	if err := repo.Delete(ctx, bob, alices.ID); !errors.Is(err, memories.ErrNotFound) {
		t.Fatalf("Delete as the wrong user = %v, want ErrNotFound", err)
	}
	enabled := false
	if _, err := repo.Update(ctx, bob, alices.ID, memories.Patch{Enabled: &enabled}, nil); !errors.Is(err, memories.ErrNotFound) {
		t.Fatalf("Update as the wrong user = %v, want ErrNotFound", err)
	}
	if still, err := repo.ByID(ctx, alice, alices.ID); err != nil || !still.Enabled {
		t.Fatalf("Alice's memory after Bob's attempts = %+v (%v)", still, err)
	}
}

func TestMemoryListFiltersByTypeAndEnabled(t *testing.T) {
	pool := testDB(t)
	repo := memories.NewRepository(pool)
	ctx := context.Background()
	alice := makeUser(t, pool, "alice@example.com")

	seedMemory(t, repo, alice, fact(memories.TypePreference, "mornings", 1))
	seedMemory(t, repo, alice, fact(memories.TypeSemantic, "knows Go", 2))
	off := seedMemory(t, repo, alice, fact(memories.TypeSemantic, "knows COBOL", 3))
	disabled := false
	if _, err := repo.Update(ctx, alice, off.ID, memories.Patch{Enabled: &disabled}, nil); err != nil {
		t.Fatal(err)
	}

	on := true
	for _, tc := range []struct {
		name string
		f    memories.Filter
		want int
	}{
		{"everything", memories.Filter{}, 3},
		{"by type", memories.Filter{Type: memories.TypeSemantic}, 2},
		{"enabled only", memories.Filter{Enabled: &on}, 2},
		{"disabled only", memories.Filter{Enabled: &disabled}, 1},
		{"both filters", memories.Filter{Type: memories.TypeSemantic, Enabled: &on}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := tc.f
			f.Sort, f.Limit = memories.DefaultSort, 50
			got, err := repo.List(ctx, alice, f)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != tc.want {
				t.Fatalf("listed %d memories, want %d: %+v", len(got), tc.want, got)
			}
		})
	}
}

// An edit replaces the vector, so the memory becomes retrievable by what it now
// says and stops being retrievable by what it used to.
func TestUpdatingContentReplacesTheStoredVector(t *testing.T) {
	pool := testDB(t)
	repo := memories.NewRepository(pool)
	ctx := context.Background()
	alice := makeUser(t, pool, "alice@example.com")

	m := seedMemory(t, repo, alice, fact(memories.TypeSemantic, "The user knows Go.", 13))

	corrected := "The user knows Rust."
	updated, err := repo.Update(ctx, alice, m.ID, memories.Patch{Content: &corrected}, unit(14))
	if err != nil {
		t.Fatal(err)
	}
	if updated.Content != corrected {
		t.Fatalf("content = %q", updated.Content)
	}
	if !updated.UpdatedAt.After(m.UpdatedAt) {
		t.Fatal("the set_updated_at trigger did not fire on the edit")
	}

	found, err := repo.Search(ctx, alice, unit(14), memories.SearchQuery{Limit: 5, MinSimilarity: 0.9})
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 {
		t.Fatalf("the edited memory is not retrievable by its new vector: %+v", found)
	}
	found, err = repo.Search(ctx, alice, unit(13), memories.SearchQuery{Limit: 5, MinSimilarity: 0.9})
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 0 {
		t.Fatalf("the old vector still finds the memory: %+v", found)
	}
}

// A toggle names no vector, so the old one stays: switching a memory off and on
// again must not lose its embedding.
func TestTogglingEnabledKeepsTheVector(t *testing.T) {
	pool := testDB(t)
	repo := memories.NewRepository(pool)
	ctx := context.Background()
	alice := makeUser(t, pool, "alice@example.com")

	m := seedMemory(t, repo, alice, fact(memories.TypeSemantic, "The user knows Go.", 15))
	off, on := false, true
	if _, err := repo.Update(ctx, alice, m.ID, memories.Patch{Enabled: &off}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Update(ctx, alice, m.ID, memories.Patch{Enabled: &on}, nil); err != nil {
		t.Fatal(err)
	}

	found, err := repo.Search(ctx, alice, unit(15), memories.SearchQuery{Limit: 5, MinSimilarity: 0.9})
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 {
		t.Fatalf("the memory lost its embedding across a toggle: %+v", found)
	}
}

func TestDeleteAllIsScopedToTheOwner(t *testing.T) {
	pool := testDB(t)
	repo := memories.NewRepository(pool)
	ctx := context.Background()
	alice := makeUser(t, pool, "alice@example.com")
	bob := makeUser(t, pool, "bob@example.com")

	seedMemory(t, repo, alice, fact(memories.TypeSemantic, "one", 1))
	seedMemory(t, repo, alice, fact(memories.TypeSemantic, "two", 2))
	seedMemory(t, repo, bob, fact(memories.TypeSemantic, "bob's", 3))

	n, err := repo.DeleteAll(ctx, alice)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("deleted %d memories, want 2", n)
	}
	left, err := repo.List(ctx, bob, memories.Filter{Sort: memories.DefaultSort, Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 1 {
		t.Fatalf("Bob has %d memories left, want his own untouched", len(left))
	}

	// Clearing an empty memory is not an error; it deletes nothing.
	if n, err = repo.DeleteAll(ctx, alice); err != nil || n != 0 {
		t.Fatalf("second clear = %d, %v", n, err)
	}
}

// The type and the two scores are constrained in the schema, so a bug in the
// Go code is a write error rather than a row nothing can filter or rank.
func TestMemoryColumnsAreConstrained(t *testing.T) {
	pool := testDB(t)
	alice := makeUser(t, pool, "alice@example.com")

	for _, tc := range []struct {
		name  string
		query string
		args  []any
	}{
		{
			"an unknown type",
			`INSERT INTO memories (user_id, type, content) VALUES ($1, 'vibes', 'x')`,
			[]any{alice},
		},
		{
			"an importance above 1",
			`INSERT INTO memories (user_id, type, content, importance) VALUES ($1, 'semantic', 'x', 1.5)`,
			[]any{alice},
		},
		{
			"a negative confidence",
			`INSERT INTO memories (user_id, type, content, confidence) VALUES ($1, 'semantic', 'x', -0.1)`,
			[]any{alice},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := pool.Exec(tc.query, tc.args...); err == nil {
				t.Fatal("the row was accepted; the CHECK constraint is missing")
			}
		})
	}

	// Every type the Go code can produce is storable, so the allow-list and the
	// constraint cannot drift apart.
	for _, kind := range memories.Types {
		if _, err := pool.Exec(
			`INSERT INTO memories (user_id, type, content) VALUES ($1, $2, 'x')`, alice, kind); err != nil {
			t.Fatalf("type %q was rejected by the schema: %v", kind, err)
		}
	}
}

// Deleting the conversation a memory came from must not delete the memory: the
// fact outlives its source and says so by carrying a NULL provenance.
func TestDeletingAConversationKeepsItsMemories(t *testing.T) {
	pool := testDB(t)
	repo := memories.NewRepository(pool)
	chatRepo := chat.NewRepository(pool)
	ctx := context.Background()
	alice := makeUser(t, pool, "alice@example.com")

	conv := seedConversation(t, chatRepo, alice, "How I study")
	in := fact(memories.TypePreference, "The user prefers studying in the morning.", 1)
	in.SourceConversationID = &conv.ID
	m := seedMemory(t, repo, alice, in)

	if err := chatRepo.DeleteConversation(ctx, alice, conv.ID); err != nil {
		t.Fatal(err)
	}

	survivor, err := repo.ByID(ctx, alice, m.ID)
	if err != nil {
		t.Fatalf("the memory went with its conversation: %v", err)
	}
	if survivor.SourceConversationID != nil {
		t.Fatalf("source = %v, want NULL once the conversation is gone", survivor.SourceConversationID)
	}
	if survivor.Content != m.Content {
		t.Fatalf("content = %q", survivor.Content)
	}
}

// Deleting a user takes their memories with them, which is the other half of
// "clear all": there is no orphaned record of somebody who no longer exists.
func TestDeletingAUserTakesTheirMemories(t *testing.T) {
	pool := testDB(t)
	repo := memories.NewRepository(pool)
	alice := makeUser(t, pool, "alice@example.com")

	seedMemory(t, repo, alice, fact(memories.TypeSemantic, "The user knows Go.", 1))
	if _, err := pool.Exec(`DELETE FROM users`); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := pool.QueryRow(`SELECT count(*) FROM memories`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("%d memories survived the deleted user", n)
	}
}

// The list's default order is newest first, so "what has it learned about me
// lately" is the first thing a user sees.
func TestMemoryListIsNewestFirst(t *testing.T) {
	pool := testDB(t)
	repo := memories.NewRepository(pool)
	ctx := context.Background()
	alice := makeUser(t, pool, "alice@example.com")

	first := seedMemory(t, repo, alice, fact(memories.TypeSemantic, "first", 1))
	// created_at defaults to now() and two inserts can share a transaction
	// timestamp only if they are in one; they are not, but nudging the first
	// row back makes the assertion independent of clock resolution.
	if _, err := pool.Exec(
		`UPDATE memories SET created_at = created_at - interval '1 minute' WHERE id = $1`, first.ID); err != nil {
		t.Fatal(err)
	}
	second := seedMemory(t, repo, alice, fact(memories.TypeSemantic, "second", 2))

	got, err := repo.List(ctx, alice, memories.Filter{Sort: memories.DefaultSort, Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != second.ID {
		t.Fatalf("list = %+v, want the newest memory first", got)
	}
}
