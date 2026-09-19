package chat

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/jashveer/lifeos/backend/internal/calendar"
	"github.com/jashveer/lifeos/backend/internal/documents"
	"github.com/jashveer/lifeos/backend/internal/finance"
	"github.com/jashveer/lifeos/backend/internal/goals"
	"github.com/jashveer/lifeos/backend/internal/graph"
	"github.com/jashveer/lifeos/backend/internal/memories"
	"github.com/jashveer/lifeos/backend/internal/notes"
	"github.com/jashveer/lifeos/backend/internal/study"
	"github.com/jashveer/lifeos/backend/internal/tasks"
)

// The six things the orchestrator can retrieve from, and the two things it
// writes to. Each is the narrowest slice of an existing Phase 2/3/5/6 service,
// and each takes the owner id as an argument -- so the orchestrator cannot
// reach another user's data even by mistake, for the same structural reason the
// repositories cannot.
//
// They are interfaces rather than concrete *documents.Service and friends so
// the orchestrator's tests can hand it a corpus without a database.
type (
	// DocumentSearcher is Phase 3's RAG search, called as a service function
	// rather than over HTTP.
	DocumentSearcher interface {
		Search(ctx context.Context, userID uuid.UUID, q documents.SearchQuery) ([]documents.SearchResult, error)
	}
	// MemorySearcher is Phase 5's search over what the assistant has learned
	// about this user in earlier conversations.
	MemorySearcher interface {
		Search(ctx context.Context, userID uuid.UUID, q memories.SearchQuery) ([]memories.SearchResult, error)
	}
	// MemoryExtractor is the other half of Phase 5: it reads a finished
	// exchange and stores the durable facts in it. It is separate from
	// MemorySearcher rather than one interface with two methods because the two
	// are independently switchable -- an operator who wants the assistant to
	// use what it already knows without learning anything new wires the first
	// and not the second (MEMORY_EXTRACTION=false).
	//
	// It is handed a memories.Turn rather than the two messages, because the
	// messages alone are not enough to tell a fact from a request or from a
	// document the answer quoted; see memories.Turn.
	MemoryExtractor interface {
		Extract(ctx context.Context, userID, conversationID uuid.UUID, turn memories.Turn) ([]memories.Memory, error)
	}
	// GraphSearcher is Phase 6's 1-hop lookup: given the text of a question, it
	// answers with the neighbourhoods of the nodes that text names. The
	// matching lives behind the interface rather than here, because the labels
	// to match against are the graph's and the orchestrator has no business
	// holding a copy of them.
	GraphSearcher interface {
		Mentioned(ctx context.Context, userID uuid.UUID, text string, limit int) ([]graph.Neighborhood, error)
	}
	// GraphExtractor is the other half of Phase 6, split from the searcher for
	// exactly the reason MemoryExtractor is split from MemorySearcher: the two
	// are independently switchable, and an operator who wants the assistant to
	// use the graph without growing it wires the first and not the second
	// (GRAPH_EXTRACTION=false).
	GraphExtractor interface {
		Extract(ctx context.Context, userID, conversationID uuid.UUID, turn graph.Turn) ([]graph.Edge, error)
	}
	TaskLister interface {
		List(ctx context.Context, userID uuid.UUID, f tasks.Filter) ([]tasks.Task, error)
	}
	GoalLister interface {
		List(ctx context.Context, userID uuid.UUID, f goals.Filter) ([]goals.Goal, error)
	}
	NoteLister interface {
		List(ctx context.Context, userID uuid.UUID, f notes.Filter) ([]notes.Note, error)
	}
	// EventLister is Phase 8's calendar, read the way the other resource
	// listers are: as a plain function with the owner id as an argument. It is
	// optional -- a nil one is an assistant that retrieves exactly what Phase 7
	// did -- because the module is, and because an operator running without a
	// calendar should not get an empty section implying they have one.
	EventLister interface {
		List(ctx context.Context, userID uuid.UUID, f calendar.Filter) ([]calendar.Event, error)
	}
)

