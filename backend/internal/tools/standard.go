package tools

import (
	"context"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/jashveer/lifeos/backend/internal/calendar"
	"github.com/jashveer/lifeos/backend/internal/documents"
	"github.com/jashveer/lifeos/backend/internal/finance"
	"github.com/jashveer/lifeos/backend/internal/goals"
	"github.com/jashveer/lifeos/backend/internal/notes"
	"github.com/jashveer/lifeos/backend/internal/study"
	"github.com/jashveer/lifeos/backend/internal/tasks"
)

// The services the tools call into. Each is the narrowest slice of an existing
// Phase 2/3 service, and each takes the owner id -- so a tool cannot reach
// another user's data for the same structural reason the repositories cannot.
//
// Note what is missing. No interface here has a Delete method, and only the
// task one has Update: the briefs scope the tools to creating tasks, goals,
// notes and calendar events and updating tasks, and an interface that does not
// name a method is a guarantee that no tool calls it. Adding a delete tool is a
// deliberate change to one of these, not a line anybody writes by accident.
type (
	TaskService interface {
		List(ctx context.Context, userID uuid.UUID, f tasks.Filter) ([]tasks.Task, error)
		Get(ctx context.Context, userID, id uuid.UUID) (tasks.Task, error)
		Create(ctx context.Context, userID uuid.UUID, in tasks.CreateInput) (tasks.Task, error)
		Update(ctx context.Context, userID, id uuid.UUID, in tasks.UpdateInput) (tasks.Task, error)
	}
	GoalService interface {
		List(ctx context.Context, userID uuid.UUID, f goals.Filter) ([]goals.Goal, error)
		Create(ctx context.Context, userID uuid.UUID, in goals.CreateInput) (goals.Goal, error)
	}
	NoteService interface {
		List(ctx context.Context, userID uuid.UUID, f notes.Filter) ([]notes.Note, error)
		Create(ctx context.Context, userID uuid.UUID, in notes.CreateInput) (notes.Note, error)
	}
	DocumentSearcher interface {
		Search(ctx context.Context, userID uuid.UUID, q documents.SearchQuery) ([]documents.SearchResult, error)
	}
	CalendarService interface {
		List(ctx context.Context, userID uuid.UUID, f calendar.Filter) ([]calendar.Event, error)
		Create(ctx context.Context, userID uuid.UUID, in calendar.CreateInput) (calendar.Event, error)
	}
	// FinanceService is Phase 9's expenses. Summarize is the interesting one:
	// it is here so analyze_spending can answer "how much did I spend on food"
	// with a number Postgres worked out, instead of a year of rows for a 3B
	// model to add up. Categories and CategoryByName are read-only -- there is
	// no CreateCategory, so no tool can add one, which is what makes "the
	// category list is the user's" structural rather than a rule.
	FinanceService interface {
		List(ctx context.Context, userID uuid.UUID, f finance.Filter) ([]finance.Expense, error)
		Summarize(ctx context.Context, userID uuid.UUID, f finance.Filter) (finance.Summary, error)
		Categories(ctx context.Context, userID uuid.UUID) ([]finance.Category, error)
		CategoryByName(ctx context.Context, userID uuid.UUID, name string) (finance.Category, error)
		Create(ctx context.Context, userID uuid.UUID, in finance.CreateInput) (finance.Expense, error)
	}
	// StudyService is Phase 10a's study plans and flashcards.
	//
	// ProposeFlashcards is the interesting one, and the shape of the interface
	// is the phase's approval story in miniature: it *proposes* cards and
	// writes nothing, and CreateFlashcards -- the write -- takes the cards
	// rather than a document and a count, so there is no method here that
	// generates and stores in one step. A write tool cannot regenerate on
	// approval even by mistake, because the service offers no way to.
	//
	// There is no UpdatePlan and no delete of anything, so no tool can reach
	// one; and ResolveDocument is a read, which is what lets a proposal name a
	// file by the name the user used.
	StudyService interface {
		Plans(ctx context.Context, userID uuid.UUID, f study.Filter) ([]study.Plan, error)
		CreatePlan(ctx context.Context, userID uuid.UUID, in study.CreatePlanInput) (study.Plan, error)
		ProposeFlashcards(ctx context.Context, userID uuid.UUID, in study.GenerateInput) (study.Proposal, error)
		CreateFlashcards(ctx context.Context, userID uuid.UUID, in []study.CreateCardInput) ([]study.Flashcard, error)
		ResolveDocument(ctx context.Context, userID uuid.UUID, ref string) (documents.Document, error)
	}
)

// Services is what the standard tools are built over.
type Services struct {
	Tasks     TaskService
	Goals     GoalService
	Notes     NoteService
	Documents DocumentSearcher
	Calendar  CalendarService
	Finance   FinanceService
	Study     StudyService
	// DocumentMinSimilarity is search_documents' floor. It is the chat
	// retrieval floor, passed in rather than defaulted here, so a document the
	// assistant finds by searching is held to the same bar as one it retrieves
	// on its own.
	DocumentMinSimilarity float64
	// Now is the clock relative dates are resolved against. Nil means
	// time.Now.
	Now func() time.Time
}

// SearchLimit is how many records one search returns. It spends from the same
// context budget as everything else retrieved for a turn, and five is the
// number the Phase 4 heuristic settled on for the same reason.
const SearchLimit = 5

