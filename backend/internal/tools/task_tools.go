package tools

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/jashveer/lifeos/backend/internal/httpx"
	"github.com/jashveer/lifeos/backend/internal/optional"
	"github.com/jashveer/lifeos/backend/internal/tasks"
	"github.com/jashveer/lifeos/backend/internal/validate"
)

// taskRecord is a task as a tool reports it. It is deliberately smaller than
// the API's task response: it is recorded as the action's result, and an audit
// trail of what the assistant looked at does not need every column.
type taskRecord struct {
	ID       string  `json:"id"`
	Title    string  `json:"title"`
	Status   string  `json:"status"`
	Priority string  `json:"priority"`
	Deadline *string `json:"deadline"`
}

func toTaskRecord(t tasks.Task) taskRecord {
	r := taskRecord{ID: t.ID.String(), Title: t.Title, Status: t.Status, Priority: t.Priority}
	if t.Deadline != nil {
		d := t.Deadline.UTC().Format(httpx.TimeFormat)
		r.Deadline = &d
	}
	return r
}

var taskRecordSchema = object(map[string]Schema{
	"id":       uuidField("the task's id"),
	"title":    str("the task's title"),
	"status":   {Type: "string", Enum: tasks.Statuses},
	"priority": {Type: "string", Enum: tasks.Priorities},
	"deadline": timestamp("when it is due, or null"),
}, "id", "title", "status", "priority", "deadline")

// --- search_tasks -------------------------------------------------------------

type searchTasksInput struct {
	Query  string `json:"query,omitempty"`
	Status string `json:"status,omitempty"`
}

func searchTasksTool(s Services) Tool {
	return define(Tool{
		Name:        SearchTasks,
		Description: "Find the user's tasks whose title or description contains a word or phrase, optionally only those with one status.",
		Permission:  Read,
		Params: []Param{
			{Name: "query", Type: "string", Description: "the word or phrase to look for"},
			{Name: "status", Type: "string", Description: "only tasks with this status, if the user asked for one", Enum: tasks.Statuses,
				Filter: true, Synonyms: taskStatusSynonyms},
		},
		Output: object(map[string]Schema{
			"count": integer("how many tasks are listed"),
			"more":  boolean("whether more tasks matched than are listed"),
			"tasks": listOf(taskRecordSchema),
		}, "count", "more", "tasks"),
	},
		func(_ context.Context, _ uuid.UUID, a Args) (searchTasksInput, error) {
			in := searchTasksInput{Query: a.String("query", "q", "search", "text", "keyword")}
			if raw := a.String("status"); raw != "" {
				if in.Status = normalizeChoice(raw, tasks.Statuses, taskStatusSynonyms); in.Status == "" {
					return in, invalid(SearchTasks, "status must be one of %s", strings.Join(tasks.Statuses, ", "))
				}
			}
			if e := validate.MaxLen("query", in.Query, validate.MaxQueryLen); e != nil {
				return in, invalid(SearchTasks, "the query is too long")
			}
			return in, nil
		},
		func(ctx context.Context, userID uuid.UUID, in searchTasksInput) (Result, error) {
			// Open work is most usefully listed by what is due first; anything
			// else by what was touched last.
			sort := "-updated_at"
			if in.Status == "pending" || in.Status == "in_progress" {
				sort = "deadline"
			}
			found, more, err := search(in.Query, func(term string, limit int) ([]tasks.Task, error) {
				return s.Tasks.List(ctx, userID, tasks.Filter{Query: term, Status: in.Status, Sort: sort, Limit: limit})
			}, func(t tasks.Task) uuid.UUID { return t.ID })
			if err != nil {
				return Result{}, fmt.Errorf("search tasks: %w", err)
			}
			records := make([]taskRecord, 0, len(found))
			for _, t := range found {
				records = append(records, toTaskRecord(t))
			}
			return Result{
				Output: map[string]any{"count": len(records), "more": more, "tasks": records},
				Tasks:  found, More: more,
			}, nil
		},
		func(in searchTasksInput) string {
			return "Search tasks" + searchPhrase(in.Query, in.Status) + "."
		},
	)
}

// searchPhrase is the tail of a search summary: `for "antenna" with status
// pending`.
func searchPhrase(query, status string) string {
	out := ""
	if query != "" {
		out += " for " + quoted(query)
	}
	if status != "" {
		out += " with status " + status
	}
	return out
}

// --- create_task --------------------------------------------------------------

type createTaskInput struct {
	Title       string   `json:"title"`
	Description string   `json:"description,omitempty"`
	Priority    string   `json:"priority,omitempty"`
	Deadline    string   `json:"deadline,omitempty"`
	Category    string   `json:"category,omitempty"`
	Tags        []string `json:"tags,omitempty"`
}

