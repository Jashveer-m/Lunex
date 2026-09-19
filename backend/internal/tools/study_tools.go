package tools

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/jashveer/lifeos/backend/internal/documents"
	"github.com/jashveer/lifeos/backend/internal/study"
)

// studyPlanRecord is a study plan as a tool reports it.
type studyPlanRecord struct {
	ID        string  `json:"id"`
	Title     string  `json:"title"`
	Status    string  `json:"status"`
	Document  *string `json:"document"`
	CardCount int     `json:"card_count"`
}

func toStudyPlanRecord(p study.Plan) studyPlanRecord {
	return studyPlanRecord{
		ID: p.ID.String(), Title: p.Title, Status: p.Status,
		Document: p.DocumentName, CardCount: p.CardCount,
	}
}

var studyPlanRecordSchema = object(map[string]Schema{
	"id":         uuidField("the plan's id"),
	"title":      str("what the plan is called"),
	"status":     str("active, completed or abandoned"),
	"document":   str("the document it is built from, or null"),
	"card_count": integer("how many flashcards are filed under it"),
}, "id", "title", "status", "document", "card_count")

var flashcardRecordSchema = object(map[string]Schema{
	"id":    uuidField("the card's id"),
	"front": str("the question"),
	"back":  str("the answer, as the document states it"),
}, "id", "front", "back")

// planStatusSynonyms are the words a user says for a plan's status. They are
// the `status` filter's Synonyms, which is what lets "which study plans have I
// finished" ground `completed` -- a word the message does not contain.
var planStatusSynonyms = map[string]string{
	"done": study.StatusCompleted, "complete": study.StatusCompleted,
	"finished": study.StatusCompleted, "over": study.StatusCompleted,
	"current": study.StatusActive, "ongoing": study.StatusActive, "open": study.StatusActive,
	"in_progress": study.StatusActive, "live": study.StatusActive,
	"dropped": study.StatusAbandoned, "abandoned": study.StatusAbandoned,
	"cancelled": study.StatusAbandoned, "canceled": study.StatusAbandoned,
	"given_up": study.StatusAbandoned, "quit": study.StatusAbandoned,
}

// The argument keys a model writes for a document and for a plan, besides the
// declared names. They are on the Param rather than only read in prepare for
// the reason Param.Aliases gives.
var (
	documentAliases  = []string{"file", "filename", "doc", "source", "document_name", "from"}
	studyPlanAliases = []string{"plan", "plan_name", "study_plan_title", "deck", "under"}
)

// --- search_study_plans ------------------------------------------------------

type searchStudyPlansInput struct {
	Query  string `json:"query,omitempty"`
	Status string `json:"status,omitempty"`
}

func searchStudyPlansTool(s Services) Tool {
	return define(Tool{
		Name: SearchStudyPlans,
		Description: "Look at the user's study plans: what they are studying, what each plan is built from, " +
			"and how many flashcards it has.",
		Permission: Read,
		Params: []Param{
			{Name: "query", Type: "string", Description: "a word or phrase from the plan's title or description, if the user gave one"},
			{Name: "status", Type: "string", Enum: study.Statuses, Filter: true,
				Synonyms:    planStatusSynonyms,
				Description: "only plans in this state, if the user said which"},
		},
		Output: object(map[string]Schema{
			"count":       integer("how many plans are listed"),
			"more":        boolean("whether more plans matched than are listed"),
			"study_plans": listOf(studyPlanRecordSchema),
		}, "count", "more", "study_plans"),
	},
		func(_ context.Context, _ uuid.UUID, a Args) (searchStudyPlansInput, error) {
			in := searchStudyPlansInput{
				Query: a.String("query", "q", "search", "text", "keyword", "topic", "subject"),
			}
			if raw := a.String("status", "state"); raw != "" {
				// An unrecognised status is dropped rather than refused: the
				// plans are still worth listing, and a search that came back
				// empty because the model wrote "studying" would be a lie
				// about what the user has. It is the opposite call from
				// create_expense's category, and for the reason
				// search_expenses gives -- a read is recoverable by reading
				// again.
				in.Status = normalizeChoice(raw, study.Statuses, planStatusSynonyms)
			}
			return in, nil
		},
		func(ctx context.Context, userID uuid.UUID, in searchStudyPlansInput) (Result, error) {
			found, more, err := search(in.Query, func(term string, limit int) ([]study.Plan, error) {
				return s.Study.Plans(ctx, userID, study.Filter{
					Query: term, Status: in.Status, Sort: study.DefaultSort, Limit: limit,
				})
			}, func(p study.Plan) uuid.UUID { return p.ID })
			if err != nil {
				return Result{}, fmt.Errorf("search study plans: %w", err)
			}
			records := make([]studyPlanRecord, 0, len(found))
			for _, p := range found {
				records = append(records, toStudyPlanRecord(p))
			}
			return Result{
				Output: map[string]any{"count": len(records), "more": more, "study_plans": records},
				Plans:  found, More: more,
			}, nil
		},
		func(in searchStudyPlansInput) string {
			out := "Look at the study plans"
			if in.Status != "" {
				out += " that are " + in.Status
			}
			if in.Query != "" {
				out += " matching " + quoted(in.Query)
			}
			return out + "."
		},
	)
}

