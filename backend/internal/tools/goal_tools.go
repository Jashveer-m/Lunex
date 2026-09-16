package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/jashveer/lifeos/backend/internal/goals"
	"github.com/jashveer/lifeos/backend/internal/httpx"
	"github.com/jashveer/lifeos/backend/internal/validate"
)

type goalRecord struct {
	ID       string  `json:"id"`
	Title    string  `json:"title"`
	Type     string  `json:"type"`
	Status   string  `json:"status"`
	Deadline *string `json:"deadline"`
}

func toGoalRecord(g goals.Goal) goalRecord {
	r := goalRecord{ID: g.ID.String(), Title: g.Title, Type: g.Type, Status: g.Status}
	if g.Deadline != nil {
		d := g.Deadline.UTC().Format(httpx.TimeFormat)
		r.Deadline = &d
	}
	return r
}

var goalRecordSchema = object(map[string]Schema{
	"id":       uuidField("the goal's id"),
	"title":    str("the goal's title"),
	"type":     {Type: "string", Enum: goals.Types},
	"status":   {Type: "string", Enum: goals.Statuses},
	"deadline": timestamp("when it should be reached, or null"),
}, "id", "title", "type", "status", "deadline")

// --- search_goals -------------------------------------------------------------

type searchGoalsInput struct {
	Query  string `json:"query,omitempty"`
	Status string `json:"status,omitempty"`
}

func searchGoalsTool(s Services) Tool {
	return define(Tool{
		Name:        SearchGoals,
		Description: "Find the user's goals whose title or description contains a word or phrase, optionally only those with one status.",
		Permission:  Read,
		Params: []Param{
			{Name: "query", Type: "string", Description: "the word or phrase to look for"},
			{Name: "status", Type: "string", Description: "only goals with this status, if the user asked for one", Enum: goals.Statuses,
				Filter: true, Synonyms: goalStatusSynonyms},
		},
		Output: object(map[string]Schema{
			"count": integer("how many goals are listed"),
			"more":  boolean("whether more goals matched than are listed"),
			"goals": listOf(goalRecordSchema),
		}, "count", "more", "goals"),
	},
		func(_ context.Context, _ uuid.UUID, a Args) (searchGoalsInput, error) {
			in := searchGoalsInput{Query: a.String("query", "q", "search", "text", "keyword")}
			if raw := a.String("status"); raw != "" {
				if in.Status = normalizeChoice(raw, goals.Statuses, goalStatusSynonyms); in.Status == "" {
					return in, invalid(SearchGoals, "status must be one of %s", strings.Join(goals.Statuses, ", "))
				}
			}
			if e := validate.MaxLen("query", in.Query, validate.MaxQueryLen); e != nil {
				return in, invalid(SearchGoals, "the query is too long")
			}
			return in, nil
		},
		func(ctx context.Context, userID uuid.UUID, in searchGoalsInput) (Result, error) {
			sort := "-updated_at"
			if in.Status == "active" {
				sort = "deadline"
			}
			found, more, err := search(in.Query, func(term string, limit int) ([]goals.Goal, error) {
				return s.Goals.List(ctx, userID, goals.Filter{Query: term, Status: in.Status, Sort: sort, Limit: limit})
			}, func(g goals.Goal) uuid.UUID { return g.ID })
			if err != nil {
				return Result{}, fmt.Errorf("search goals: %w", err)
			}
			records := make([]goalRecord, 0, len(found))
			for _, g := range found {
				records = append(records, toGoalRecord(g))
			}
			return Result{
				Output: map[string]any{"count": len(records), "more": more, "goals": records},
				Goals:  found, More: more,
			}, nil
		},
		func(in searchGoalsInput) string {
			return "Search goals" + searchPhrase(in.Query, in.Status) + "."
		},
	)
}

// --- create_goal --------------------------------------------------------------

type createGoalInput struct {
	Title       string `json:"title"`
	Type        string `json:"type"`
	Description string `json:"description,omitempty"`
	Deadline    string `json:"deadline,omitempty"`
}

// DefaultGoalType is what a proposed goal is filed under when the user did not
// say. goals.Type has no column default -- the API requires one -- but asking a
// user "what *type* of goal is running a marathon?" before proposing it is a
// worse experience than filing it under the broadest type and showing that in
// the proposal, where the user sees it before anything is written.
const DefaultGoalType = "personal"

func createGoalTool(s Services) Tool {
	return define(Tool{
		Name:        CreateGoal,
		Description: "Propose a new goal. It is only created after the user approves it.",
		Permission:  Write,
		Params: []Param{
			{Name: "title", Type: "string", Description: "the goal", Required: true},
			{Name: "type", Type: "string", Description: "what kind of goal it is", Enum: goals.Types},
			{Name: "deadline", Type: "string", Description: "when it should be reached: YYYY-MM-DD, or the user's own words"},
			{Name: "description", Type: "string", Description: "more detail, if the user gave any"},
		},
		Output: object(map[string]Schema{"goal": goalRecordSchema}, "goal"),
	},
		func(_ context.Context, _ uuid.UUID, a Args) (createGoalInput, error) {
			in := createGoalInput{
				Title:       a.String("title", "name", "goal"),
				Description: a.String("description", "details"),
				Type:        normalizeChoice(a.String("type", "kind", "category"), goals.Types, goalTypeSynonyms),
			}
			if in.Title == "" {
				return in, invalid(CreateGoal, "a goal needs a title: ask the user what the goal is")
			}
			if in.Type == "" {
				in.Type = DefaultGoalType
			}
			deadline, err := deadlineArg(CreateGoal, a, s.Now())
			if err != nil {
				return in, err
			}
			in.Deadline = deadline
			v, err := goals.ValidateCreate(in.toService())
			if err != nil {
				return in, fieldProblems(CreateGoal, err)
			}
			in.Title, in.Description = v.Title, v.Description
			return in, nil
		},
		func(ctx context.Context, userID uuid.UUID, in createGoalInput) (Result, error) {
			g, err := s.Goals.Create(ctx, userID, in.toService())
			if err != nil {
				return Result{}, err
			}
			return Result{Output: map[string]any{"goal": toGoalRecord(g)}, Goals: []goals.Goal{g}}, nil
		},
		func(in createGoalInput) string {
			return "Create a goal " + quoted(in.Title) + details("type", in.Type, "deadline", dayOf(in.Deadline)) + "."
		},
	)
}

func (in createGoalInput) toService() goals.CreateInput {
	return goals.CreateInput{
		Title: in.Title, Type: in.Type, Description: in.Description, Deadline: parseStored(in.Deadline),
	}
}
