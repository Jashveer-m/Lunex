package memories

import (
	"errors"
	"strings"
	"testing"

	"github.com/jashveer/lifeos/backend/internal/optional"
	"github.com/jashveer/lifeos/backend/internal/validate"
)

func TestValidateFilterDefaultsAndClamps(t *testing.T) {
	f, err := ValidateFilter(Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if f.Sort != DefaultSort || f.Limit != DefaultLimit || f.Offset != 0 {
		t.Fatalf("defaults = %+v", f)
	}
	if f.Enabled != nil {
		t.Fatal("the default list is filtered by enabled; it should show every memory the user has")
	}

	f, err = ValidateFilter(Filter{Limit: MaxLimit + 500})
	if err != nil {
		t.Fatal(err)
	}
	if f.Limit != MaxLimit {
		t.Fatalf("Limit = %d, want it clamped to %d", f.Limit, MaxLimit)
	}
}

// The three-state filter: absent lists everything, true and false each list
// half. This is the difference between "show me what you know" and "show me
// what you are still using".
func TestValidateFilterKeepsTheEnabledFlagThreeState(t *testing.T) {
	on, off := true, false
	for _, tc := range []struct {
		name string
		in   *bool
	}{{"absent", nil}, {"true", &on}, {"false", &off}} {
		t.Run(tc.name, func(t *testing.T) {
			f, err := ValidateFilter(Filter{Enabled: tc.in})
			if err != nil {
				t.Fatal(err)
			}
			if (f.Enabled == nil) != (tc.in == nil) {
				t.Fatalf("Enabled = %v, want %v", f.Enabled, tc.in)
			}
			if f.Enabled != nil && *f.Enabled != *tc.in {
				t.Fatalf("Enabled = %v, want %v", *f.Enabled, *tc.in)
			}
		})
	}
}

func TestValidateFilterRejectsWhatCannotReachTheSQL(t *testing.T) {
	for _, tc := range []struct {
		name  string
		in    Filter
		field string
	}{
		{"an unknown sort", Filter{Sort: "content"}, "sort"},
		{"an injected sort", Filter{Sort: "created_at; DROP TABLE memories"}, "sort"},
		{"an unknown type", Filter{Type: "vibes"}, "type"},
		{"a negative limit", Filter{Limit: -1}, "limit"},
		{"a negative offset", Filter{Offset: -1}, "offset"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var verrs validate.Errors
			if _, err := ValidateFilter(tc.in); !errors.As(err, &verrs) {
				t.Fatalf("err = %v, want a validation error", err)
			} else if verrs[0].Field != tc.field {
				t.Fatalf("the error names %q, want %q", verrs[0].Field, tc.field)
			}
		})
	}
	// Every listed type is accepted, so the allow-list and the CHECK
	// constraint cannot drift apart unnoticed.
	for _, kind := range Types {
		if _, err := ValidateFilter(Filter{Type: kind}); err != nil {
			t.Fatalf("type %q was rejected: %v", kind, err)
		}
	}
}

func TestValidatePatch(t *testing.T) {
	t.Run("an omitted key leaves the column alone", func(t *testing.T) {
		p, err := ValidatePatch(UpdateInput{})
		if err != nil {
			t.Fatal(err)
		}
		if !p.Empty() {
			t.Fatalf("patch = %+v, want nothing to change", p)
		}
	})

	t.Run("content is trimmed", func(t *testing.T) {
		p, err := ValidatePatch(UpdateInput{Content: optional.Of("  The user knows Go.  ")})
		if err != nil {
			t.Fatal(err)
		}
		if p.Content == nil || *p.Content != "The user knows Go." {
			t.Fatalf("content = %v", p.Content)
		}
	})

	t.Run("an oversized fact is rejected", func(t *testing.T) {
		var verrs validate.Errors
		_, err := ValidatePatch(UpdateInput{Content: optional.Of(strings.Repeat("a", MaxContentLen+1))})
		if !errors.As(err, &verrs) || verrs[0].Field != "content" {
			t.Fatalf("err = %v, want a content length error", err)
		}
	})

	t.Run("enabled survives being set to false", func(t *testing.T) {
		p, err := ValidatePatch(UpdateInput{Enabled: optional.Of(false)})
		if err != nil {
			t.Fatal(err)
		}
		// The bug this pins: treating `false` as "unset" would make a memory
		// impossible to switch off.
		if p.Enabled == nil || *p.Enabled {
			t.Fatalf("Enabled = %v, want a pointer to false", p.Enabled)
		}
	})
}

func TestValidateSearchDefaultsAndBounds(t *testing.T) {
	q, err := ValidateSearch(SearchQuery{Query: "  what do I prefer?  "})
	if err != nil {
		t.Fatal(err)
	}
	if q.Query != "what do I prefer?" || q.Limit != DefaultSearchLimit {
		t.Fatalf("query = %+v", q)
	}

	q, err = ValidateSearch(SearchQuery{Query: "x", Limit: MaxSearchLimit + 100})
	if err != nil {
		t.Fatal(err)
	}
	if q.Limit != MaxSearchLimit {
		t.Fatalf("Limit = %d, want it clamped to %d", q.Limit, MaxSearchLimit)
	}

	for _, tc := range []struct {
		name string
		in   SearchQuery
	}{
		{"empty", SearchQuery{Query: "   "}},
		{"oversized", SearchQuery{Query: strings.Repeat("a", MaxQueryLen+1)}},
		{"a floor above 1", SearchQuery{Query: "x", MinSimilarity: 1.5}},
		{"a negative floor", SearchQuery{Query: "x", MinSimilarity: -0.2}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var verrs validate.Errors
			if _, err := ValidateSearch(tc.in); !errors.As(err, &verrs) {
				t.Fatalf("err = %v, want a validation error", err)
			}
		})
	}
}