// --- create_study_plan --------------------------------------------------------

// createStudyPlanInput is the canonical proposal.
//
// The document is carried as both an id and a filename: the id is what runs,
// and the filename is what the user reads on the approval card. It is the
// shape update_task uses for the task it changes, and for the same reason --
// a uuid in a proposal tells the person approving it nothing.
type createStudyPlanInput struct {
	Title       string     `json:"title"`
	Description string     `json:"description,omitempty"`
	DocumentID  *uuid.UUID `json:"document_id,omitempty"`
	Document    string     `json:"document,omitempty"`
}

func createStudyPlanTool(s Services) Tool {
	return define(Tool{
		Name: CreateStudyPlan,
		Description: "Propose a study plan: something the user says they are going to study. " +
			"It is only created after the user approves it.",
		Permission: Write,
		Params: []Param{
			{Name: "title", Type: "string", Required: true,
				Description: `what the plan is for, in the user's own words ("Linear algebra finals")`},
			{Name: "description", Type: "string", Description: "any detail the user gave about what the plan covers"},
			{Name: "document", Type: "string", Aliases: documentAliases,
				Description: "the uploaded document the plan is built from, by filename, if the user named one"},
			{Name: "document_id", Type: "string", Description: "the document's id, if it is known exactly"},
		},
		Output: object(map[string]Schema{"study_plan": studyPlanRecordSchema}, "study_plan"),
	},
		func(ctx context.Context, userID uuid.UUID, a Args) (createStudyPlanInput, error) {
			in := createStudyPlanInput{
				Title:       a.String("title", "name", "plan", "subject", "topic"),
				Description: a.String("description", "details", "notes", "about", "covers"),
			}
			if in.Title == "" {
				return in, invalid(CreateStudyPlan, "a study plan needs a title: ask the user what they are studying")
			}
			ref := a.String(append([]string{"document", "document_id"}, documentAliases...)...)
			if ref != "" {
				doc, err := resolveDocument(ctx, s, userID, CreateStudyPlan, ref)
				if err != nil {
					return in, err
				}
				in.DocumentID, in.Document = &doc.ID, doc.Filename
			}
			// The service's own validation, run now so the user is never shown
			// a proposal that would fail on approval.
			v, err := study.ValidateCreatePlan(in.toService())
			if err != nil {
				return in, fieldProblems(CreateStudyPlan, err)
			}
			in.Title, in.Description = v.Title, v.Description
			return in, nil
		},
		func(ctx context.Context, userID uuid.UUID, in createStudyPlanInput) (Result, error) {
			p, err := s.Study.CreatePlan(ctx, userID, in.toService())
			if err != nil {
				return Result{}, err
			}
			return Result{
				Output: map[string]any{"study_plan": toStudyPlanRecord(p)},
				Plans:  []study.Plan{p},
			}, nil
		},
		func(in createStudyPlanInput) string {
			out := "Create a study plan " + quoted(in.Title)
			if in.Document != "" {
				out += " from " + quoted(in.Document)
			}
			return out + details("covering", in.Description) + "."
		},
	)
}

func (in createStudyPlanInput) toService() study.CreatePlanInput {
	return study.CreatePlanInput{
		Title: in.Title, Description: in.Description, DocumentID: in.DocumentID,
	}
}

