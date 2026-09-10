package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/jashveer/lifeos/backend/internal/documents"
	"github.com/jashveer/lifeos/backend/internal/notes"
	"github.com/jashveer/lifeos/backend/internal/validate"
)

func prepare(t *testing.T, w *world, tool string, args Args) Call {
	t.Helper()
	call, err := w.reg.Prepare(context.Background(), w.user, tool, args)
	if err != nil {
		t.Fatalf("Prepare(%s, %v): %v", tool, args, err)
	}
	return call
}

func declined(t *testing.T, w *world, tool string, args Args) string {
	t.Helper()
	_, err := w.reg.Prepare(context.Background(), w.user, tool, args)
	var ae *ArgumentError
	if !errors.As(err, &ae) {
		t.Fatalf("Prepare(%s, %v) = %v, want an ArgumentError", tool, args, err)
	}
	if !errors.Is(err, ErrInvalidArguments) {
		t.Fatal("an ArgumentError does not match ErrInvalidArguments")
	}
	return ae.Reason
}

func input(t *testing.T, call Call) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(call.Input, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// --- create_task --------------------------------------------------------------

// The call as the model writes it, turned into the input the user approves:
// the relative date resolved by Go rather than by the model, the priority
// word mapped onto the closed set.
func TestCreateTaskCanonicalizesWhatTheModelWrote(t *testing.T) {
	w := newWorld()
	call := prepare(t, w, CreateTask, Args{
		"title": "  Buy milk ", "deadline": "tomorrow", "priority": "urgent", "tags": "home, errands",
	})
	in := input(t, call)
	switch {
	case in["title"] != "Buy milk":
		t.Fatalf("title = %v", in["title"])
	case in["deadline"] != "2026-09-11T00:00:00Z":
		t.Fatalf("deadline = %v, want the day after the fixed Thursday", in["deadline"])
	case in["priority"] != "high":
		t.Fatalf("priority = %v, want urgent mapped to high", in["priority"])
	}
	if call.Permission != Write {
		t.Fatalf("permission = %v", call.Permission)
	}
	want := `Create a task "Buy milk" (priority high, due Fri 11 Sep 2026, tags home, errands).`
	if call.Summary != want {
		t.Fatalf("summary = %q, want %q", call.Summary, want)
	}
}

// The aliases a model reaches for, and the placeholders it writes for "not
// given", are shape rather than content.
func TestCreateTaskIsLenientAboutShape(t *testing.T) {
	w := newWorld()
	in := input(t, prepare(t, w, CreateTask, Args{
		"name": "Call the bank", "due_date": "2026-09-30", "priority": "none", "description": "N/A",
	}))
	if in["title"] != "Call the bank" || in["deadline"] != "2026-09-30T00:00:00Z" {
		t.Fatalf("input = %v", in)
	}
	if _, set := in["priority"]; set {
		t.Fatalf("a placeholder priority was kept: %v", in)
	}
	if _, set := in["description"]; set {
		t.Fatalf("a placeholder description was kept: %v", in)
	}
}

// And strict about content: each of these is a proposal the user must never be
// shown, because it would fail on approval or means nothing.
func TestCreateTaskDeclinesWhatItCannotPropose(t *testing.T) {
	w := newWorld()
	for name, tc := range map[string]struct {
		args Args
		want string
	}{
		"no title":       {Args{"deadline": "tomorrow"}, "needs a title"},
		"a bad priority": {Args{"title": "x", "priority": "cosmic"}, "priority must be one of"},
		"a bad date":     {Args{"title": "x", "deadline": "whenever I feel like it"}, "could not read"},
		"too long":       {Args{"title": strings.Repeat("a", validate.MaxTitleLen+1)}, "title must be at most"},
	} {
		if reason := declined(t, w, CreateTask, tc.args); !strings.Contains(reason, tc.want) {
			t.Fatalf("%s: reason = %q, want it to mention %q", name, reason, tc.want)
		}
	}
}

// --- update_task --------------------------------------------------------------

// A reference resolves to exactly one of the caller's tasks, or the call is
// declined with a reason the model can relay. Guessing is the one thing it
// never does.
func TestUpdateTaskResolvesItsReference(t *testing.T) {
	w := newWorld()
	sched := w.tasks.seed(w.user, "Write the CFS scheduler", "pending")
	w.tasks.seed(w.user, "Fix the antenna guy-line", "pending")
	w.tasks.seed(w.user, "Replace the antenna cable", "pending")
	exact := w.tasks.seed(w.user, "Essay", "in_progress")
	stranger := uuid.New()
	theirs := w.tasks.seed(stranger, "Write the CFS scheduler", "pending")

	for _, tc := range []struct {
		name string
		args Args
		want uuid.UUID
	}{
		{"a word from the title", Args{"task": "scheduler", "status": "done"}, sched.ID},
		{"a reference with the word task in it", Args{"task": "the scheduler task", "status": "done"}, sched.ID},
		{"an exact title wins over a substring", Args{"task": "essay", "status": "done"}, exact.ID},
		{"an id", Args{"task_id": sched.ID.String(), "priority": "high"}, sched.ID},
		{"a title in the id field", Args{"task_id": "CFS scheduler", "priority": "high"}, sched.ID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := input(t, prepare(t, w, UpdateTask, tc.args))
			if in["task_id"] != tc.want.String() {
				t.Fatalf("resolved to %v, want %v", in["task_id"], tc.want)
			}
		})
	}

	for _, tc := range []struct {
		name string
		args Args
		want string
	}{
		{"ambiguous", Args{"task": "antenna", "status": "done"}, `2 tasks match "antenna"`},
		{"nothing matches", Args{"task": "passport", "status": "done"}, `no task matches "passport"`},
		{"no reference", Args{"status": "done"}, "say which task"},
		// Somebody else's task is not a task of this user's -- the same answer
		// as an id that does not exist.
		{"another user's id", Args{"task_id": theirs.ID.String(), "status": "done"}, "no such task"},
		{"nothing to change", Args{"task": "scheduler"}, "nothing to change"},
		{"already that status", Args{"task": "essay", "status": "in progress"}, "nothing to change"},
		{"a status that is not one", Args{"task": "scheduler", "status": "vibing"}, "status must be one of"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if reason := declined(t, w, UpdateTask, tc.args); !strings.Contains(reason, tc.want) {
				t.Fatalf("reason = %q, want it to mention %q", reason, tc.want)
			}
		})
	}
}

