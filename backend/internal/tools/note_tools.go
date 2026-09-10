package tools

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/jashveer/lifeos/backend/internal/notes"
	"github.com/jashveer/lifeos/backend/internal/validate"
)

type noteRecord struct {
	ID    string   `json:"id"`
	Title string   `json:"title"`
	Tags  []string `json:"tags"`
}

func toNoteRecord(n notes.Note) noteRecord {
	tags := n.Tags
	if tags == nil {
		tags = []string{}
	}
	return noteRecord{ID: n.ID.String(), Title: n.Title, Tags: tags}
}

var noteRecordSchema = object(map[string]Schema{
	"id":    uuidField("the note's id"),
	"title": str("the note's title"),
	"tags":  listOf(Schema{Type: "string"}),
}, "id", "title", "tags")

// --- search_notes -------------------------------------------------------------

type searchNotesInput struct {
	Query string `json:"query,omitempty"`
	Tag   string `json:"tag,omitempty"`
}

func searchNotesTool(s Services) Tool {
	return define(Tool{
		Name:        SearchNotes,
		Description: "Find the user's notes whose title or text contains a word or phrase, optionally only those with one tag.",
		Permission:  Read,
		Params: []Param{
			{Name: "query", Type: "string", Description: "the word or phrase to look for"},
			{Name: "tag", Type: "string", Description: "only notes with this tag"},
		},
		Output: object(map[string]Schema{
			"count": integer("how many notes are listed"),
			"more":  boolean("whether more notes matched than are listed"),
			"notes": listOf(noteRecordSchema),
		}, "count", "more", "notes"),
	},
		func(_ context.Context, _ uuid.UUID, a Args) (searchNotesInput, error) {
			in := searchNotesInput{Query: a.String("query", "q", "search", "text", "keyword"), Tag: a.String("tag")}
			if e := validate.MaxLen("query", in.Query, validate.MaxQueryLen); e != nil {
				return in, invalid(SearchNotes, "the query is too long")
			}
			if e := validate.MaxLen("tag", in.Tag, validate.MaxTagLen); e != nil {
				return in, invalid(SearchNotes, "the tag is too long")
			}
			return in, nil
		},
		func(ctx context.Context, userID uuid.UUID, in searchNotesInput) (Result, error) {
			found, more, err := search(in.Query, func(term string, limit int) ([]notes.Note, error) {
				return s.Notes.List(ctx, userID, notes.Filter{Query: term, Tag: in.Tag, Sort: "-updated_at", Limit: limit})
			}, func(n notes.Note) uuid.UUID { return n.ID })
			if err != nil {
				return Result{}, fmt.Errorf("search notes: %w", err)
			}
			records := make([]noteRecord, 0, len(found))
			for _, n := range found {
				records = append(records, toNoteRecord(n))
			}
			return Result{
				Output: map[string]any{"count": len(records), "more": more, "notes": records},
				Notes:  found, More: more,
			}, nil
		},
		func(in searchNotesInput) string {
			out := "Search notes"
			if in.Query != "" {
				out += " for " + quoted(in.Query)
			}
			if in.Tag != "" {
				out += " tagged " + quoted(in.Tag)
			}
			return out + "."
		},
	)
}

// --- create_note --------------------------------------------------------------

type createNoteInput struct {
	Title   string   `json:"title"`
	Content string   `json:"content,omitempty"`
	Tags    []string `json:"tags,omitempty"`
}

// maxDerivedTitle is how much of a note's text becomes its title when the
// model gave it none -- the same length a conversation's derived title is cut to.
const maxDerivedTitle = 60

func createNoteTool(s Services) Tool {
	return define(Tool{
		Name:        CreateNote,
		Description: "Propose a new note. It is only created after the user approves it.",
		Permission:  Write,
		Params: []Param{
			{Name: "title", Type: "string", Description: "a short title", Required: true},
			{Name: "content", Type: "string", Description: "the text of the note, in the user's words"},
			{Name: "tags", Type: "array", Description: "tags, if the user named any"},
		},
		Output: object(map[string]Schema{"note": noteRecordSchema}, "note"),
	},
		func(_ context.Context, _ uuid.UUID, a Args) (createNoteInput, error) {
			in := createNoteInput{
				Title:   a.String("title", "name", "subject"),
				Content: a.String("content", "text", "body", "note"),
				Tags:    a.Strings("tags", "tag"),
			}
			// A model asked to save "the wifi password is hunter2" often puts
			// all of it in the content and nothing in the title. The first line
			// of the user's own text is a better title than a refusal -- and it
			// is theirs, not invented.
			if in.Title == "" {
				in.Title = firstLine(in.Content, maxDerivedTitle)
			}
			if in.Title == "" {
				return in, invalid(CreateNote, "a note needs a title or some text: ask the user what to write down")
			}
			v, err := notes.ValidateCreate(notes.CreateInput{Title: in.Title, Content: in.Content, Tags: in.Tags})
			if err != nil {
				return in, fieldProblems(CreateNote, err)
			}
			in.Title, in.Tags = v.Title, v.Tags
			return in, nil
		},
		func(ctx context.Context, userID uuid.UUID, in createNoteInput) (Result, error) {
			n, err := s.Notes.Create(ctx, userID, notes.CreateInput{Title: in.Title, Content: in.Content, Tags: in.Tags})
			if err != nil {
				return Result{}, err
			}
			return Result{Output: map[string]any{"note": toNoteRecord(n)}, Notes: []notes.Note{n}}, nil
		},
		func(in createNoteInput) string {
			out := "Create a note " + quoted(in.Title)
			if in.Content != "" && in.Content != in.Title {
				out += " saying " + quoted(in.Content)
			}
			return out + details("tags", strings.Join(in.Tags, ", ")) + "."
		},
	)
}

// firstLine is the first line of s, collapsed to single spaces and cut to max
// characters on a word boundary where there is one.
func firstLine(s string, max int) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	cut := string([]rune(s)[:max])
	if i := strings.LastIndex(cut, " "); i > max/2 {
		cut = cut[:i]
	}
	return strings.TrimRight(cut, " ,;:") + "…"
}
