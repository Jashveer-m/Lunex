package study

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/jashveer/lifeos/backend/internal/optional"
	"github.com/jashveer/lifeos/backend/internal/validate"
)

func fields(t *testing.T, err error) []string {
	t.Helper()
	var verrs validate.Errors
	if !errors.As(err, &verrs) {
		t.Fatalf("err = %v, want validation errors", err)
	}
	out := make([]string, 0, len(verrs))
	for _, v := range verrs {
		out = append(out, v.Field)
	}
	return out
}

func TestValidateCreatePlanReportsEveryProblemAtOnce(t *testing.T) {
	nilID := uuid.Nil
	_, err := ValidateCreatePlan(CreatePlanInput{
		Title: "   ", Description: strings.Repeat("x", validate.MaxDescriptionLen+1),
		Status: "studying", DocumentID: &nilID,
	})
	got := fields(t, err)
	for _, want := range []string{"title", "description", "status", "document_id"} {
		if !contains(got, want) {
			t.Fatalf("fields = %v, want %s among them", got, want)
		}
	}
}

func TestAPlanWithNoStatusIsActive(t *testing.T) {
	in, err := ValidateCreatePlan(CreatePlanInput{Title: "Finals"})
	if err != nil {
		t.Fatal(err)
	}
	if in.Status != StatusActive {
		t.Fatalf("status = %q", in.Status)
	}
	// And a status in the wrong case is the same status.
	in, err = ValidateCreatePlan(CreatePlanInput{Title: "Finals", Status: " COMPLETED "})
	if err != nil {
		t.Fatal(err)
	}
	if in.Status != StatusCompleted {
		t.Fatalf("status = %q", in.Status)
	}
}

// A PATCH says three different things with three different bodies, and the
// two NOT NULL columns cannot be cleared.
func TestValidatePatchDistinguishesUnsetNullAndAValue(t *testing.T) {
	p, err := ValidatePatch(UpdatePlanInput{})
	if err != nil {
		t.Fatal(err)
	}
	if !p.Empty() {
		t.Fatalf("an empty body produced %+v", p)
	}

	p, err = ValidatePatch(clearedDescription())
	if err != nil {
		t.Fatal(err)
	}
	if !p.Description.Set || p.Description.Value != nil {
		t.Fatalf("a null description produced %+v", p.Description)
	}

	if _, err := ValidatePatch(UpdatePlanInput{Title: optional.Null[string]()}); err == nil {
		t.Fatal("a null title was accepted")
	}
	if _, err := ValidatePatch(UpdatePlanInput{Status: optional.Null[string]()}); err == nil {
		t.Fatal("a null status was accepted")
	}
}

func TestValidateFilterClampsPagingAndChecksTheClosedSets(t *testing.T) {
	f, err := ValidateFilter(Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if f.Limit != DefaultLimit || f.Sort != DefaultSort {
		t.Fatalf("defaults = %+v", f)
	}
	if f, _ = ValidateFilter(Filter{Limit: MaxLimit * 10}); f.Limit != MaxLimit {
		t.Fatalf("limit = %d, want it clamped to %d", f.Limit, MaxLimit)
	}
	if _, err := ValidateFilter(Filter{Limit: -1}); err == nil {
		t.Fatal("a negative limit was accepted")
	}
	if _, err := ValidateFilter(Filter{Sort: "front"}); err == nil {
		t.Fatal("an unknown sort was accepted")
	}
	if _, err := ValidateFilter(Filter{Status: "studying"}); err == nil {
		t.Fatal("an unknown status filter was accepted")
	}
}

// A deck is meant to be read whole, so its paging bounds are larger than the
// resource ones. That is a decision, so it is pinned.
func TestACardFilterUsesTheDeckPagingBounds(t *testing.T) {
	f, err := ValidateCardFilter(CardFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if f.Limit != DefaultCardLimit {
		t.Fatalf("limit = %d, want %d", f.Limit, DefaultCardLimit)
	}
	if f, _ = ValidateCardFilter(CardFilter{Limit: MaxCardLimit * 10}); f.Limit != MaxCardLimit {
		t.Fatalf("limit = %d, want it clamped to %d", f.Limit, MaxCardLimit)
	}
	if DefaultCardLimit <= DefaultLimit {
		t.Fatal("a deck reads in smaller pages than a list of plans")
	}
}

func TestValidateCardRequiresBothSidesAndBoundsThem(t *testing.T) {
	got := fields(t, mustFail(t, NewCard{}))
	if len(got) != 2 || got[0] != "front" || got[1] != "back" {
		t.Fatalf("fields = %v, want both sides named at once", got)
	}
	long := strings.Repeat("x", MaxCardSideLen+1)
	if err := mustFail(t, NewCard{Front: long, Back: long}); len(fields(t, err)) != 2 {
		t.Fatalf("an essay on both sides was reported as %v", fields(t, err))
	}

	c, err := ValidateCard(NewCard{Front: "  What   is\n it? ", Back: " This. "})
	if err != nil {
		t.Fatal(err)
	}
	if c.Front != "What is it?" || c.Back != "This." {
		t.Fatalf("card = %+v, want it collapsed", c)
	}
}

func mustFail(t *testing.T, c NewCard) error {
	t.Helper()
	_, err := ValidateCard(c)
	if err == nil {
		t.Fatalf("%+v was accepted", c)
	}
	return err
}

// "Make me fifty cards" is a reasonable thing to say, and the cap is the
// honest answer to it -- not a refusal.
func TestValidateGenerateClampsTheCount(t *testing.T) {
	in, err := ValidateGenerate(GenerateInput{DocumentID: uuid.New(), Count: 500})
	if err != nil {
		t.Fatal(err)
	}
	if in.Count != MaxCardsPerBatch {
		t.Fatalf("count = %d, want %d", in.Count, MaxCardsPerBatch)
	}
	if in, _ = ValidateGenerate(GenerateInput{DocumentID: uuid.New()}); in.Count != DefaultCardsPerBatch {
		t.Fatalf("count = %d, want the default %d", in.Count, DefaultCardsPerBatch)
	}
	if _, err := ValidateGenerate(GenerateInput{}); err == nil {
		t.Fatal("a generation with no document was accepted")
	}
	if _, err := ValidateGenerate(GenerateInput{
		DocumentID: uuid.New(), Topic: strings.Repeat("x", MaxTopicLen+1),
	}); err == nil {
		t.Fatal("a topic longer than a phrase was accepted")
	}
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}