// retrieve gathers the context for one question, strongest first: whatever a
// read tool found for it, then document chunks that actually match it, then
// the memories that match it, then what is on the user's calendar in the next
// day or two, then their current tasks, goals and notes.
//
// Documents and memories are matched semantically. Tasks, goals and notes are
// not -- they are selected by a plain heuristic (in progress, due soonest,
// recently touched), because they have no embeddings yet. That difference is
// visible to the model: a chunk or a memory arrives ranked by similarity, an
// item arrives as background the question may or may not be about, and the
// system prompt forbids citing anything that does not answer the question.
//
// The calendar leads the heuristic items because it is the only one of them
// selected by time rather than by status: an event in the next two days is
// about now in a way that a pending task is not, which is what makes "what does
// my day look like" answerable from background context at all.
//
// Memories come after documents and before the heuristic items because that is
// their standing: a fact the assistant recorded about the user is stronger
// evidence than "here is a task you have open", and weaker than the user's own
// document saying so. The graph sits between the memories and the heuristic
// items for the same kind of reason: it fires only when the question actually
// names something, which makes it a targeted match rather than background --
// but what it carries is a set of links, not a claim in the user's own words.
//
// Any retrieval failure fails the whole turn. Answering without the tasks
// table because its query errored would produce "I could not find anything in
// your tasks" -- a false statement about the user's data, which is exactly
// what this phase's grounding rule exists to prevent.
func (s *Service) retrieve(ctx context.Context, userID uuid.UUID, question string, first []Source) ([]Source, error) {
	// A read tool's results lead: they are the one thing retrieved because the
	// user asked for exactly it.
	out := append([]Source(nil), first...)

	found, err := s.docs.Search(ctx, userID, documents.SearchQuery{
		Query:         truncate(question, documents.MaxQueryLen),
		Limit:         MaxDocumentChunks,
		MinSimilarity: s.opts.MinSimilarity,
	})
	if err != nil {
		return nil, fmt.Errorf("retrieve documents: %w", err)
	}
	for _, r := range found {
		idx, sim := r.ChunkIndex, r.Similarity
		out = append(out, Source{
			Type: SourceDocument, ID: r.DocumentID, Title: r.Filename,
			ChunkIndex: &idx, Similarity: &sim,
			Excerpt: truncate(r.Content, MaxExcerptChars),
		})
	}

	remembered, err := s.recall(ctx, userID, question)
	if err != nil {
		return nil, err
	}
	out = append(out, remembered...)

	linked, err := s.related(ctx, userID, question)
	if err != nil {
		return nil, err
	}
	out = append(out, linked...)

	soon, err := s.upcomingEvents(ctx, userID)
	if err != nil {
		return nil, err
	}
	out = append(out, soon...)

	found2, err := s.currentTasks(ctx, userID)
	if err != nil {
		return nil, err
	}
	out = append(out, found2...)

	activeGoals, err := s.goals.List(ctx, userID, goals.Filter{
		Status: "active", Sort: "deadline", Limit: MaxGoals,
	})
	if err != nil {
		return nil, fmt.Errorf("retrieve goals: %w", err)
	}
	for _, g := range activeGoals {
		out = append(out, Source{
			Type: SourceGoal, ID: g.ID, Title: g.Title,
			Excerpt: truncate(goalSummary(g), MaxExcerptChars),
		})
	}

	recentNotes, err := s.notes.List(ctx, userID, notes.Filter{Sort: "-updated_at", Limit: MaxNotes})
	if err != nil {
		return nil, fmt.Errorf("retrieve notes: %w", err)
	}
	for _, n := range recentNotes {
		out = append(out, Source{
			Type: SourceNote, ID: n.ID, Title: n.Title,
			Excerpt: truncate(noteSummary(n), MaxExcerptChars),
		})
	}

	return labelled(deduplicated(out)), nil
}

// deduplicated drops a source already retrieved by an earlier step. A task the
// search tool found is usually also one the heuristic picks up, and showing the
// model the same task twice under two labels would split its citations between
// them. The first occurrence wins, which keeps the tool's -- it came first,
// and it carries the tool's name.
func deduplicated(in []Source) []Source {
	type key struct {
		kind  string
		id    uuid.UUID
		chunk int
	}
	seen := make(map[key]struct{}, len(in))
	out := in[:0:0]
	for _, s := range in {
		// A source with no id is not a record, so it cannot be the same record
		// twice. A "spending" total is the one kind: it carries the zero uuid
		// because there is no row to open, and two of them in a turn are two
		// different answers that would otherwise collapse into one.
		if s.ID == uuid.Nil {
			out = append(out, s)
			continue
		}
		k := key{kind: s.Type, id: s.ID, chunk: -1}
		if s.ChunkIndex != nil {
			k.chunk = *s.ChunkIndex
		}
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, s)
	}
	return out
}