func createTaskTool(s Services) Tool {
	return define(Tool{
		Name:        CreateTask,
		Description: "Propose a new task. It is only created after the user approves it.",
		Permission:  Write,
		Params: []Param{
			{Name: "title", Type: "string", Description: "what the task is", Required: true},
			{Name: "deadline", Type: "string", Description: "when it is due: a date as YYYY-MM-DD, or the user's own words such as \"tomorrow\" or \"friday\""},
			{Name: "priority", Type: "string", Description: "how urgent it is", Enum: tasks.Priorities},
			{Name: "description", Type: "string", Description: "more detail, if the user gave any"},
			{Name: "category", Type: "string", Description: "a category, if the user named one"},
			{Name: "tags", Type: "array", Description: "tags, if the user named any"},
		},
		Output: object(map[string]Schema{"task": taskRecordSchema}, "task"),
	},
		func(_ context.Context, _ uuid.UUID, a Args) (createTaskInput, error) {
			in := createTaskInput{
				Title:       a.String("title", "name", "task"),
				Description: a.String("description", "details", "notes"),
				Category:    a.String("category"),
				Tags:        a.Strings("tags", "tag"),
			}
			if in.Title == "" {
				return in, invalid(CreateTask, "a task needs a title: ask the user what the task is")
			}
			if raw := a.String("priority"); raw != "" {
				if in.Priority = normalizeChoice(raw, tasks.Priorities, prioritySynonyms); in.Priority == "" {
					return in, invalid(CreateTask, "priority must be one of %s", strings.Join(tasks.Priorities, ", "))
				}
			}
			deadline, err := deadlineArg(CreateTask, a, s.Now())
			if err != nil {
				return in, err
			}
			in.Deadline = deadline
			// The service's own validation, run now so the user is never shown
			// a proposal that would fail on approval.
			v, err := tasks.ValidateCreate(in.toService())
			if err != nil {
				return in, fieldProblems(CreateTask, err)
			}
			in.Title, in.Description, in.Category, in.Tags = v.Title, v.Description, v.Category, v.Tags
			return in, nil
		},
		func(ctx context.Context, userID uuid.UUID, in createTaskInput) (Result, error) {
			t, err := s.Tasks.Create(ctx, userID, in.toService())
			if err != nil {
				return Result{}, err
			}
			return Result{Output: map[string]any{"task": toTaskRecord(t)}, Tasks: []tasks.Task{t}}, nil
		},
		func(in createTaskInput) string {
			return "Create a task " + quoted(in.Title) + details(
				"priority", in.Priority, "due", dayOf(in.Deadline), "category", in.Category,
				"tags", strings.Join(in.Tags, ", ")) + "."
		},
	)
}

func (in createTaskInput) toService() tasks.CreateInput {
	return tasks.CreateInput{
		Title: in.Title, Description: in.Description, Priority: in.Priority,
		Category: in.Category, Tags: in.Tags, Deadline: parseStored(in.Deadline),
	}
}

// --- update_task --------------------------------------------------------------

// updateTaskInput names the task by id -- resolved from whatever the model
// wrote -- and carries only the fields being changed.
//
// Task is the task's title when the change was proposed. It is there for the
// summary, so the user approves "Update "Write the scheduler"" rather than an
// id, and it is never used to find the task: execution goes by TaskID alone.
type updateTaskInput struct {
	TaskID      uuid.UUID `json:"task_id"`
	Task        string    `json:"task"`
	Title       string    `json:"title,omitempty"`
	Description string    `json:"description,omitempty"`
	Priority    string    `json:"priority,omitempty"`
	Status      string    `json:"status,omitempty"`
	Deadline    string    `json:"deadline,omitempty"`
}