func TestUpdateTaskSummaryNamesTheTaskAndTheChange(t *testing.T) {
	w := newWorld()
	w.tasks.seed(w.user, "Write the CFS scheduler", "pending")
	call := prepare(t, w, UpdateTask, Args{"task": "scheduler", "status": "completed", "priority": "high"})
	want := `Update the task "Write the CFS scheduler": status → completed, priority → high.`
	if call.Summary != want {
		t.Fatalf("summary = %q, want %q", call.Summary, want)
	}
}

// Approving an update changes the task it resolved to, by id, whatever has
// happened to its title since.
func TestAnApprovedUpdateGoesByID(t *testing.T) {
	w := newWorld()
	task := w.tasks.seed(w.user, "Write the CFS scheduler", "pending")
	call := prepare(t, w, UpdateTask, Args{"task": "scheduler", "status": "completed"})
	id := w.ledger.add(w.user, call, "proposed")
	exec, err := w.reg.RunApproved(context.Background(), w.user, id)
	if err != nil || exec.Err != nil {
		t.Fatal(err, exec.Err)
	}
	if len(w.tasks.updates) != 1 || w.tasks.updates[0] != task.ID {
		t.Fatalf("updated %v, want %v", w.tasks.updates, task.ID)
	}
	if exec.Result.Tasks[0].Status != "completed" {
		t.Fatalf("result = %+v", exec.Result.Tasks)
	}
}

// --- create_goal and create_note ----------------------------------------------

func TestCreateGoalFilesAnUnstatedTypeAsPersonalAndSaysSo(t *testing.T) {
	w := newWorld()
	call := prepare(t, w, CreateGoal, Args{"title": "Run a marathon", "deadline": "end of the year"})
	in := input(t, call)
	if in["type"] != DefaultGoalType || in["deadline"] != "2026-12-31T00:00:00Z" {
		t.Fatalf("input = %v", in)
	}
	if !strings.Contains(call.Summary, "type personal") {
		t.Fatalf("the proposal does not show the type it chose: %q", call.Summary)
	}
	if in := input(t, prepare(t, w, CreateGoal, Args{"title": "Save £5k", "type": "savings"})); in["type"] != "financial" {
		t.Fatalf("type = %v, want savings mapped to financial", in["type"])
	}
}

func TestCreateNoteTakesItsTitleFromTheUsersTextWhenTheModelGivesNone(t *testing.T) {
	w := newWorld()
	in := input(t, prepare(t, w, CreateNote, Args{"title": "", "content": "the wifi password for the cabin is hunter2"}))
	if in["title"] != "the wifi password for the cabin is hunter2" {
		t.Fatalf("title = %v", in["title"])
	}
	long := strings.Repeat("word ", 40)
	in = input(t, prepare(t, w, CreateNote, Args{"content": long}))
	if title, _ := in["title"].(string); len([]rune(title)) > maxDerivedTitle+1 || !strings.HasSuffix(title, "…") {
		t.Fatalf("derived title = %q", title)
	}
	if reason := declined(t, w, CreateNote, Args{}); !strings.Contains(reason, "needs a title or some text") {
		t.Fatalf("reason = %q", reason)
	}
}

// --- the read tools -------------------------------------------------------------