// recall is the memory half of retrieval. It is a no-op when no memory service
// is wired, which is what an assistant with the memory system switched off
// looks like: it retrieves documents and items exactly as Phase 4 did.
//
// The memory's type becomes the source title, because a memory has no title of
// its own and its type is the one word that says what kind of claim it is.
func (s *Service) recall(ctx context.Context, userID uuid.UUID, question string) ([]Source, error) {
	if s.memories == nil {
		return nil, nil
	}
	found, err := s.memories.Search(ctx, userID, memories.SearchQuery{
		Query:         truncate(question, memories.MaxQueryLen),
		Limit:         MaxMemories,
		MinSimilarity: s.opts.MemoryMinSimilarity,
	})
	if err != nil {
		return nil, fmt.Errorf("retrieve memories: %w", err)
	}
	out := make([]Source, 0, len(found))
	for _, m := range found {
		sim := m.Similarity
		out = append(out, Source{
			Type: SourceMemory, ID: m.ID, Title: m.Type, Similarity: &sim,
			Excerpt: truncate(m.Content, MaxExcerptChars),
		})
	}
	return out, nil
}

// related is the knowledge-graph half of retrieval. It is a no-op when no
// graph service is wired, which is what an assistant with the graph switched
// off looks like: it retrieves exactly what Phase 5 did.
//
// The node's label becomes the source title and its neighbourhood becomes the
// excerpt. There is no similarity to report: a node either was named in the
// question or it was not, and reporting a score for an exact match would
// invite the model to weigh it against the cosine scores next to it, which
// measure something else entirely.
func (s *Service) related(ctx context.Context, userID uuid.UUID, question string) ([]Source, error) {
	if s.graph == nil {
		return nil, nil
	}
	found, err := s.graph.Mentioned(ctx, userID, truncate(question, MaxGraphQueryChars), MaxGraphNodes)
	if err != nil {
		return nil, fmt.Errorf("retrieve graph: %w", err)
	}
	out := make([]Source, 0, len(found))
	for _, n := range found {
		out = append(out, Source{
			Type: SourceGraph, ID: n.Node.ID, Title: n.Node.Label,
			Excerpt: truncate(graphSummary(n), MaxExcerptChars),
		})
	}
	return out, nil
}

// upcomingEvents is the calendar heuristic: what is on between now and
// EventWindow from now, soonest first.
//
// It is a plain range query rather than a retrieval subsystem, and the window
// is deliberately short. An event next month does not belong in the context of
// every question; the one this afternoon does, and a question that is actually
// about next month routes to search_calendar, which takes the dates from the
// user. The window starts at the top of today rather than at `now` so an event
// that began this morning is still in front of the model at four in the
// afternoon -- "what have I got on today" is a question about the whole day.
func (s *Service) upcomingEvents(ctx context.Context, userID uuid.UUID) ([]Source, error) {
	if s.calendar == nil {
		return nil, nil
	}
	now := s.now().UTC()
	from := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	found, err := s.calendar.List(ctx, userID, calendar.Filter{
		Start: from, End: now.Add(EventWindow), Sort: calendar.DefaultSort, Limit: MaxEvents,
	})
	if err != nil {
		return nil, fmt.Errorf("retrieve calendar: %w", err)
	}
	out := make([]Source, 0, len(found))
	for _, e := range found {
		out = append(out, Source{
			Type: SourceEvent, ID: e.ID, Title: e.Title,
			Excerpt: truncate(eventSummary(e), MaxExcerptChars),
		})
	}
	return out, nil
}

