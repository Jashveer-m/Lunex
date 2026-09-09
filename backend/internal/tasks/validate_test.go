package tasks

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jashveer/lifeos/backend/internal/optional"
	"github.com/jashveer/lifeos/backend/internal/validate"
)

// fieldsOf pulls the field names out of a validation failure so a test can
// assert on what was rejected rather than on message wording.
func fieldsOf(t *testing.T, err error) []string {
	t.Helper()
	var verrs validate.Errors
	if !errors.As(err, &verrs) {
		t.Fatalf("err = %v (%T), want validation errors", err, err)
	}
	out := make([]string, 0, len(verrs))
	for _, e := range verrs {
		out = append(out, e.Field)
	}
	return out
}

func TestValidateCreateDefaultsAndNormalizes(t *testing.T) {
	in, err := ValidateCreate(CreateInput{
		Title:       "  Write the brief  ",
		Description: "  notes  ",
		Category:    "  work  ",
		Tags:        []string{" a ", "a", ""},
	})
	if err != nil {
		t.Fatalf("ValidateCreate: %v", err)
	}
	if in.Title != "Write the brief" || in.Description != "notes" || in.Category != "work" {
		t.Fatalf("free text was not trimmed: %+v", in)
	}
	// Omitted enums fall back to the same values the columns default to.
	if in.Priority != DefaultPriority || in.Status != DefaultStatus {
		t.Fatalf("priority/status = %q/%q, want the column defaults", in.Priority, in.Status)
	}
	if len(in.Tags) != 1 || in.Tags[0] != "a" {
		t.Fatalf("tags = %q, want them trimmed and de-duplicated", in.Tags)
	}
}

func TestValidateCreateReportsEveryProblemAtOnce(t *testing.T) {
	bad := -5
	_, err := ValidateCreate(CreateInput{
		Title:                  "",
		Priority:               "urgent",
		Status:                 "doing",
		Description:            strings.Repeat("x", validate.MaxDescriptionLen+1),
		EstimatedEffortMinutes: &bad,
	})
	got := fieldsOf(t, err)
	for _, want := range []string{"title", "priority", "status", "description", "estimated_effort_minutes"} {
		if !contains(got, want) {
			t.Errorf("missing %q in %v", want, got)
		}
	}
}

func TestValidatePatchOnlyTouchesMentionedFields(t *testing.T) {
	p, err := ValidatePatch(UpdateInput{Status: optional.Of("completed")})
	if err != nil {
		t.Fatal(err)
	}
	if p.Status == nil || *p.Status != "completed" {
		t.Fatalf("status = %v, want completed", p.Status)
	}
	if p.Title != nil || p.Description.Set || p.Tags != nil || p.Deadline.Set {
		t.Fatalf("an unmentioned field produced an instruction: %+v", p)
	}
}

func TestValidatePatchNullSemantics(t *testing.T) {
	p, err := ValidatePatch(UpdateInput{
		Description: optional.Null[string](),
		Category:    optional.Null[string](),
		Deadline:    optional.Null[time.Time](),
		Tags:        optional.Null[[]string](),
	})
	if err != nil {
		t.Fatal(err)
	}
	// Nullable columns are cleared...
	if !p.Description.Cleared() || !p.Category.Cleared() || !p.Deadline.Cleared() {
		t.Fatalf("null did not clear a nullable column: %+v", p)
	}
	// ...but tags is NOT NULL, so null means "no tags", not NULL.
	if p.Tags == nil || len(*p.Tags) != 0 {
		t.Fatalf("tags = %v, want an empty slice", p.Tags)
	}
}

func TestValidatePatchRejectsClearingRequiredColumns(t *testing.T) {
	_, err := ValidatePatch(UpdateInput{
		Title:    optional.Null[string](),
		Priority: optional.Null[string](),
		Status:   optional.Null[string](),
	})
	got := fieldsOf(t, err)
	for _, want := range []string{"title", "priority", "status"} {
		if !contains(got, want) {
			t.Errorf("clearing NOT NULL column %q was allowed (%v)", want, got)
		}
	}
}

func TestValidateFilter(t *testing.T) {
	f, err := ValidateFilter(Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if f.Sort != DefaultSort || f.Limit != DefaultLimit {
		t.Fatalf("defaults = %q/%d, want %q/%d", f.Sort, f.Limit, DefaultSort, DefaultLimit)
	}

	// An oversized limit is clamped rather than rejected: the client still gets
	// a useful answer, just not an unbounded one.
	if f, err := ValidateFilter(Filter{Limit: MaxLimit * 10}); err != nil || f.Limit != MaxLimit {
		t.Fatalf("limit = %d, %v; want it clamped to %d", f.Limit, err, MaxLimit)
	}

	// A bad sort is rejected, not silently ignored, so the typo surfaces.
	if _, err := ValidateFilter(Filter{Sort: "title; DROP TABLE tasks"}); err == nil {
		t.Fatal("an unknown sort was accepted")
	}
	if _, err := ValidateFilter(Filter{Status: "nonsense"}); err == nil {
		t.Fatal("an unknown status filter was accepted")
	}
	if _, err := ValidateFilter(Filter{Offset: -1}); err == nil {
		t.Fatal("a negative offset was accepted")
	}
}

// Every sortable value must be a fragment we wrote, never client text. This
// test is the guard on that: it fails the moment a value is added that could
// carry something else into the query.
func TestSortsAreFixedFragments(t *testing.T) {
	for key, fragment := range Sorts {
		if strings.ContainsAny(fragment, ";-") && !strings.HasPrefix(key, "-") {
			t.Errorf("sort %q maps to a suspicious fragment %q", key, fragment)
		}
		if strings.Contains(fragment, ";") {
			t.Errorf("sort %q maps to a fragment containing a statement separator: %q", key, fragment)
		}
	}
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
