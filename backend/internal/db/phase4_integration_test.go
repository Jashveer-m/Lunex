package db_test

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/jashveer/lifeos/backend/internal/chat"
)

// These cover the Phase 4 SQL: the jsonb sources column, the ordering that
// keeps an answer behind its question, the owner scoping that runs through the
// conversation rather than through a column on messages, and the cascades.
// Nothing here talks to a model.

func seedConversation(t *testing.T, repo *chat.Repository, owner uuid.UUID, title string) chat.Conversation {
	t.Helper()
	c, err := repo.CreateConversation(context.Background(), owner, title)
	if err != nil {
		t.Fatalf("create conversation: %v", err)
	}
	return c
}

func TestConversationRoundTrip(t *testing.T) {
	pool := testDB(t)
	repo := chat.NewRepository(pool)
	ctx := context.Background()
	alice := makeUser(t, pool, "alice@example.com")

	conv := seedConversation(t, repo, alice, chat.DefaultTitle)
	if conv.Title != chat.DefaultTitle || conv.MessageCount != 0 {
		t.Fatalf("new conversation = %+v", conv)
	}

	docID := uuid.New()
	idx, sim := 2, 0.71
	sources := []chat.Source{{
		Type: chat.SourceDocument, ID: docID, Label: "S1", Title: "field-notes.md",
		ChunkIndex: &idx, Similarity: &sim, Excerpt: "The aurora borealis…", Cited: true,
	}}

	written, err := repo.AppendTurn(ctx, alice, conv.ID, []chat.NewMessage{
		{Role: chat.RoleUser, Content: "what happened over the tundra?"},
		{Role: chat.RoleAssistant, Content: "The aurora appeared [S1].", Sources: sources},
	}, "what happened over the tundra?")
	if err != nil {
		t.Fatalf("AppendTurn: %v", err)
	}
	if len(written) != 2 {
		t.Fatalf("wrote %d messages, want 2", len(written))
	}

	msgs, err := repo.Messages(ctx, alice, conv.ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Fatalf("read %d messages, want 2", len(msgs))
	}
	// The question must come back before the answer. Both rows are written in
	// one transaction, so this is exactly the ordering clock_timestamp() buys.
	if msgs[0].Role != chat.RoleUser || msgs[1].Role != chat.RoleAssistant {
		t.Fatalf("roles = %q, %q -- the answer precedes the question", msgs[0].Role, msgs[1].Role)
	}
	if !msgs[1].CreatedAt.After(msgs[0].CreatedAt) {
		t.Fatalf("timestamps are not strictly increasing: %v then %v", msgs[0].CreatedAt, msgs[1].CreatedAt)
	}

	// jsonb survives the round trip whole, pointers and all.
	if msgs[0].Sources != nil {
		t.Fatalf("a user message came back with sources: %v", msgs[0].Sources)
	}
	got := msgs[1].Sources
	if len(got) != 1 {
		t.Fatalf("read %d sources, want 1", len(got))
	}
	switch {
	case got[0].ID != docID:
		t.Fatalf("document id = %v, want %v", got[0].ID, docID)
	case got[0].Label != "S1" || !got[0].Cited:
		t.Fatalf("source = %+v, want the cited S1", got[0])
	case got[0].ChunkIndex == nil || *got[0].ChunkIndex != 2:
		t.Fatalf("chunk index = %v, want 2", got[0].ChunkIndex)
	case got[0].Similarity == nil || *got[0].Similarity != 0.71:
		t.Fatalf("similarity = %v, want 0.71", got[0].Similarity)
	}

	// The turn named the conversation and moved it up the list.
	after, err := repo.ConversationByID(ctx, alice, conv.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Title != "what happened over the tundra?" {
		t.Fatalf("title = %q, want the derived one", after.Title)
	}
	if after.MessageCount != 2 {
		t.Fatalf("message_count = %d, want 2", after.MessageCount)
	}
	if !after.UpdatedAt.After(conv.UpdatedAt) {
		t.Fatalf("updated_at did not move: %v then %v", conv.UpdatedAt, after.UpdatedAt)
	}
}

// A title the user chose is not overwritten by the first message.
func TestAppendTurnLeavesANamedConversationAlone(t *testing.T) {
	pool := testDB(t)
	repo := chat.NewRepository(pool)
	ctx := context.Background()
	alice := makeUser(t, pool, "alice@example.com")

	conv := seedConversation(t, repo, alice, "Thesis planning")
	if _, err := repo.AppendTurn(ctx, alice, conv.ID,
		[]chat.NewMessage{{Role: chat.RoleUser, Content: "where do I start?"}}, "where do I start?"); err != nil {
		t.Fatal(err)
	}
	after, _ := repo.ConversationByID(ctx, alice, conv.ID)
	if after.Title != "Thesis planning" {
		t.Fatalf("title = %q, want the one the user chose", after.Title)
	}
}

// messages carries no user_id: ownership runs through the conversation. This
// is the test that the join actually scopes, rather than the column that is
// not there being missed.
func TestMessagesAreScopedThroughTheirConversation(t *testing.T) {
	pool := testDB(t)
	repo := chat.NewRepository(pool)
	ctx := context.Background()
	alice := makeUser(t, pool, "alice@example.com")
	bob := makeUser(t, pool, "bob@example.com")

	conv := seedConversation(t, repo, alice, chat.DefaultTitle)
	if _, err := repo.AppendTurn(ctx, alice, conv.ID, []chat.NewMessage{
		{Role: chat.RoleUser, Content: "the launch code is quetzal seventeen"},
		{Role: chat.RoleAssistant, Content: "noted"},
	}, "the launch code"); err != nil {
		t.Fatal(err)
	}

	// Bob knows the id and asks for it by name. Every path answers as though
	// it does not exist.
	if _, err := repo.ConversationByID(ctx, bob, conv.ID); !errors.Is(err, chat.ErrNotFound) {
		t.Fatalf("ConversationByID as the wrong user = %v, want ErrNotFound", err)
	}
	msgs, err := repo.Messages(ctx, bob, conv.ID, 100)
	if err != nil {
		t.Fatalf("Messages as the wrong user = %v, want no rows and no error", err)
	}
	if len(msgs) != 0 {
		t.Fatalf("Bob read %d of Alice's messages", len(msgs))
	}
	if _, err := repo.AppendTurn(ctx, bob, conv.ID,
		[]chat.NewMessage{{Role: chat.RoleUser, Content: "hijacked"}}, ""); !errors.Is(err, chat.ErrNotFound) {
		t.Fatalf("AppendTurn as the wrong user = %v, want ErrNotFound", err)
	}
	if err := repo.DeleteConversation(ctx, bob, conv.ID); !errors.Is(err, chat.ErrNotFound) {
		t.Fatalf("DeleteConversation as the wrong user = %v, want ErrNotFound", err)
	}
	list, err := repo.ListConversations(ctx, bob, chat.Filter{Sort: chat.DefaultSort, Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("Bob listed %d of Alice's conversations", len(list))
	}

	// And nothing Bob did landed: the write must have been refused, not just
	// reported as refused.
	still, err := repo.Messages(ctx, alice, conv.ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(still) != 2 {
		t.Fatalf("Alice's conversation holds %d messages after Bob's attempts, want 2", len(still))
	}
	for _, m := range still {
		if strings.Contains(m.Content, "hijacked") {
			t.Fatalf("Bob's message is in Alice's conversation: %v", m)
		}
	}
}

// A long conversation returns its most recent turns, in order — which is what
// the history window and the read endpoint both want.
func TestMessagesReturnsTheMostRecentWindowInOrder(t *testing.T) {
	pool := testDB(t)
	repo := chat.NewRepository(pool)
	ctx := context.Background()
	alice := makeUser(t, pool, "alice@example.com")
	conv := seedConversation(t, repo, alice, chat.DefaultTitle)

	for i := 0; i < 10; i++ {
		if _, err := repo.AppendTurn(ctx, alice, conv.ID, []chat.NewMessage{
			{Role: chat.RoleUser, Content: "q" + string(rune('0'+i))},
			{Role: chat.RoleAssistant, Content: "a" + string(rune('0'+i))},
		}, ""); err != nil {
			t.Fatal(err)
		}
	}

	msgs, err := repo.Messages(ctx, alice, conv.ID, 4)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 4 {
		t.Fatalf("read %d messages, want 4", len(msgs))
	}
	want := []string{"q8", "a8", "q9", "a9"}
	for i, m := range msgs {
		if m.Content != want[i] {
			t.Fatalf("message %d = %q, want %q -- the window must be the end, in order", i, m.Content, want[i])
		}
	}
}

// The role CHECK is the database's own guard against a typo in the Go code
// becoming a message no client knows how to render.
func TestMessageRoleIsConstrained(t *testing.T) {
	pool := testDB(t)
	repo := chat.NewRepository(pool)
	alice := makeUser(t, pool, "alice@example.com")
	conv := seedConversation(t, repo, alice, chat.DefaultTitle)

	_, err := pool.Exec(
		`INSERT INTO messages (conversation_id, role, content) VALUES ($1, 'narrator', 'x')`, conv.ID)
	if err == nil {
		t.Fatal("the messages table accepted a role outside the allow-list")
	}
	if !strings.Contains(err.Error(), "messages_role_check") {
		t.Fatalf("err = %v, want the role CHECK constraint", err)
	}

	for _, role := range []string{chat.RoleUser, chat.RoleAssistant, chat.RoleSystem} {
		if _, err := pool.Exec(
			`INSERT INTO messages (conversation_id, role, content) VALUES ($1, $2, 'x')`, conv.ID, role); err != nil {
			t.Fatalf("role %q was rejected: %v", role, err)
		}
	}
}

// A turn is atomic: if the second insert fails, the first is not left behind.
func TestAppendTurnIsAtomic(t *testing.T) {
	pool := testDB(t)
	repo := chat.NewRepository(pool)
	ctx := context.Background()
	alice := makeUser(t, pool, "alice@example.com")
	conv := seedConversation(t, repo, alice, chat.DefaultTitle)

	_, err := repo.AppendTurn(ctx, alice, conv.ID, []chat.NewMessage{
		{Role: chat.RoleUser, Content: "a question"},
		{Role: "narrator", Content: "an invalid role"},
	}, "a question")
	if err == nil {
		t.Fatal("want the invalid role to fail the turn")
	}

	msgs, err := repo.Messages(ctx, alice, conv.ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 0 {
		t.Fatalf("%d messages survived a rolled-back turn, want 0", len(msgs))
	}
	after, _ := repo.ConversationByID(ctx, alice, conv.ID)
	if after.Title != chat.DefaultTitle {
		t.Fatalf("title = %q, want the rollback to have undone the rename", after.Title)
	}
}

// Deleting a conversation takes its messages; deleting a user takes both.
func TestChatCascades(t *testing.T) {
	pool := testDB(t)
	repo := chat.NewRepository(pool)
	ctx := context.Background()
	alice := makeUser(t, pool, "alice@example.com")

	first := seedConversation(t, repo, alice, chat.DefaultTitle)
	second := seedConversation(t, repo, alice, chat.DefaultTitle)
	for _, c := range []uuid.UUID{first.ID, second.ID} {
		if _, err := repo.AppendTurn(ctx, alice, c, []chat.NewMessage{
			{Role: chat.RoleUser, Content: "q"}, {Role: chat.RoleAssistant, Content: "a"},
		}, "q"); err != nil {
			t.Fatal(err)
		}
	}
	if got := countRows(t, pool, "messages"); got != 4 {
		t.Fatalf("%d messages stored, want 4", got)
	}

	if err := repo.DeleteConversation(ctx, alice, first.ID); err != nil {
		t.Fatal(err)
	}
	if got := countRows(t, pool, "messages"); got != 2 {
		t.Fatalf("%d messages after deleting one conversation, want 2", got)
	}

	if _, err := pool.Exec(`DELETE FROM users`); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"conversations", "messages"} {
		if got := countRows(t, pool, table); got != 0 {
			t.Fatalf("%d rows survived in %s after the user was deleted", got, table)
		}
	}
}

func countRows(t *testing.T, pool *sql.DB, table string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(`SELECT count(*) FROM ` + table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// The list sorts by real activity, which is only true because AppendTurn
// touches the parent row.
func TestConversationListIsMostRecentlyActiveFirst(t *testing.T) {
	pool := testDB(t)
	repo := chat.NewRepository(pool)
	ctx := context.Background()
	alice := makeUser(t, pool, "alice@example.com")

	older := seedConversation(t, repo, alice, "older")
	newer := seedConversation(t, repo, alice, "newer")
	// A turn on the older conversation makes it the most recently active.
	if _, err := repo.AppendTurn(ctx, alice, older.ID,
		[]chat.NewMessage{{Role: chat.RoleUser, Content: "q"}}, ""); err != nil {
		t.Fatal(err)
	}

	list, err := repo.ListConversations(ctx, alice, chat.Filter{Sort: chat.DefaultSort, Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("listed %d conversations, want 2", len(list))
	}
	if list[0].ID != older.ID {
		t.Fatalf("first = %q, want the one that was just used", list[0].Title)
	}
	if list[1].ID != newer.ID {
		t.Fatalf("second = %q", list[1].Title)
	}
}