// currentTasks is the task heuristic: what the user is working on, then what
// is due soonest. Two queries rather than one because Filter takes a single
// status, and "in progress" and "due next" are different questions.
func (s *Service) currentTasks(ctx context.Context, userID uuid.UUID) ([]Source, error) {
	const inProgress = 3

	active, err := s.tasks.List(ctx, userID, tasks.Filter{
		Status: "in_progress", Sort: "-updated_at", Limit: inProgress,
	})
	if err != nil {
		return nil, fmt.Errorf("retrieve tasks: %w", err)
	}
	// deadline sorts ASC NULLS LAST, so undated tasks fill the remaining slots
	// only once the dated ones are exhausted -- which is what "upcoming
	// deadlines" should mean.
	upcoming, err := s.tasks.List(ctx, userID, tasks.Filter{
		Status: "pending", Sort: "deadline", Limit: MaxTasks,
	})
	if err != nil {
		return nil, fmt.Errorf("retrieve tasks: %w", err)
	}

	out := make([]Source, 0, MaxTasks)
	seen := make(map[uuid.UUID]struct{}, MaxTasks)
	for _, t := range append(active, upcoming...) {
		if len(out) == MaxTasks {
			break
		}
		if _, dup := seen[t.ID]; dup {
			continue
		}
		seen[t.ID] = struct{}{}
		out = append(out, Source{
			Type: SourceTask, ID: t.ID, Title: t.Title,
			Excerpt: truncate(taskSummary(t), MaxExcerptChars),
		})
	}
	return out, nil
}

