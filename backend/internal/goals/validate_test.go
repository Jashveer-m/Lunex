package goals

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jashveer/lifeos/backend/internal/optional"
	"github.com/jashveer/lifeos/backend/internal/validate"
)

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

// type has no column default, so unlike status it cannot be omitted.
func TestValidateCreateRequiresType(t *testing.T) {
	_, err := ValidateCreate(CreateInput{Title: "Learn Go"})
	if !contains(fieldsOf(t, err), "type") {
		t.Fatal("a goal without a type was accepted")
	}

	in, err := ValidateCreate(CreateInput{Title: "Learn Go", Type: "education"})
	if err != nil {
		t.Fatal(err)
	}
	if in.Status != DefaultStatus {
		t.Fatalf("status = %q, want the column default %q", in.Status, DefaultStatus)
	}
}

func TestValidateCreateRejectsUnknownEnums(t *testing.T) {
	_, err := ValidateCreate(CreateInput{Title: "x", Type: "vibes", Status: "paused"})
	got := fieldsOf(t, err)
	for _, want := range []string{"type", "status"} {
		if !contains(got, want) {
			t.Errorf("missing %q in %v", want, got)
		}
	}
}

func TestValidatePatchNullSemantics(t *testing.T) {
	p, err := ValidatePatch(UpdateInput{
		Description: optional.Null[string](),
		Deadline:    optional.Null[time.Time](),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !p.Description.Cleared() || !p.Deadline.Cleared() {
		t.Fatalf("null did not clear a nullable column: %+v", p)
	}
	if p.Title != nil || p.Type != nil || p.Status != nil {
		t.Fatalf("an unmentioned field produced an instruction: %+v", p)
	}

	// The NOT NULL columns cannot be cleared.
	_, err = ValidatePatch(UpdateInput{
		Title:  optional.Null[string](),
		Type:   optional.Null[string](),
		Status: optional.Null[string](),
	})
	got := fieldsOf(t, err)
	for _, want := range []string{"title", "type", "status"} {
		if !contains(got, want) {
			t.Errorf("clearing NOT NULL column %q was allowed (%v)", want, got)
		}
	}
}

func TestValidateMilestonePatch(t *testing.T) {
	p, err := ValidateMilestonePatch(MilestoneUpdateInput{Completed: optional.Of(true)})
	if err != nil {
		t.Fatal(err)
	}
	if p.Completed == nil || !*p.Completed {
		t.Fatalf("completed = %v, want true", p.Completed)
	}
	if p.Title != nil || p.TargetDate.Set {
		t.Fatalf("an unmentioned field produced an instruction: %+v", p)
	}

	// A target date can be cleared; completed cannot become NULL.
	p, err = ValidateMilestonePatch(MilestoneUpdateInput{TargetDate: optional.Null[time.Time]()})
	if err != nil || !p.TargetDate.Cleared() {
		t.Fatalf("target_date = %+v, %v; want a clear instruction", p.TargetDate, err)
	}
	if _, err := ValidateMilestonePatch(MilestoneUpdateInput{Completed: optional.Null[bool]()}); err == nil {
		t.Fatal("completed was allowed to become null")
	}
}

func TestValidateFilter(t *testing.T) {
	f, err := ValidateFilter(Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if f.Sort != DefaultSort || f.Limit != DefaultLimit {
		t.Fatalf("defaults = %q/%d", f.Sort, f.Limit)
	}
	if f, err := ValidateFilter(Filter{Limit: MaxLimit * 3}); err != nil || f.Limit != MaxLimit {
		t.Fatalf("limit = %d, %v; want it clamped", f.Limit, err)
	}
	if _, err := ValidateFilter(Filter{Sort: "deadline; DROP TABLE goals"}); err == nil {
		t.Fatal("an unknown sort was accepted")
	}
	if _, err := ValidateFilter(Filter{Type: "vibes"}); err == nil {
		t.Fatal("an unknown type filter was accepted")
	}
}

func TestSortsAreFixedFragments(t *testing.T) {
	for key, fragment := range Sorts {
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

// The text filter is trimmed and bounded: it is a substring to look for, and
// it scans every one of the owner's rows.
func TestFilterQueryIsTrimmedAndBounded(t *testing.T) {
	f, err := ValidateFilter(Filter{Query: "  antenna  "})
	if err != nil || f.Query != "antenna" {
		t.Fatalf("ValidateFilter = %+v, %v", f, err)
	}
	if _, err := ValidateFilter(Filter{Query: strings.Repeat("a", validate.MaxQueryLen+1)}); err == nil {
		t.Fatal("an oversized q was accepted")
	}
}
