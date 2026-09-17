package calendar

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jashveer/lifeos/backend/internal/optional"
	"github.com/jashveer/lifeos/backend/internal/validate"
)

func fieldsOf(t *testing.T, err error) map[string]string {
	t.Helper()
	var verrs validate.Errors
	if !errors.As(err, &verrs) {
		t.Fatalf("err = %v, want validation errors", err)
	}
	out := map[string]string{}
	for _, v := range verrs {
		out[v.Field] = v.Message
	}
	return out
}

func TestValidateCreateChecksTheInterval(t *testing.T) {
	// An end before a start is the CHECK constraint, reported as a field.
	_, err := ValidateCreate(CreateInput{Title: "x", StartTime: tenAM, EndTime: nine})
	if got := fieldsOf(t, err)["end_time"]; !strings.Contains(got, "before start_time") {
		t.Fatalf("end_time = %q", got)
	}

	// A start and no end is an instant, not an error.
	in, err := ValidateCreate(CreateInput{Title: "  Reminder ", StartTime: nine})
	if err != nil {
		t.Fatal(err)
	}
	if !in.EndTime.Equal(nine) || in.Title != "Reminder" {
		t.Fatalf("input = %+v", in)
	}

	// No start at all is the one required field beyond the title.
	_, err = ValidateCreate(CreateInput{Title: "x"})
	if got := fieldsOf(t, err)["start_time"]; got != "is required" {
		t.Fatalf("start_time = %q", got)
	}

	// Every problem is reported at once.
	problems := fieldsOf(t, func() error {
		_, err := ValidateCreate(CreateInput{
			Title:          " ",
			Location:       strings.Repeat("a", MaxLocationLen+1),
			RecurrenceRule: strings.Repeat("R", MaxRecurrenceRuleLen+1),
		})
		return err
	}())
	for _, field := range []string{"title", "location", "recurrence_rule", "start_time"} {
		if _, ok := problems[field]; !ok {
			t.Fatalf("%s is not reported: %v", field, problems)
		}
	}
}

// The recurrence rule is stored, not parsed: anything within the length limit
// is accepted, because this phase does not claim to understand it.
func TestRecurrenceRuleIsOpaque(t *testing.T) {
	for _, rule := range []string{
		"FREQ=WEEKLY;BYDAY=MO,WE",
		"every other tuesday",
		"RRULE:FREQ=DAILY;COUNT=10",
	} {
		in, err := ValidateCreate(CreateInput{Title: "x", StartTime: nine, EndTime: tenAM, RecurrenceRule: rule})
		if err != nil || in.RecurrenceRule != rule {
			t.Fatalf("%q was not stored as written: %+v %v", rule, in.RecurrenceRule, err)
		}
	}
}

func TestValidatePatchKeepsOmittedFieldsOut(t *testing.T) {
	p, err := ValidatePatch(UpdateInput{Title: optional.Of("Retro")})
	if err != nil {
		t.Fatal(err)
	}
	if p.Title == nil || *p.Title != "Retro" {
		t.Fatalf("title = %v", p.Title)
	}
	if p.StartTime != nil || p.EndTime != nil || p.AllDay != nil || p.Location.Set {
		t.Fatalf("an omitted field became an instruction: %+v", p)
	}
	if p.Empty() || p.MovesTheInterval() {
		t.Fatalf("patch = %+v", p)
	}

	// Null clears what is nullable, empties what is not, and is refused where
	// the column is required.
	p, err = ValidatePatch(UpdateInput{
		Description: optional.Null[string](),
		Location:    optional.Null[string](),
		AllDay:      optional.Null[bool](),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !p.Description.Set || p.Description.Value != nil {
		t.Fatalf("description = %+v, want a clear instruction", p.Description)
	}
	if p.AllDay == nil || *p.AllDay {
		t.Fatalf("all_day = %v, want false", p.AllDay)
	}
	for field, in := range map[string]UpdateInput{
		"title":      {Title: optional.Null[string]()},
		"start_time": {StartTime: optional.Null[time.Time]()},
		"end_time":   {EndTime: optional.Null[time.Time]()},
	} {
		if _, err := ValidatePatch(in); fieldsOf(t, err)[field] != "is required" {
			t.Fatalf("clearing %s = %v, want a required-field error", field, err)
		}
	}

	// A patch carrying both ends is checked without a round trip.
	_, err = ValidatePatch(UpdateInput{StartTime: optional.Of(tenAM), EndTime: optional.Of(nine)})
	if got := fieldsOf(t, err)["end_time"]; !strings.Contains(got, "before start_time") {
		t.Fatalf("end_time = %q", got)
	}

	// A link is passed through untouched; the service is what checks it.
	id := uuid.New()
	p, err = ValidatePatch(UpdateInput{RelatedTaskID: optional.Of(id)})
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := p.RelatedTaskID.Get(); !ok || got != id {
		t.Fatalf("related_task_id = %+v", p.RelatedTaskID)
	}
}

func TestValidateFilterClampsAndDefaults(t *testing.T) {
	f, err := ValidateFilter(Filter{Start: day, End: day.AddDate(0, 0, 1), Limit: MaxLimit + 1000})
	if err != nil {
		t.Fatal(err)
	}
	if f.Limit != MaxLimit {
		t.Fatalf("limit = %d, want it clamped to %d", f.Limit, MaxLimit)
	}
	if f.Sort != DefaultSort || Sorts[f.Sort] != "start_time ASC" {
		t.Fatalf("sort = %q; a calendar reads forwards", f.Sort)
	}

	// An unknown sort is a client error rather than a silent fallback.
	if _, err := ValidateFilter(Filter{Start: day, End: day.AddDate(0, 0, 1), Sort: "priority"}); fieldsOf(t, err)["sort"] == "" {
		t.Fatal("an unknown sort was accepted")
	}
	if _, err := ValidateFilter(Filter{Start: day, End: day.AddDate(0, 0, 1), Offset: -1}); fieldsOf(t, err)["offset"] == "" {
		t.Fatal("a negative offset was accepted")
	}
}