// --- generate_flashcards -------------------------------------------------------

// flashcardPair is one proposed card in the canonical input.
type flashcardPair struct {
	Front string `json:"front"`
	Back  string `json:"back"`
}

// generateFlashcardsInput is the canonical proposal, and the cards are *in*
// it.
//
// That is the design decision this tool is built around. The model writes the
// cards while the call is being prepared, they are stored in the input, and
// approving the action writes exactly those rows. The alternative -- store
// the document and the count, generate on approval -- would mean the user
// approved "8 flashcards from lecture-3.pdf" and got eight sentences nobody
// had read, which for content whose whole purpose is to be rehearsed until it
// is believed is the wrong way round. It also makes the proposal reproducible:
// the same input always writes the same cards, so an approval is not a second
// roll of the dice.
//
// The cost is that a proposal left pending gets stale if the document changes.
// It cannot: a document cannot be edited in this version, only deleted -- and
// a deleted one takes document_id to NULL, which is checked when the approval
// runs.
type generateFlashcardsInput struct {
	DocumentID  uuid.UUID       `json:"document_id"`
	Document    string          `json:"document"`
	StudyPlanID *uuid.UUID      `json:"study_plan_id,omitempty"`
	StudyPlan   string          `json:"study_plan,omitempty"`
	Topic       string          `json:"topic,omitempty"`
	Cards       []flashcardPair `json:"cards"`
}