// labelled assigns S1…Sn in order and drops anything past the context budget.
//
// A source over the budget is dropped whole rather than truncated: a source
// listed as retrieved but carrying none of the text it would be cited for is
// worse than one that is absent, because the citation trail would claim
// something the model never saw.
func labelled(in []Source) []Source {
	out := make([]Source, 0, len(in))
	used := 0
	for _, s := range in {
		size := len(s.Title) + len(s.Excerpt) + 64 // 64 covers the header line
		if used+size > MaxContextChars && len(out) > 0 {
			continue
		}
		used += size
		s.Label = label(len(out))
		out = append(out, s)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// --- item summaries ---------------------------------------------------------
//
// Each renders one Phase 2 row as the few lines the model is shown. They are
// deliberately telegraphic: the fields that decide whether an item answers the
// question (status, priority, dates) come first, free text after.

// graphSummary renders one node's neighbourhood as the lines the model is
// shown.
//
// Every neighbour is written as a whole triple in subject-relationship-object
// order, with both types named, rather than as an arrow relative to the node
// the block is about. Direction is the meaning here -- "the user STUDIES Go"
// and "Go STUDIES the user" are different claims -- and a rendering the model
// has to combine with a header line to work out which way round it is, is a
// rendering it will sometimes get backwards.
func graphSummary(n graph.Neighborhood) string {
	lines := make([]string, 0, len(n.Neighbors)+1)
	if n.Node.RefTable != nil && n.Node.RefID != nil {
		// The node mirrors a record, so say which. It lets a client follow the
		// link, and it tells the model that the goal named on the far side of
		// an edge is the same goal it may have been given as its own source --
		// which is where the deadline and the status come from, since the node
		// itself carries neither.
		lines = append(lines, "this is the "+singular(*n.Node.RefTable)+" "+n.Node.RefID.String())
	}
	for _, nb := range n.Neighbors {
		from, to := n.Node, nb.Node
		if nb.Incoming {
			from, to = nb.Node, n.Node
		}
		lines = append(lines, fmt.Sprintf("%s (%s) %s %s (%s) · confidence %.2f",
			strconv.Quote(from.Label), from.Type, nb.Edge.Relationship,
			strconv.Quote(to.Label), to.Type, nb.Edge.Confidence))
	}
	return strings.Join(lines, "\n")
}

// singular turns a ref_table name into the words for one of its rows:
// "calendar_events" is "calendar event", not "calendar_event".
func singular(table string) string {
	return strings.TrimSuffix(strings.ReplaceAll(table, "_", " "), "s")
}

// eventSummary renders one event as the line the model is shown. When it is
// comes first and is spelled in full, including the weekday: the model is
// answering questions like "what is on tomorrow", and a bare date makes it do
// calendar arithmetic it is bad at.
func eventSummary(e calendar.Event) string {
	parts := []string{"starts " + e.StartTime.UTC().Format("Mon 2 Jan 2006 15:04") + " UTC"}
	if e.AllDay {
		parts = []string{"all day on " + e.StartTime.UTC().Format("Mon 2 Jan 2006")}
		if last := e.EndTime.Add(-time.Nanosecond).UTC(); last.Format(time.DateOnly) != e.StartTime.UTC().Format(time.DateOnly) {
			parts = []string{"all day from " + e.StartTime.UTC().Format("Mon 2 Jan 2006") +
				" to " + last.Format("Mon 2 Jan 2006")}
		}
	} else {
		parts = append(parts, "ends "+e.EndTime.UTC().Format("Mon 2 Jan 2006 15:04")+" UTC")
	}
	if e.Location != nil && *e.Location != "" {
		parts = append(parts, "at "+*e.Location)
	}
	if e.RecurrenceRule != nil && *e.RecurrenceRule != "" {
		// Said plainly, because the repeats are not rows: the model is being
		// shown one event and must not describe next week's as scheduled.
		parts = append(parts, "repeats (only this occurrence is recorded)")
	}
	return withDescription(strings.Join(parts, " · "), e.Description)
}

// expenseTitle is what one expense is called in the context block. It is
// finance.NodeLabel -- what the expense is called in the knowledge graph -- so
// the same purchase reads the same whichever way the model meets it.
func expenseTitle(e finance.Expense) string { return finance.NodeLabel(e) }

// expenseSummary renders one expense as the line the model is shown.
//
// The amount comes first and carries its currency, because the amount is what
// was asked about and a bare number would be one the model might attach to the
// wrong currency in an answer covering two. The date is spelled in full,
// including the weekday, for the reason eventSummary gives: the model should
// never have to work out what day a date was.
func expenseSummary(e finance.Expense) string {
	parts := []string{e.Currency + " " + e.Amount.String(),
		"on " + e.Date.UTC().Format("Mon 2 Jan 2006")}
	if e.CategoryName != nil && *e.CategoryName != "" {
		parts = append(parts, "category "+*e.CategoryName)
	} else {
		parts = append(parts, "no category")
	}
	if e.RelatedDocumentID != nil {
		// Said plainly: there is a receipt on file, and nothing has read it.
		parts = append(parts, "has a document attached (its contents are not shown here)")
	}
	return withDescription(strings.Join(parts, " · "), e.Description)
}

// studyPlanSummary renders one study plan as the line the model is shown.
//
// The card count is in it because it is the difference between a plan that
// exists and a plan that has been worked on, and the source document is named
// because "what am I studying" is very often really "which file was that".
func studyPlanSummary(p study.Plan) string {
	parts := []string{"status " + p.Status, plural(p.CardCount, "flashcard")}
	if p.DocumentName != nil && *p.DocumentName != "" {
		parts = append(parts, "built from "+*p.DocumentName)
	}
	return withDescription(strings.Join(parts, " · "), p.Description)
}

// plural renders a count with its noun: "1 flashcard", "24 flashcards".
func plural(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

func taskSummary(t tasks.Task) string {
	parts := []string{"priority " + t.Priority, "status " + t.Status}
	if t.Deadline != nil {
		parts = append(parts, "due "+t.Deadline.UTC().Format(time.DateOnly))
	}
	if t.Category != nil && *t.Category != "" {
		parts = append(parts, "category "+*t.Category)
	}
	if len(t.Tags) > 0 {
		parts = append(parts, "tags "+strings.Join(t.Tags, ", "))
	}
	return withDescription(strings.Join(parts, " · "), t.Description)
}

func goalSummary(g goals.Goal) string {
	parts := []string{"type " + g.Type, "status " + g.Status}
	if g.Deadline != nil {
		parts = append(parts, "deadline "+g.Deadline.UTC().Format(time.DateOnly))
	}
	if len(g.Milestones) > 0 {
		done := 0
		for _, m := range g.Milestones {
			if m.Completed {
				done++
			}
		}
		parts = append(parts, fmt.Sprintf("milestones %d of %d complete", done, len(g.Milestones)))
	}
	return withDescription(strings.Join(parts, " · "), g.Description)
}

func noteSummary(n notes.Note) string {
	head := ""
	if len(n.Tags) > 0 {
		head = "tags " + strings.Join(n.Tags, ", ")
	}
	content := strings.TrimSpace(n.Content)
	switch {
	case head == "":
		return content
	case content == "":
		return head
	}
	return head + "\n" + content
}

func withDescription(head string, description *string) string {
	if description == nil || strings.TrimSpace(*description) == "" {
		return head
	}
	return head + "\n" + strings.TrimSpace(*description)
}