func updateTaskTool(s Services) Tool {
	return define(Tool{
		Name:        UpdateTask,
		Description: "Propose changing one of the user's existing tasks. It is only changed after the user approves it.",
		Permission:  Write,
		Params: []Param{
			{Name: "task", Type: "string", Description: "the task to change, by its title or a word from it", Required: true},
			{Name: "task_id", Type: "string", Description: "the task's id, if it is known exactly"},
			{Name: "status", Type: "string", Description: "the new status", Enum: tasks.Statuses},
			{Name: "priority", Type: "string", Description: "the new priority", Enum: tasks.Priorities},
			{Name: "title", Type: "string", Description: "a new title"},
			{Name: "deadline", Type: "string", Description: "a new due date: YYYY-MM-DD, or the user's own words such as \"friday\""},
			{Name: "description", Type: "string", Description: "a new description"},
		},
		Output: object(map[string]Schema{"task": taskRecordSchema}, "task"),
	},
		func(ctx context.Context, userID uuid.UUID, a Args) (updateTaskInput, error) {
			var in updateTaskInput
			current, err := resolveTask(ctx, s.Tasks, userID, a.String("task_id", "id"), a.String("task", "task_title", "name", "query"))
			if err != nil {
				return in, err
			}
			in.TaskID, in.Task = current.ID, current.Title

			in.Title = a.String("title", "new_title")
			in.Description = a.String("description", "new_description")
			if raw := a.String("status", "new_status"); raw != "" {
				if in.Status = normalizeChoice(raw, tasks.Statuses, taskStatusSynonyms); in.Status == "" {
					return in, invalid(UpdateTask, "status must be one of %s", strings.Join(tasks.Statuses, ", "))
				}
			}
			if raw := a.String("priority", "new_priority"); raw != "" {
				if in.Priority = normalizeChoice(raw, tasks.Priorities, prioritySynonyms); in.Priority == "" {
					return in, invalid(UpdateTask, "priority must be one of %s", strings.Join(tasks.Priorities, ", "))
				}
			}
			if in.Deadline, err = deadlineArg(UpdateTask, a, s.Now(), "new_deadline"); err != nil {
				return in, err
			}

			// A change to what it already is, is no change: say so, rather than
			// asking the user to approve nothing.
			if in.Title == current.Title {
				in.Title = ""
			}
			if in.Status == current.Status {
				in.Status = ""
			}
			if in.Priority == current.Priority {
				in.Priority = ""
			}
			if in.Description != "" && current.Description != nil && in.Description == *current.Description {
				in.Description = ""
			}
			if d := parseStored(in.Deadline); d != nil && current.Deadline != nil && d.Equal(*current.Deadline) {
				in.Deadline = ""
			}
			if in.Title == "" && in.Description == "" && in.Status == "" && in.Priority == "" && in.Deadline == "" {
				return in, invalid(UpdateTask, "nothing to change on the task %s: it already has those values, or no new value was given", quoted(current.Title))
			}

			if _, err := tasks.ValidatePatch(in.toService()); err != nil {
				return in, fieldProblems(UpdateTask, err)
			}
			return in, nil
		},
		func(ctx context.Context, userID uuid.UUID, in updateTaskInput) (Result, error) {
			t, err := s.Tasks.Update(ctx, userID, in.TaskID, in.toService())
			if err != nil {
				return Result{}, err
			}
			return Result{Output: map[string]any{"task": toTaskRecord(t)}, Tasks: []tasks.Task{t}}, nil
		},
		func(in updateTaskInput) string {
			var changes []string
			add := func(field, value string) {
				if value != "" {
					changes = append(changes, field+" → "+value)
				}
			}
			if in.Title != "" {
				add("title", quoted(in.Title))
			}
			add("status", in.Status)
			add("priority", in.Priority)
			add("due", dayOf(in.Deadline))
			if in.Description != "" {
				add("description", quoted(in.Description))
			}
			return "Update the task " + quoted(in.Task) + ": " + strings.Join(changes, ", ") + "."
		},
	)
}

func (in updateTaskInput) toService() tasks.UpdateInput {
	var u tasks.UpdateInput
	if in.Title != "" {
		u.Title = optional.Of(in.Title)
	}
	if in.Description != "" {
		u.Description = optional.Of(in.Description)
	}
	if in.Priority != "" {
		u.Priority = optional.Of(in.Priority)
	}
	if in.Status != "" {
		u.Status = optional.Of(in.Status)
	}
	if d := parseStored(in.Deadline); d != nil {
		u.Deadline = optional.Of(*d)
	}
	return u
}

// taskRefStopWords are dropped from a task reference before it is searched for:
// "the scheduler task" names the task "scheduler", not a task containing the
// word "task".
var taskRefStopWords = map[string]struct{}{
	"the": {}, "my": {}, "a": {}, "an": {}, "task": {}, "tasks": {}, "todo": {}, "item": {},
	"one": {}, "called": {}, "named": {}, "about": {},
}