func generateFlashcardsTool(s Services) Tool {
	return define(Tool{
		Name: GenerateFlashcards,
		Description: "Propose flashcards made from one of the user's uploaded documents. " +
			"The questions and answers are written from the document's own text and shown to the user; " +
			"they are only saved after the user approves them.",
		Permission: Write,
		Params: []Param{
			{Name: "document", Type: "string", Required: true, Aliases: documentAliases,
				Description: "the uploaded document to make the cards from, by filename"},
			{Name: "document_id", Type: "string", Description: "the document's id, if it is known exactly"},
			{Name: "study_plan", Type: "string", Aliases: studyPlanAliases,
				Description: "the study plan to file the cards under, by title, if the user named one"},
			{Name: "study_plan_id", Type: "string", Description: "the study plan's id, if it is known exactly"},
			{Name: "topic", Type: "string", Aliases: []string{"about", "subject", "chapter", "section"},
				Description: "the part of the document to cover, if the user named one"},
			{Name: "count", Type: "string", Aliases: []string{"how_many", "number", "cards"},
				Description: "how many cards the user asked for, if they said a number"},
			// Not offered to the model: written by the tool from the document.
			// See Param.Derived and the type comment above.
			{Name: "cards", Type: "array", Derived: true,
				Description: "the generated cards, written from the document while this call was prepared"},
		},
		Output: object(map[string]Schema{
			"document":   str("the document the cards were made from"),
			"study_plan": str("the plan they were filed under, or null"),
			"count":      integer("how many cards were created"),
			"flashcards": listOf(flashcardRecordSchema),
		}, "document", "study_plan", "count", "flashcards"),
	},
		func(ctx context.Context, userID uuid.UUID, a Args) (generateFlashcardsInput, error) {
			var in generateFlashcardsInput
			ref := a.String(append([]string{"document", "document_id"}, documentAliases...)...)
			if ref == "" {
				return in, invalid(GenerateFlashcards,
					"say which uploaded document to make the cards from")
			}
			doc, err := resolveDocument(ctx, s, userID, GenerateFlashcards, ref)
			if err != nil {
				return in, err
			}
			in.DocumentID, in.Document = doc.ID, doc.Filename

			gen := study.GenerateInput{
				DocumentID: doc.ID,
				Topic:      a.String("topic", "about", "subject", "chapter", "section"),
				Count:      cardCount(a),
			}
			if planRef := a.String(append([]string{"study_plan", "study_plan_id"}, studyPlanAliases...)...); planRef != "" {
				plan, err := resolveStudyPlan(ctx, s, userID, planRef)
				if err != nil {
					return in, err
				}
				gen.StudyPlanID, in.StudyPlanID, in.StudyPlan = &plan.ID, &plan.ID, plan.Title
			}
			in.Topic = gen.Topic

			// The model call. It happens here, during Prepare, which is
			// allowed to read and never writes: nothing is stored by this, and
			// what comes back is a proposal the user has still to approve.
			proposal, err := s.Study.ProposeFlashcards(ctx, userID, gen)
			switch {
			case errors.Is(err, study.ErrNotStudyable):
				return in, invalid(GenerateFlashcards,
					"%s has no text that can be used yet: tell the user to check it finished processing",
					quoted(doc.Filename))
			case errors.Is(err, study.ErrNoPassages):
				return in, invalid(GenerateFlashcards,
					"nothing in %s is about %s: ask the user which part of it they mean",
					quoted(doc.Filename), quoted(gen.Topic))
			case errors.Is(err, study.ErrGeneration):
				// The model wrote nothing usable, or nothing it wrote was
				// actually in the document. Either way there is no proposal to
				// show, and saying so is better than proposing cards that are
				// not about the document -- which is the failure the whole
				// grounding check exists to prevent.
				return in, invalid(GenerateFlashcards,
					"could not write flashcards from %s that its own text supports: "+
						"tell the user the document does not have enough on this to make cards from",
					quoted(doc.Filename))
			case err != nil:
				return in, fieldProblems(GenerateFlashcards, err)
			}
			for _, c := range proposal.Cards {
				in.Cards = append(in.Cards, flashcardPair{Front: c.Front, Back: c.Back})
			}
			return in, nil
		},
		func(ctx context.Context, userID uuid.UUID, in generateFlashcardsInput) (Result, error) {
			cards := make([]study.CreateCardInput, 0, len(in.Cards))
			for _, c := range in.Cards {
				docID := in.DocumentID
				cards = append(cards, study.CreateCardInput{
					StudyPlanID: in.StudyPlanID, DocumentID: &docID,
					Front: c.Front, Back: c.Back,
				})
			}
			created, err := s.Study.CreateFlashcards(ctx, userID, cards)
			if err != nil {
				// The document or the plan existed when this was proposed. If
				// one does not now, the approved write fails and is recorded
				// as having failed, rather than quietly filing cards under
				// nothing the user chose.
				return Result{}, fmt.Errorf("save flashcards from %q: %w", in.Document, err)
			}
			records := make([]map[string]any, 0, len(created))
			for _, c := range created {
				records = append(records, map[string]any{
					"id": c.ID.String(), "front": c.Front, "back": c.Back,
				})
			}
			var plan any
			if in.StudyPlan != "" {
				plan = in.StudyPlan
			}
			return Result{Output: map[string]any{
				"document": in.Document, "study_plan": plan,
				"count": len(records), "flashcards": records,
			}}, nil
		},
		func(in generateFlashcardsInput) string {
			// The cards themselves, not "create 8 flashcards". The whole point
			// of generating them before the approval is that the user reads
			// the questions and answers they are approving; a summary that
			// only counted them would throw that away.
			var b strings.Builder
			fmt.Fprintf(&b, "Save %s made from %s", plural(len(in.Cards), "flashcard"), quoted(in.Document))
			if in.Topic != "" {
				b.WriteString(" about " + quoted(in.Topic))
			}
			if in.StudyPlan != "" {
				b.WriteString(" under " + quoted(in.StudyPlan))
			}
			b.WriteString(":")
			for i, c := range in.Cards {
				fmt.Fprintf(&b, "\n%d. %s — %s", i+1, c.Front, c.Back)
			}
			// Every summary in the registry ends in a full stop, and a card's
			// answer usually ends in one already -- so this adds the stop only
			// when the last line does not have it, rather than writing
			// "58.4 volts..".
			return endSentence(b.String())
		},
	)
}

// --- shared helpers -------------------------------------------------------------

// endSentence gives a summary the full stop every one of them ends with,
// without doubling one that is already there.
func endSentence(s string) string {
	if strings.HasSuffix(s, ".") || strings.HasSuffix(s, "?") || strings.HasSuffix(s, "!") {
		return s
	}
	return s + "."
}

