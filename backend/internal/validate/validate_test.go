package validate_test

import (
	"strings"
	"testing"

	"github.com/jashveer/lifeos/backend/internal/validate"
)

func TestTitle(t *testing.T) {
	if got, err := validate.Title("title", "  Ship it  "); err != nil || got != "Ship it" {
		t.Fatalf("Title = %q, %v; want the trimmed value and no error", got, err)
	}
	// Whitespace-only is empty, not a title.
	if _, err := validate.Title("title", "   "); err == nil {
		t.Fatal("a whitespace-only title was accepted")
	}
	if _, err := validate.Title("title", ""); err == nil {
		t.Fatal("an empty title was accepted")
	}
	if _, err := validate.Title("title", strings.Repeat("a", validate.MaxTitleLen+1)); err == nil {
		t.Fatal("an over-long title was accepted")
	}
	// The limit counts runes, not bytes: a multi-byte title at the limit fits.
	if _, err := validate.Title("title", strings.Repeat("é", validate.MaxTitleLen)); err != nil {
		t.Fatalf("a %d-rune multi-byte title was rejected: %v", validate.MaxTitleLen, err)
	}
}

func TestTagsNormalize(t *testing.T) {
	got, err := validate.Tags("tags", []string{" work ", "work", "", "   ", "home"})
	if err != nil {
		t.Fatal(err)
	}
	// Trimmed, blanks dropped, duplicates removed, order preserved.
	if len(got) != 2 || got[0] != "work" || got[1] != "home" {
		t.Fatalf("Tags = %q, want [work home]", got)
	}

	if _, err := validate.Tags("tags", []string{strings.Repeat("a", validate.MaxTagLen+1)}); err == nil {
		t.Fatal("an over-long tag was accepted")
	}

	many := make([]string, validate.MaxTags+1)
	for i := range many {
		many[i] = string(rune('a'+i%26)) + strings.Repeat("x", i)
	}
	if _, err := validate.Tags("tags", many); err == nil {
		t.Fatal("too many tags were accepted")
	}
}

func TestOneOf(t *testing.T) {
	if e := validate.OneOf("status", "pending", []string{"pending", "done"}); e != nil {
		t.Fatalf("OneOf rejected a listed value: %v", e)
	}
	e := validate.OneOf("status", "PENDING", []string{"pending", "done"})
	if e == nil {
		t.Fatal("OneOf is case-insensitive; it should not be")
	}
	// The message names the allowed set: it is part of the contract.
	if !strings.Contains(e.Message, "pending, done") {
		t.Fatalf("message = %q, want it to list the allowed values", e.Message)
	}
}

func TestMinutes(t *testing.T) {
	if e := validate.Minutes("estimated_effort_minutes", 0); e != nil {
		t.Fatalf("zero minutes was rejected: %v", e)
	}
	if e := validate.Minutes("estimated_effort_minutes", -1); e == nil {
		t.Fatal("a negative estimate was accepted")
	}
	if e := validate.Minutes("estimated_effort_minutes", validate.MaxEffortMinutes+1); e == nil {
		t.Fatal("an absurd estimate was accepted")
	}
}

func TestTrimmedFieldClearsBlanks(t *testing.T) {
	if f := validate.TrimmedField("  x  "); !f.Set || f.Value == nil || *f.Value != "x" {
		t.Fatalf("TrimmedField(\"  x  \") = %+v", f)
	}
	if f := validate.TrimmedField("   "); !f.Cleared() {
		t.Fatalf("TrimmedField(\"   \") = %+v, want a clear instruction", f)
	}
}