// resolveTask finds the one task a model's reference means, among the caller's
// own tasks only.
//
// By id if the model gave a real one; otherwise by title. An exact title match
// (case-insensitive) wins outright. Failing that, the reference is searched for
// as text, and it resolves only if exactly one task matches -- several matches
// are an error listing them, because guessing which task to change is exactly
// the decision the user should be making, and the proposal is the wrong place
// to find out the guess was wrong.
//
// Another user's task id resolves to nothing, the same "no such task" as an id
// that does not exist, because the lookup is the owner-scoped service.
func resolveTask(ctx context.Context, svc TaskService, userID uuid.UUID, id, ref string) (tasks.Task, error) {
	if parsed, err := uuid.Parse(id); err == nil {
		t, err := svc.Get(ctx, userID, parsed)
		if errors.Is(err, tasks.ErrNotFound) {
			return tasks.Task{}, invalid(UpdateTask, "there is no such task")
		}
		if err != nil {
			return tasks.Task{}, fmt.Errorf("resolve task: %w", err)
		}
		return t, nil
	}
	if ref == "" {
		// A non-uuid in task_id is a title the model put in the wrong field.
		ref = id
	}
	ref = strings.TrimSpace(strings.Trim(ref, `"'`))
	if ref == "" {
		return tasks.Task{}, invalid(UpdateTask, "say which task to change")
	}

	var words []string
	for _, w := range strings.Fields(ref) {
		if _, stop := taskRefStopWords[strings.ToLower(w)]; !stop {
			words = append(words, w)
		}
	}
	core := strings.Join(words, " ")
	if core == "" {
		core = ref
	}

	const maxCandidates = 20
	var candidates []tasks.Task
	seen := map[uuid.UUID]struct{}{}
	terms := []string{ref}
	if core != ref {
		terms = append(terms, core)
	}
	for _, w := range words {
		if w != core {
			terms = append(terms, w)
		}
	}
	for _, term := range terms {
		found, err := svc.List(ctx, userID, tasks.Filter{Query: term, Limit: maxCandidates})
		if err != nil {
			return tasks.Task{}, fmt.Errorf("resolve task: %w", err)
		}
		for _, t := range found {
			if strings.EqualFold(t.Title, ref) || strings.EqualFold(t.Title, core) {
				return t, nil
			}
			if _, dup := seen[t.ID]; !dup {
				seen[t.ID] = struct{}{}
				candidates = append(candidates, t)
			}
		}
		// The whole reference, or its significant words together, finding
		// anything is a better answer than the looser single-word search.
		if len(candidates) > 0 && (term == ref || term == core) {
			break
		}
	}

	switch len(candidates) {
	case 0:
		return tasks.Task{}, invalid(UpdateTask, "no task matches %s", quoted(ref))
	case 1:
		return candidates[0], nil
	}
	titles := make([]string, 0, 4)
	for i, t := range candidates {
		if i == 4 {
			titles = append(titles, "…")
			break
		}
		titles = append(titles, quoted(t.Title))
	}
	return tasks.Task{}, invalid(UpdateTask, "%d tasks match %s (%s): ask the user which one", len(candidates), quoted(ref), strings.Join(titles, ", "))
}

// --- shared helpers -------------------------------------------------------------

// deadlineArg reads and resolves a deadline, returning it in the canonical
// stored form (RFC 3339, UTC).
func deadlineArg(tool string, a Args, now time.Time, aliases ...string) (string, error) {
	raw := a.String(append([]string{"deadline", "due", "due_date", "date", "by"}, aliases...)...)
	if raw == "" {
		return "", nil
	}
	t, err := ParseDate(raw, now)
	if err != nil {
		return "", invalid(tool, "could not read %s as a date: ask the user for a date such as 2026-09-30", quoted(raw))
	}
	return t.UTC().Format(time.RFC3339), nil
}

// parseStored reads a canonical deadline back. The canonical form was written
// by deadlineArg, so a failure here is a row that was not -- and is treated as
// no deadline rather than a guess.
func parseStored(s string) *time.Time {
	if s == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return nil
	}
	return &t
}

// dayOf renders a canonical deadline for a summary.
func dayOf(s string) string {
	if t := parseStored(s); t != nil {
		return formatDay(*t)
	}
	return ""
}

// details renders the non-empty (label, value) pairs as ` (priority high, due
// Fri 11 Sep 2026)`, or nothing at all.
func details(pairs ...string) string {
	var parts []string
	for i := 0; i+1 < len(pairs); i += 2 {
		if pairs[i+1] != "" {
			parts = append(parts, pairs[i]+" "+pairs[i+1])
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return " (" + strings.Join(parts, ", ") + ")"
}

// fieldProblems turns the services' field-level validation errors into an
// argument error the model can relay: "title: must be at most 500 characters".
func fieldProblems(tool string, err error) error {
	var verrs validate.Errors
	if !errors.As(err, &verrs) {
		return err
	}
	parts := make([]string, 0, len(verrs))
	for _, v := range verrs {
		parts = append(parts, v.Field+" "+v.Message)
	}
	return invalid(tool, "%s", strings.Join(parts, "; "))
}