// The names of the tools. They are constants because other packages -- the
// agents that group them, the tests that pin them -- refer to them, and a typo
// in a string would be a tool that silently never gets offered.
const (
	SearchTasks     = "search_tasks"
	SearchGoals     = "search_goals"
	SearchNotes     = "search_notes"
	SearchDocuments = "search_documents"
	SearchCalendar  = "search_calendar"
	CreateTask      = "create_task"
	UpdateTask      = "update_task"
	CreateGoal      = "create_goal"
	CreateNote      = "create_note"
	SearchExpenses  = "search_expenses"
	AnalyzeSpending = "analyze_spending"
	// CreateCalendarEvent is Phase 8's one write. There is no
	// update_calendar_event to go with it: moving an event is the same sharp
	// edge as deleting one -- the wrong meeting moved is a meeting missed --
	// and it needs the same "show the user exactly what would change" that
	// deletion is waiting for. See docs/decisions.md.
	CreateCalendarEvent = "create_calendar_event"
	// CreateExpense is Phase 9's one write, on the same terms: there is no
	// update_expense and no delete, so a number the assistant recorded can be
	// corrected through the API or the UI and not by asking it again.
	CreateExpense = "create_expense"
	// SearchStudyPlans and the two writes below are Phase 10a.
	SearchStudyPlans = "search_study_plans"
	CreateStudyPlan  = "create_study_plan"
	// GenerateFlashcards is the one write in this codebase whose proposal is
	// *content* rather than a restatement of what the user said: the cards are
	// written by a model during preparation and are what the approval card
	// shows. See generateFlashcardsInput.
	GenerateFlashcards = "generate_flashcards"
)

// Standard returns the tools, read tools first.
//
// There is no delete tool. Deleting through a conversation is a sharper edge
// than creating -- an approved create that was wrong costs a click to undo, an
// approved delete of the wrong task costs the task -- and it is deferred to a
// phase that can show the user exactly what would go; see docs/decisions.md.
func Standard(s Services) []Tool {
	if s.Now == nil {
		s.Now = time.Now
	}
	return []Tool{
		searchTasksTool(s),
		searchGoalsTool(s),
		searchNotesTool(s),
		searchDocumentsTool(s),
		searchCalendarTool(s),
		searchExpensesTool(s),
		analyzeSpendingTool(s),
		searchStudyPlansTool(s),
		createTaskTool(s),
		updateTaskTool(s),
		createGoalTool(s),
		createNoteTool(s),
		createCalendarEventTool(s),
		createExpenseTool(s),
		createStudyPlanTool(s),
		generateFlashcardsTool(s),
	}
}

// --- searching ----------------------------------------------------------------

// searchStopWords never become a search term on their own. They are the words
// a model puts in a query for grammar ("the tasks about the antenna") or to
// name the kind of thing being searched, which the tool already knows.
var searchStopWords = map[string]struct{}{
	"the": {}, "and": {}, "for": {}, "with": {}, "about": {}, "that": {}, "this": {},
	"all": {}, "any": {}, "some": {}, "my": {}, "our": {}, "from": {}, "into": {},
	"task": {}, "tasks": {}, "goal": {}, "goals": {}, "note": {}, "notes": {},
	"todo": {}, "item": {}, "items": {}, "event": {}, "events": {}, "calendar": {},
	"expense": {}, "expenses": {}, "spending": {}, "spent": {}, "cost": {}, "costs": {},
	"plan": {}, "plans": {}, "study": {}, "flashcard": {}, "flashcards": {}, "card": {}, "cards": {},
}

// searchTerms is the whole phrase first, then -- as a fallback -- up to three of
// its significant words.
//
// The fallback is for the gap between how a user refers to something and what
// it is called. "The scheduler task" is not a substring of "Write the CFS
// scheduler"; "scheduler" is. The phrase goes first because when it matches it
// is the more specific search, and the words are only tried when it finds
// nothing.
func searchTerms(query string) []string {
	query = strings.TrimSpace(query)
	if query == "" {
		return []string{""}
	}
	terms := []string{query}
	for _, w := range strings.Fields(strings.ToLower(query)) {
		w = strings.Trim(w, `"'.,;:!?()`)
		if utf8.RuneCountInString(w) < 2 || w == strings.ToLower(query) {
			continue
		}
		if _, stop := searchStopWords[w]; stop {
			continue
		}
		terms = append(terms, w)
		if len(terms) == 4 {
			break
		}
	}
	return terms
}

// search runs a text search over one kind of record: the phrase, then its
// words, stopping at the first term that finds anything for the phrase and
// merging the words' results otherwise. It returns up to SearchLimit records and
// whether there were more.
func search[T any](query string, list func(term string, limit int) ([]T, error), id func(T) uuid.UUID) ([]T, bool, error) {
	var out []T
	seen := map[uuid.UUID]struct{}{}
	more := false
	for i, term := range searchTerms(query) {
		found, err := list(term, SearchLimit+1)
		if err != nil {
			return nil, false, err
		}
		if len(found) > SearchLimit {
			more = true
		}
		for _, r := range found {
			if _, dup := seen[id(r)]; dup {
				continue
			}
			seen[id(r)] = struct{}{}
			out = append(out, r)
		}
		if i == 0 && len(out) > 0 {
			break
		}
	}
	if len(out) > SearchLimit {
		out, more = out[:SearchLimit], true
	}
	return out, more, nil
}

// quoted renders a user-visible string for a summary, cut short so one long
// title cannot make a proposal unreadable.
func quoted(s string) string {
	const max = 80
	if utf8.RuneCountInString(s) > max {
		s = string([]rune(s)[:max]) + "…"
	}
	return `"` + s + `"`
}
