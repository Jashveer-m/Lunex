package db_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/jashveer/lifeos/backend/internal/documents"
	"github.com/jashveer/lifeos/backend/internal/graph"
	"github.com/jashveer/lifeos/backend/internal/optional"
	"github.com/jashveer/lifeos/backend/internal/study"
)

// These cover the Phase 10a SQL: the card count that is computed rather than
// stored, the two cascades that differ (a plan takes its cards, a document
// does not), the ordered read of a document's passages that grounds a
// generation, and the node a study plan gets -- a `project`, which is the one
// mirrored type that is not its own table's name. Cross-user isolation over
// the whole stack lives in internal/api/study_isolation_test.go.

// studyPassages is the seeded document's text. It is the same two passages the
// study package's own tests use, so "grounded" means the same thing here.
var studyPassages = []documents.Chunk{
	{Index: 0, Content: "The aurora borealis appeared over the tundra shortly after midnight " +
		"and lasted about forty minutes.", Embedding: unit(0)},
	{Index: 1, Content: "The generator needs a new fuel filter before the next resupply run.",
		Embedding: unit(1)},
}

func TestStudyPlanRepositoryRoundTrip(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	repo := study.NewRepository(pool)
	docs := documents.NewRepository(pool)
	owner := makeUser(t, pool, "ada@example.com")
	doc := makeDocument(t, docs, owner, "field-notes.txt", studyPassages)

	created, err := repo.CreatePlan(ctx, owner, study.CreatePlanInput{
		Title: "Field notes revision", Description: "the whole file",
		DocumentID: &doc.ID, Status: study.StatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	switch {
	case created.Title != "Field notes revision":
		t.Fatalf("title = %q", created.Title)
	case created.Description == nil || *created.Description != "the whole file":
		t.Fatalf("description = %v", created.Description)
	case created.DocumentID == nil || *created.DocumentID != doc.ID:
		t.Fatalf("document_id = %v", created.DocumentID)
	// The filename comes back on the join, so a client renders a row without
	// a second request.
	case created.DocumentName == nil || *created.DocumentName != "field-notes.txt":
		t.Fatalf("document = %v", created.DocumentName)
	case created.CardCount != 0:
		t.Fatalf("a new plan reports %d cards", created.CardCount)
	}

	// The card count is a subquery, not a column, so it cannot disagree with
	// the rows.
	if _, err := repo.CreateFlashcards(ctx, owner, []study.CreateCardInput{
		{StudyPlanID: &created.ID, DocumentID: &doc.ID,
			Front: "How long did the aurora last?", Back: "About forty minutes."},
		{StudyPlanID: &created.ID, DocumentID: &doc.ID,
			Front: "What does the generator need?", Back: "A new fuel filter."},
	}); err != nil {
		t.Fatal(err)
	}
	reloaded, err := repo.PlanByID(ctx, owner, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.CardCount != 2 {
		t.Fatalf("card_count = %d, want 2", reloaded.CardCount)
	}

	// A partial update touches only what it names, and updated_at moves.
	updated, err := repo.UpdatePlan(ctx, owner, created.ID, study.Patch{
		Status: ptr(study.StatusCompleted),
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status != study.StatusCompleted || updated.Title != created.Title {
		t.Fatalf("update = %+v", updated)
	}
	if !updated.UpdatedAt.After(created.UpdatedAt) {
		t.Fatalf("updated_at did not move: %s then %s", created.UpdatedAt, updated.UpdatedAt)
	}

	// And an explicit null clears the nullable columns.
	cleared, err := repo.UpdatePlan(ctx, owner, created.ID, study.Patch{
		Description: optional.Null[string](),
		DocumentID:  optional.Null[uuid.UUID](),
	})
	if err != nil {
		t.Fatal(err)
	}
	if cleared.Description != nil || cleared.DocumentID != nil || cleared.DocumentName != nil {
		t.Fatalf("cleared = %+v", cleared)
	}
}

func TestStudyPlanFiltersAndSorts(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	repo := study.NewRepository(pool)
	docs := documents.NewRepository(pool)
	owner := makeUser(t, pool, "ada@example.com")
	doc := makeDocument(t, docs, owner, "field-notes.txt", studyPassages)

	for _, p := range []study.CreatePlanInput{
		{Title: "Linear algebra", Status: study.StatusActive},
		{Title: "Operating systems", Description: "scheduling and paging", Status: study.StatusCompleted},
		{Title: "Field notes", DocumentID: &doc.ID, Status: study.StatusActive},
	} {
		if _, err := repo.CreatePlan(ctx, owner, p); err != nil {
			t.Fatal(err)
		}
	}

	for name, tc := range map[string]struct {
		filter study.Filter
		want   int
	}{
		"everything":         {study.Filter{Limit: 50}, 3},
		"by status":          {study.Filter{Status: study.StatusActive, Limit: 50}, 2},
		"by document":        {study.Filter{DocumentID: &doc.ID, Limit: 50}, 1},
		"by title text":      {study.Filter{Query: "algebra", Limit: 50}, 1},
		"by description":     {study.Filter{Query: "paging", Limit: 50}, 1},
		"a literal wildcard": {study.Filter{Query: "%", Limit: 50}, 0},
		"no match":           {study.Filter{Query: "thermodynamics", Limit: 50}, 0},
	} {
		t.Run(name, func(t *testing.T) {
			f := tc.filter
			f.Sort = study.DefaultSort
			found, err := repo.Plans(ctx, owner, f)
			if err != nil {
				t.Fatal(err)
			}
			if len(found) != tc.want {
				t.Fatalf("found %d plans, want %d: %+v", len(found), tc.want, found)
			}
		})
	}

	// Every sort the allow-list offers is real SQL. A fragment the alias was
	// not applied to is a syntax error rather than a wrong answer, which is
	// why every one of them is run.
	for sort := range study.Sorts {
		if _, err := repo.Plans(ctx, owner, study.Filter{Sort: sort, Limit: 50}); err != nil {
			t.Fatalf("sort %q: %v", sort, err)
		}
	}
}

// A deck reads back in the order the model wrote it, which for a generated
// deck is the order of the document it came from.
//
// It is worth a test of its own because the obvious implementation gets it
// wrong and does so invisibly. A batch is one transaction, `now()` is the
// transaction's start time, so every card would carry the identical created_at
// and the tie-break on a random uuid would shuffle the deck -- eight correct
// cards in a meaningless order, which nothing about the API would tell you
// about. CreateFlashcards stamps clock_timestamp() per row instead.
func TestADeckReadsBackInTheOrderItWasWritten(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	repo := study.NewRepository(pool)
	owner := makeUser(t, pool, "ada@example.com")
	plan, err := repo.CreatePlan(ctx, owner, study.CreatePlanInput{Title: "Deck", Status: study.StatusActive})
	if err != nil {
		t.Fatal(err)
	}

	// Enough cards that a random ordering is overwhelmingly unlikely to come
	// back sorted: 12! is about 479 million.
	want := make([]string, 0, 12)
	batch := make([]study.CreateCardInput, 0, 12)
	for i := 0; i < 12; i++ {
		front := fmt.Sprintf("card %02d", i)
		want = append(want, front)
		batch = append(batch, study.CreateCardInput{StudyPlanID: &plan.ID, Front: front, Back: "a."})
	}
	created, err := repo.CreateFlashcards(ctx, owner, batch)
	if err != nil {
		t.Fatal(err)
	}
	for i, c := range created {
		if c.Front != want[i] {
			t.Fatalf("the insert returned %q at %d, want %q", c.Front, i, want[i])
		}
	}

	read, err := repo.Flashcards(ctx, owner, study.CardFilter{StudyPlanID: &plan.ID, Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	if len(read) != len(want) {
		t.Fatalf("read %d cards, want %d", len(read), len(want))
	}
	for i, c := range read {
		if c.Front != want[i] {
			t.Fatalf("card %d is %q, want %q -- the deck came back out of order", i, c.Front, want[i])
		}
	}
}

// The two links behave differently on delete, on purpose: a plan takes its
// cards with it, and a document takes neither.
func TestStudyDeletionCascades(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	repo := study.NewRepository(pool)
	docs := documents.NewRepository(pool)
	owner := makeUser(t, pool, "ada@example.com")
	doc := makeDocument(t, docs, owner, "field-notes.txt", studyPassages)

	plan, err := repo.CreatePlan(ctx, owner, study.CreatePlanInput{
		Title: "Field notes revision", DocumentID: &doc.ID, Status: study.StatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	cards, err := repo.CreateFlashcards(ctx, owner, []study.CreateCardInput{
		{StudyPlanID: &plan.ID, DocumentID: &doc.ID, Front: "q1?", Back: "a1."},
		{DocumentID: &doc.ID, Front: "q2?", Back: "a2."}, // filed under no plan
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(cards) != 2 {
		t.Fatalf("created %d cards", len(cards))
	}

	// Deleting the document un-links the plan and the cards and deletes
	// neither: the studying happened, the source file was a convenience.
	if _, err := pool.Exec(`DELETE FROM documents WHERE id = $1`, doc.ID); err != nil {
		t.Fatal(err)
	}
	orphaned, err := repo.PlanByID(ctx, owner, plan.ID)
	if err != nil {
		t.Fatalf("the plan went with its document: %v", err)
	}
	if orphaned.DocumentID != nil || orphaned.DocumentName != nil {
		t.Fatalf("document = %v/%v, want null once it is gone", orphaned.DocumentID, orphaned.DocumentName)
	}
	remaining, err := repo.Flashcards(ctx, owner, study.CardFilter{Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 2 {
		t.Fatalf("%d cards survived the deleted document, want 2", len(remaining))
	}
	for _, c := range remaining {
		if c.DocumentID != nil {
			t.Fatalf("card %v still points at the deleted document", c.ID)
		}
	}

	// Deleting the plan takes the card filed under it, and leaves the one that
	// was not.
	if err := repo.DeletePlan(ctx, owner, plan.ID); err != nil {
		t.Fatal(err)
	}
	left, err := repo.Flashcards(ctx, owner, study.CardFilter{Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 1 || left[0].Front != "q2?" {
		t.Fatalf("after deleting the plan: %+v", left)
	}

	// And the user takes everything.
	if _, err := pool.Exec(`DELETE FROM users`); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"study_plans", "flashcards"} {
		var n int
		if err := pool.QueryRow(`SELECT count(*) FROM ` + table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("%d rows of %s survived the deleted user", n, table)
		}
	}
}

func TestStudyRepositoryRefusesForeignRows(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	repo := study.NewRepository(pool)
	docs := documents.NewRepository(pool)
	ada := makeUser(t, pool, "ada@example.com")
	bob := makeUser(t, pool, "bob@example.com")
	adasDoc := makeDocument(t, docs, ada, "field-notes.txt", studyPassages)

	plan, err := repo.CreatePlan(ctx, ada, study.CreatePlanInput{Title: "Ada's plan", Status: study.StatusActive})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.PlanByID(ctx, bob, plan.ID); !errors.Is(err, study.ErrNotFound) {
		t.Fatalf("Bob read Ada's plan: %v", err)
	}
	if err := repo.DeletePlan(ctx, bob, plan.ID); !errors.Is(err, study.ErrNotFound) {
		t.Fatalf("Bob deleted Ada's plan: %v", err)
	}
	// The ownership probes are owner-scoped, which is what keeps a foreign id
	// from being confirmable through a link.
	for name, probe := range map[string]func() (bool, error){
		"plan":     func() (bool, error) { return repo.PlanExists(ctx, bob, plan.ID) },
		"document": func() (bool, error) { return repo.DocumentExists(ctx, bob, adasDoc.ID) },
	} {
		t.Run(name, func(t *testing.T) {
			ok, err := probe()
			if err != nil {
				t.Fatal(err)
			}
			if ok {
				t.Fatalf("Ada's %s exists for Bob", name)
			}
		})
	}
}

// Passages is the read a generation is grounded in: a document's chunks, in
// document order, owner-scoped. It is the one read in the documents module
// that has no query to rank against.
func TestPassagesReadADocumentInOrder(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	repo := documents.NewRepository(pool)
	ada := makeUser(t, pool, "ada@example.com")
	bob := makeUser(t, pool, "bob@example.com")
	doc := makeDocument(t, repo, ada, "field-notes.txt", studyPassages)

	got, err := repo.Passages(ctx, ada, doc.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(studyPassages) {
		t.Fatalf("read %d passages, want %d", len(got), len(studyPassages))
	}
	for i, p := range got {
		if p.ChunkIndex != i || p.Content != studyPassages[i].Content {
			t.Fatalf("passage %d = %+v, want the document's own order", i, p)
		}
		if p.Filename != "field-notes.txt" || p.DocumentID != doc.ID {
			t.Fatalf("passage %d does not carry its citation: %+v", i, p)
		}
	}

	if limited, err := repo.Passages(ctx, ada, doc.ID, 1); err != nil || len(limited) != 1 {
		t.Fatalf("a limit of 1 read %d passages: %v", len(limited), err)
	}
	// Another user's document reads as no passages at all, rather than as an
	// error that would confirm the id exists.
	if theirs, err := repo.Passages(ctx, bob, doc.ID, 0); err != nil || len(theirs) != 0 {
		t.Fatalf("Bob read %d of Ada's passages: %v", len(theirs), err)
	}
}

// A study plan's node is a `project` -- the one mirrored type whose name is not
// its table's -- and it follows its row: created with it, renamed with it, and
// removed by the trigger however the row goes.
func TestAStudyPlanNodeFollowsItsRow(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	graphSvc := graph.NewService(graph.Deps{Store: graph.NewRepository(pool)})
	repo := study.NewRepository(pool)
	svc := study.NewService(study.Deps{Store: repo, Graph: graphSvc})
	owner := makeUser(t, pool, "ada@example.com")

	plan, err := svc.CreatePlan(ctx, owner, study.CreatePlanInput{Title: "Linear algebra finals"})
	if err != nil {
		t.Fatal(err)
	}

	node := func() (string, string) {
		t.Helper()
		var nodeType, label string
		err := pool.QueryRow(`SELECT type, label FROM knowledge_nodes
			WHERE user_id = $1 AND ref_table = 'study_plans' AND ref_id = $2`, owner, plan.ID).Scan(&nodeType, &label)
		if err != nil {
			t.Fatalf("the plan has no node: %v", err)
		}
		return nodeType, label
	}
	nodeType, label := node()
	if nodeType != graph.NodeProject || label != "Linear algebra finals" {
		t.Fatalf("node = %q/%q, want a project node labelled with the title", nodeType, label)
	}

	// Renaming the plan renames the node: a node carrying a title the user has
	// stopped using is one the chat mention scan matches on the wrong thing.
	if _, err := svc.UpdatePlan(ctx, owner, plan.ID, study.UpdatePlanInput{
		Title: optional.Of("Linear algebra resit"),
	}); err != nil {
		t.Fatal(err)
	}
	if _, label = node(); label != "Linear algebra resit" {
		t.Fatalf("label = %q, want the new title", label)
	}

	// The node is backed by a row, so the graph refuses to delete it directly
	// -- and that has to hold for a `project`, whose type is otherwise one a
	// conversation can create and a user may delete.
	var nodeID uuid.UUID
	if err := pool.QueryRow(`SELECT id FROM knowledge_nodes
		WHERE user_id = $1 AND ref_table = 'study_plans' AND ref_id = $2`, owner, plan.ID).Scan(&nodeID); err != nil {
		t.Fatal(err)
	}
	if err := graphSvc.DeleteNode(ctx, owner, nodeID); !errors.Is(err, graph.ErrNodeIsBacked) {
		t.Fatalf("deleting a mirrored project node = %v, want ErrNodeIsBacked", err)
	}

	// Deleting the row through SQL -- a path the service never sees -- still
	// takes the node, because the rule is a trigger.
	if _, err := pool.Exec(`DELETE FROM study_plans WHERE id = $1`, plan.ID); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := pool.QueryRow(`SELECT count(*) FROM knowledge_nodes
		WHERE ref_table = 'study_plans' AND ref_id = $1`, plan.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("%d nodes outlived the plan", n)
	}
}

// The ref_table allow-list migration 000010 widens has to accept study_plans
// and still refuse anything else, which is the point of it being a CHECK
// rather than a string in the Go code.
func TestTheRefTableAllowListAcceptsStudyPlansAndNothingNew(t *testing.T) {
	pool := testDB(t)
	owner := makeUser(t, pool, "ada@example.com")

	_, err := pool.Exec(`INSERT INTO knowledge_nodes (user_id, type, label, ref_table, ref_id)
		VALUES ($1, 'project', 'invented', 'quizzes', $2)`, owner, uuid.New())
	if err == nil {
		t.Fatal("a node mirroring a table this phase does not have was accepted")
	}
}
