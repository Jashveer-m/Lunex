package notes

import (
	"errors"
	"strings"
	"testing"

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

func TestValidateCreate(t *testing.T) {
	in, err := ValidateCreate(CreateInput{Title: "  Ideas  ", Tags: []string{" a ", "a", "b"}})
	if err != nil {
		t.Fatal(err)
	}
	if in.Title != "Ideas" {
		t.Fatalf("title = %q, want it trimmed", in.Title)
	}
	if len(in.Tags) != 2 {
		t.Fatalf("tags = %q, want them de-duplicated", in.Tags)
	}
	// content is NOT NULL DEFAULT '': an omitted body is an empty note, not an error.
	if in.Content != "" {
		t.Fatalf("content = %q, want empty", in.Content)
	}

	_, err = ValidateCreate(CreateInput{Title: "", Content: strings.Repeat("x", validate.MaxContentLen+1)})
	got := fieldsOf(t, err)
	for _, want := range []string{"title", "content"} {
		if !contains(got, want) {
			t.Errorf("missing %q in %v", want, got)
		}
	}
}

// content and tags are NOT NULL, so an explicit null empties them rather than
// failing or writing NULL.
func TestValidatePatchNullEmptiesNotNullColumns(t *testing.T) {
	p, err := ValidatePatch(UpdateInput{
		Content: optional.Null[string](),
		Tags:    optional.Null[[]string](),
	})
	if err != nil {
		t.Fatal(err)
	}
	if p.Content == nil || *p.Content != "" {
		t.Fatalf("content = %v, want an empty string", p.Content)
	}
	if p.Tags == nil || len(*p.Tags) != 0 {
		t.Fatalf("tags = %v, want an empty slice", p.Tags)
	}
	if p.Title != nil {
		t.Fatal("an unmentioned title produced an instruction")
	}

	if _, err := ValidatePatch(UpdateInput{Title: optional.Null[string]()}); err == nil {
		t.Fatal("the title was allowed to become null")
	}
}

func TestValidatePatchEmptyIsANoOp(t *testing.T) {
	p, err := ValidatePatch(UpdateInput{})
	if err != nil {
		t.Fatal(err)
	}
	if !p.Empty() {
		t.Fatalf("an empty PATCH produced instructions: %+v", p)
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
	if _, err := ValidateFilter(Filter{Sort: "created_at; DROP TABLE notes"}); err == nil {
		t.Fatal("an unknown sort was accepted")
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
