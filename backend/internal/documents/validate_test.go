package documents

import (
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/jashveer/lifeos/backend/internal/auth"
)

func TestValidateFilename(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string // "" means rejected
	}{
		{"plain", "notes.txt", "notes.txt"},
		{"trimmed", "  notes.txt  ", "notes.txt"},
		// Browsers and some clients send a full path in the multipart header.
		{"unix path", "/home/alice/notes.txt", "notes.txt"},
		{"windows path", `C:\Users\alice\notes.txt`, "notes.txt"},
		{"traversal", "../../../etc/passwd", "passwd"},
		{"traversal windows", `..\..\secret.md`, "secret.md"},
		{"empty", "", ""},
		{"whitespace", "   ", ""},
		{"dot", ".", ""},
		{"slash", "/", ""},
		{"nul byte", "note\x00.txt", ""},
		{"too long", strings.Repeat("a", MaxFilenameLen+1) + ".txt", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ValidateFilename(tc.in)
			if tc.want == "" {
				if err == nil {
					t.Fatalf("ValidateFilename(%q) = %q, want an error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ValidateFilename(%q) = %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("ValidateFilename(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestValidateSearchDefaults(t *testing.T) {
	got, err := ValidateSearch(SearchQuery{Query: "  what did the report say?  "})
	if err != nil {
		t.Fatal(err)
	}
	if got.Query != "what did the report say?" {
		t.Fatalf("query = %q, want it trimmed", got.Query)
	}
	if got.Limit != DefaultSearchLimit {
		t.Fatalf("limit = %d, want the default %d", got.Limit, DefaultSearchLimit)
	}
}

func TestValidateSearchClampsAndRejects(t *testing.T) {
	if got, _ := ValidateSearch(SearchQuery{Query: "q", Limit: MaxSearchLimit + 100}); got.Limit != MaxSearchLimit {
		t.Fatalf("limit = %d, want it clamped to %d", got.Limit, MaxSearchLimit)
	}
	cases := map[string]SearchQuery{
		"empty query":      {Query: "   "},
		"query too long":   {Query: strings.Repeat("a", MaxQueryLen+1)},
		"negative limit":   {Query: "q", Limit: -1},
		"similarity above": {Query: "q", MinSimilarity: 1.5},
		"similarity below": {Query: "q", MinSimilarity: -2},
		"too many ids":     {Query: "q", DocumentIDs: make([]uuid.UUID, MaxLimit+1)},
	}
	for name, q := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ValidateSearch(q); err == nil {
				t.Fatalf("ValidateSearch(%+v) = nil, want a validation error", q)
			}
		})
	}
}

func TestValidateFilter(t *testing.T) {
	got, err := ValidateFilter(Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Sort != DefaultSort || got.Limit != DefaultLimit {
		t.Fatalf("defaults = %+v, want sort %q limit %d", got, DefaultSort, DefaultLimit)
	}

	// An unknown sort must be a 400, not a silent fallback: Sorts is what
	// keeps request text out of the ORDER BY, so a miss has to be visible.
	for name, f := range map[string]Filter{
		"unknown sort":    {Sort: "; DROP TABLE documents"},
		"unknown status":  {Status: "pending"},
		"negative limit":  {Limit: -1},
		"negative offset": {Offset: -1},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ValidateFilter(f); err == nil {
				t.Fatalf("ValidateFilter(%+v) = nil, want a validation error", f)
			}
		})
	}

	if got, _ := ValidateFilter(Filter{Limit: MaxLimit + 1}); got.Limit != MaxLimit {
		t.Fatalf("limit = %d, want it clamped to %d", got.Limit, MaxLimit)
	}
}

// Every Sorts value has to be a valid SQL fragment naming a real column; the
// map is the only thing between a client and the ORDER BY.
func TestSortsAreColumnFragments(t *testing.T) {
	columns := map[string]bool{"created_at": true, "updated_at": true, "filename": true}
	for key, fragment := range Sorts {
		field := strings.TrimPrefix(key, "-")
		if !columns[field] {
			t.Fatalf("sort %q names %q, which is not a documents column", key, field)
		}
		if !strings.Contains(fragment, field) {
			t.Fatalf("sort %q maps to %q, which does not mention %q", key, fragment, field)
		}
	}
}

// The module returns the same error shape as the rest of the API.
func TestValidationErrorsAreTheSharedType(t *testing.T) {
	_, err := ValidateSearch(SearchQuery{})
	var verrs auth.ValidationErrors
	if !asValidationErrors(err, &verrs) || len(verrs) == 0 {
		t.Fatalf("error = %#v, want auth.ValidationErrors", err)
	}
	if verrs[0].Field != "query" {
		t.Fatalf("field = %q, want query", verrs[0].Field)
	}
}

func asValidationErrors(err error, dst *auth.ValidationErrors) bool {
	v, ok := err.(auth.ValidationErrors)
	if ok {
		*dst = v
	}
	return ok
}