func TestSearchTasksTriesThePhraseThenItsWords(t *testing.T) {
	w := newWorld()
	w.tasks.seed(w.user, "Write the CFS scheduler", "pending")
	w.tasks.seed(w.user, "Fix the antenna guy-line", "pending")

	call := prepare(t, w, SearchTasks, Args{"query": "scheduler work", "status": "todo"})
	if in := input(t, call); in["status"] != "pending" {
		t.Fatalf("status = %v, want todo mapped to pending", in["status"])
	}
	res, err := w.reg.RunRead(context.Background(), w.user, call)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Tasks) != 1 || res.Tasks[0].Title != "Write the CFS scheduler" {
		t.Fatalf("found %+v", res.Tasks)
	}
	// The phrase first, then the words -- and "work" found nothing on its own.
	var queries []string
	for _, f := range w.tasks.filters {
		queries = append(queries, f.Query)
	}
	if strings.Join(queries, "|") != "scheduler work|scheduler|work" {
		t.Fatalf("searched %q", queries)
	}
	out, _ := json.Marshal(res.Output)
	if !strings.Contains(string(out), `"count":1`) || !strings.Contains(string(out), `"more":false`) {
		t.Fatalf("output = %s", out)
	}
}

func TestSearchReportsThatThereWasMore(t *testing.T) {
	w := newWorld()
	for i := 0; i < SearchLimit+2; i++ {
		w.tasks.seed(w.user, "antenna job", "pending")
	}
	res, err := w.reg.RunRead(context.Background(), w.user, prepare(t, w, SearchTasks, Args{"query": "antenna"}))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Tasks) != SearchLimit || !res.More {
		t.Fatalf("returned %d, more = %v", len(res.Tasks), res.More)
	}
}

func TestSearchNeverCrossesUsers(t *testing.T) {
	w := newWorld()
	w.tasks.seed(uuid.New(), "the antenna", "pending")
	w.notes.byUser[uuid.New()] = []notes.Note{{ID: uuid.New(), Title: "antenna notes"}}
	for _, tool := range []string{SearchTasks, SearchNotes} {
		res, err := w.reg.RunRead(context.Background(), w.user, prepare(t, w, tool, Args{"query": "antenna"}))
		if err != nil {
			t.Fatal(err)
		}
		if res.Count() != 0 {
			t.Fatalf("%s found another user's records: %+v", tool, res)
		}
	}
}

// search_documents is the Phase 3 search with the chat floor, so a query the
// model chose cannot talk a distant chunk into the context.
func TestSearchDocumentsUsesTheRetrievalFloor(t *testing.T) {
	w := newWorld()
	w.docs.byUser[w.user] = []documents.SearchResult{{DocumentID: uuid.New(), Filename: "field-notes.txt", Content: "aurora", Similarity: 0.71}}
	res, err := w.reg.RunRead(context.Background(), w.user, prepare(t, w, SearchDocuments, Args{"q": "aurora"}))
	if err != nil {
		t.Fatal(err)
	}
	if got := w.docs.queries[0]; got.MinSimilarity != 0.5 || got.Limit != SearchLimit || got.Query != "aurora" {
		t.Fatalf("search = %+v", got)
	}
	out, _ := json.Marshal(res.Output)
	if strings.Contains(string(out), "aurora\"") && strings.Contains(string(out), "content") {
		t.Fatalf("the recorded result copies the passage text: %s", out)
	}
	if reason := declined(t, w, SearchDocuments, Args{}); !strings.Contains(reason, "say what to search") {
		t.Fatalf("reason = %q", reason)
	}
}

// A read tool failing is an outage, not an argument problem: it must reach the
// caller as an ordinary error so the turn fails rather than answering "found
// nothing".
func TestAReadFailureIsNotAnArgumentError(t *testing.T) {
	w := newWorld()
	w.tasks.err = errors.New("connection refused")
	_, err := w.reg.RunRead(context.Background(), w.user, prepare(t, w, SearchTasks, Args{"query": "x"}))
	if err == nil || errors.Is(err, ErrInvalidArguments) {
		t.Fatalf("err = %v, want a plain failure", err)
	}
}

// --- Args -----------------------------------------------------------------------

func TestArgsReadWhatModelsWrite(t *testing.T) {
	a := ParseArgs(json.RawMessage(`{"n": 5, "flag": true, "blank": "  ", "none": "None", "list": "a, ,b", "arr": ["x", 2, null]}`))
	if a.String("n") != "5" || a.String("flag") != "true" {
		t.Fatalf("numbers and booleans: %q %q", a.String("n"), a.String("flag"))
	}
	if a.String("blank", "none", "missing") != "" {
		t.Fatal("a placeholder counted as a value")
	}
	if got := a.Strings("list"); strings.Join(got, "|") != "a|b" {
		t.Fatalf("comma list = %q", got)
	}
	if got := a.Strings("arr"); strings.Join(got, "|") != "x|2" {
		t.Fatalf("array = %q", got)
	}
	if len(ParseArgs(json.RawMessage(`"just a string"`))) != 0 || len(ParseArgs(nil)) != 0 {
		t.Fatal("a non-object became arguments")
	}
}