// cardCount reads how many cards the user asked for. Anything unreadable is
// "they did not say", which study.ValidateGenerate turns into the default --
// there is no sense in refusing a whole generation because a model wrote "a
// few".
func cardCount(a Args) int {
	raw := a.String("count", "how_many", "number", "cards", "n")
	if raw == "" {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// resolveDocument finds the one document a model's reference means, among the
// caller's own documents only.
//
// The lookup is study.Service.ResolveDocument, which matches an id, then an
// exact filename, then a unique substring. Several matches are an
// ArgumentError listing them: guessing which lecture to build a deck from is
// exactly the decision the user should be making, and a deck from the wrong
// file is not something they will catch at the approval card -- the cards will
// look perfectly well made.
func resolveDocument(ctx context.Context, s Services, userID uuid.UUID, tool, ref string) (documents.Document, error) {
	doc, err := s.Study.ResolveDocument(ctx, userID, ref)
	var ambiguous *study.AmbiguousDocumentError
	switch {
	case errors.As(err, &ambiguous):
		return documents.Document{}, invalid(tool,
			"%s matches more than one of the user's documents (%s): ask which one they mean",
			quoted(ref), strings.Join(ambiguous.Matches, ", "))
	case errors.Is(err, study.ErrNotFound):
		return documents.Document{}, invalid(tool,
			"the user has no uploaded document called %s: ask which document they mean, or tell them to upload it",
			quoted(ref))
	case err != nil:
		return documents.Document{}, fmt.Errorf("resolve document %q: %w", ref, err)
	}
	return doc, nil
}

// planRefStopWords are dropped from a plan reference before it is searched
// for: "my linear algebra plan" names the plan "linear algebra", not a plan
// containing the word "plan".
var planRefStopWords = map[string]struct{}{
	"the": {}, "my": {}, "a": {}, "an": {}, "plan": {}, "plans": {}, "deck": {},
	"study": {}, "studying": {}, "revision": {}, "one": {}, "called": {}, "named": {}, "for": {},
}

// resolveStudyPlan finds the one plan a model's reference means, among the
// caller's own plans only. Same rule as resolveTask: an id, then an exact
// title, then a unique text match, and several matches are an error listing
// them.
func resolveStudyPlan(ctx context.Context, s Services, userID uuid.UUID, ref string) (study.Plan, error) {
	if id, err := uuid.Parse(ref); err == nil {
		found, err := s.Study.Plans(ctx, userID, study.Filter{Limit: study.MaxLimit})
		if err != nil {
			return study.Plan{}, fmt.Errorf("resolve study plan: %w", err)
		}
		for _, p := range found {
			if p.ID == id {
				return p, nil
			}
		}
		// Another user's plan id resolves to nothing, the same "no such plan"
		// as one that does not exist.
		return study.Plan{}, invalid(GenerateFlashcards, "there is no such study plan")
	}
	ref = strings.TrimSpace(strings.Trim(ref, `"'`))
	if ref == "" {
		return study.Plan{}, invalid(GenerateFlashcards, "say which study plan to file the cards under")
	}

	var words []string
	for _, w := range strings.Fields(ref) {
		if _, stop := planRefStopWords[strings.ToLower(w)]; !stop {
			words = append(words, w)
		}
	}
	core := strings.Join(words, " ")
	if core == "" {
		core = ref
	}

	terms := []string{ref}
	if core != ref {
		terms = append(terms, core)
	}
	var candidates []study.Plan
	seen := map[uuid.UUID]struct{}{}
	for _, term := range terms {
		found, err := s.Study.Plans(ctx, userID, study.Filter{Query: term, Limit: study.MaxLimit})
		if err != nil {
			return study.Plan{}, fmt.Errorf("resolve study plan: %w", err)
		}
		for _, p := range found {
			if strings.EqualFold(p.Title, ref) || strings.EqualFold(p.Title, core) {
				return p, nil
			}
			if _, dup := seen[p.ID]; dup {
				continue
			}
			seen[p.ID] = struct{}{}
			candidates = append(candidates, p)
		}
	}
	switch len(candidates) {
	case 1:
		return candidates[0], nil
	case 0:
		return study.Plan{}, invalid(GenerateFlashcards,
			"the user has no study plan called %s: ask which plan they mean, or propose creating it first",
			quoted(ref))
	}
	titles := make([]string, 0, len(candidates))
	for _, p := range candidates {
		titles = append(titles, p.Title)
	}
	return study.Plan{}, invalid(GenerateFlashcards,
		"%s matches more than one study plan (%s): ask which one they mean",
		quoted(ref), strings.Join(titles, ", "))
}
