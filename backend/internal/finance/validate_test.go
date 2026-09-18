package finance

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jashveer/lifeos/backend/internal/optional"
	"github.com/jashveer/lifeos/backend/internal/validate"
)

// --- money ---------------------------------------------------------------------

// The single most important test in this package: money is exact, and the
// decimal the caller wrote is the integer that gets stored.
func TestParseAmountIsExact(t *testing.T) {
	for written, want := range map[string]Amount{
		"0.01": 1, "0.10": 10, "0.1": 10, "1": 100, "12.34": 1234,
		"1234.5": 123450, "1,234.50": 123450, "₹1,200": 120000, "$0.99": 99,
		" 45.00 ": 4500, "9999999999.99": MaxAmount,
		// Grouping survives the spaces a person or a model puts around it.
		"1, 200": 120000, "₹ 1,234.50": 123450, "12_345": 1234500,
	} {
		got, err := ParseAmount(written)
		if err != nil || got != want {
			t.Fatalf("ParseAmount(%q) = %d, %v; want %d", written, got, err, want)
		}
	}
}

// Three decimal places is refused rather than rounded. 12.345 is a number this
// column cannot hold, and silently making it 12.35 is the kind of quiet edit a
// ledger must not make.
func TestParseAmountRefusesWhatItCannotStore(t *testing.T) {
	for _, written := range []string{
		"", "abc", "12.345", "1e3", "12.3.4", "--5", "12-",
		// A comma is a grouping separator here and never a decimal point, and
		// the ambiguous case is refused rather than guessed: silently reading
		// "12,34" as 1234.00 multiplies somebody's expense by a hundred.
		"12,34", "12,3", "1,23456", "12,",
	} {
		if got, err := ParseAmount(written); !errors.Is(err, ErrNotAnAmount) {
			t.Fatalf("ParseAmount(%q) = %d, %v; want ErrNotAnAmount", written, got, err)
		}
	}

	// How large *one expense* may be is MaxAmount, and it is validation's rule
	// rather than the parser's -- the parser also reads back the sums the
	// summary query produces, and two expenses of MaxAmount add up to more
	// than one is allowed to be.
	sum, err := ParseAmount("19999999999.98")
	if err != nil || sum != 2*MaxAmount {
		t.Fatalf("the sum of two maximum expenses = %d, %v; want it to read back", sum, err)
	}
	if e := amount("amount", sum); e == nil {
		t.Fatal("a single expense of twice the maximum was accepted")
	}
	// And a leading zero is a leading zero, not an overflow.
	if got, err := ParseAmount("00000000000012.34"); err != nil || got != 1234 {
		t.Fatalf("ParseAmount with leading zeros = %d, %v; want 12.34", got, err)
	}
}

// The float64 this type exists to avoid: 0.1 + 0.2 is 0.30000000000000004 in
// binary floating point and exactly 0.30 here.
func TestAmountsAddUpTheWayPeopleDo(t *testing.T) {
	a, _ := ParseAmount("0.1")
	b, _ := ParseAmount("0.2")
	if got := (a + b).String(); got != "0.30" {
		t.Fatalf("0.1 + 0.2 = %s, want 0.30", got)
	}
	var tenth Amount = 10
	var total Amount
	for i := 0; i < 1000; i++ {
		total += tenth
	}
	if got := total.String(); got != "100.00" {
		t.Fatalf("a thousand lots of 0.10 = %s, want 100.00", got)
	}
}

// An amount goes over the wire as an unquoted JSON number with two places, and
// comes back as the same integer -- through the JSON text, never through a
// float64.
func TestAmountRoundTripsThroughJSON(t *testing.T) {
	type wrapper struct {
		Amount Amount `json:"amount"`
	}
	raw, err := json.Marshal(wrapper{Amount: 123450})
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"amount":1234.50}` {
		t.Fatalf("marshalled to %s, want an unquoted 1234.50", raw)
	}
	for _, body := range []string{`{"amount":1234.50}`, `{"amount":1234.5}`, `{"amount":"1234.50"}`} {
		var back wrapper
		if err := json.Unmarshal([]byte(body), &back); err != nil || back.Amount != 123450 {
			t.Fatalf("%s decoded to %d, %v; want 123450", body, back.Amount, err)
		}
	}
	var bad wrapper
	if err := json.Unmarshal([]byte(`{"amount":1.005}`), &bad); err == nil {
		t.Fatal("a third decimal place was accepted")
	}
}

// --- create --------------------------------------------------------------------

func TestValidateCreate(t *testing.T) {
	ok, err := ValidateCreate(CreateInput{
		Amount: 45000, Currency: " inr ", Description: "  lunch  ",
		Date: time.Date(2026, 9, 17, 13, 45, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	switch {
	case ok.Currency != "INR":
		t.Fatalf("currency = %q, want INR", ok.Currency)
	case ok.Description != "lunch":
		t.Fatalf("description = %q, want it trimmed", ok.Description)
	case !ok.Date.Equal(time.Date(2026, 9, 17, 0, 0, 0, 0, time.UTC)):
		t.Fatalf("date = %v, want the day at midnight UTC", ok.Date)
	}

	// An empty currency is the column default, not an error: most people have
	// one currency and never say which.
	defaulted, err := ValidateCreate(CreateInput{Amount: 1, Date: ok.Date})
	if err != nil || defaulted.Currency != DefaultCurrency {
		t.Fatalf("an unstated currency = %q, %v; want %s", defaulted.Currency, err, DefaultCurrency)
	}
}

func TestValidateCreateRejects(t *testing.T) {
	day := time.Date(2026, 9, 17, 0, 0, 0, 0, time.UTC)
	for name, tc := range map[string]struct {
		in    CreateInput
		field string
	}{
		"no amount":       {CreateInput{Date: day}, "amount"},
		"a negative sum":  {CreateInput{Amount: -100, Date: day}, "amount"},
		"more than fits":  {CreateInput{Amount: MaxAmount + 1, Date: day}, "amount"},
		"no date":         {CreateInput{Amount: 100}, "expense_date"},
		"a bad currency":  {CreateInput{Amount: 100, Currency: "rupees", Date: day}, "currency"},
		"a long note":     {CreateInput{Amount: 100, Date: day, Description: strings.Repeat("x", MaxDescriptionLen+1)}, "description"},
		"a zero category": {CreateInput{Amount: 100, Date: day, CategoryID: &uuid.Nil}, "category_id"},
	} {
		var verrs validate.Errors
		_, err := ValidateCreate(tc.in)
		if !errors.As(err, &verrs) {
			t.Fatalf("%s: ValidateCreate = %v, want a validation error", name, err)
		}
		if verrs[0].Field != tc.field {
			t.Fatalf("%s: the error names %q, want %q", name, verrs[0].Field, tc.field)
		}
	}

	// Every problem is reported at once, not one per round trip.
	_, err := ValidateCreate(CreateInput{Currency: "zzzz"})
	var verrs validate.Errors
	if !errors.As(err, &verrs) || len(verrs) != 3 {
		t.Fatalf("three broken fields produced %v, want three errors", err)
	}
}

// --- patch ---------------------------------------------------------------------

// The NOT NULL columns cannot be cleared: a null is a validation failure rather
// than a 500 from Postgres.
func TestValidatePatchRefusesToClearRequiredColumns(t *testing.T) {
	for field, in := range map[string]UpdateInput{
		"amount":       {Amount: optional.Null[Amount]()},
		"currency":     {Currency: optional.Null[string]()},
		"expense_date": {Date: optional.Null[time.Time]()},
	} {
		var verrs validate.Errors
		if _, err := ValidatePatch(in); !errors.As(err, &verrs) || verrs[0].Field != field {
			t.Fatalf("clearing %s = %v, want a field error naming it", field, err)
		}
	}
}

// The nullable ones can be: an expense can stop being filed under a category
// and stop pointing at a receipt.
func TestValidatePatchClearsNullableColumns(t *testing.T) {
	p, err := ValidatePatch(UpdateInput{
		CategoryID:        optional.Null[uuid.UUID](),
		RelatedDocumentID: optional.Null[uuid.UUID](),
		Description:       optional.Null[string](),
	})
	if err != nil {
		t.Fatal(err)
	}
	switch {
	case !p.CategoryID.Set || p.CategoryID.Value != nil:
		t.Fatalf("category_id = %+v, want a clear instruction", p.CategoryID)
	case !p.RelatedDocumentID.Set || p.RelatedDocumentID.Value != nil:
		t.Fatalf("related_document_id = %+v, want a clear instruction", p.RelatedDocumentID)
	case !p.Description.Set || p.Description.Value != nil:
		t.Fatalf("description = %+v, want a clear instruction", p.Description)
	}
	if p.Empty() {
		t.Fatal("a patch clearing three columns reports that it touches none")
	}

	// An empty patch touches nothing.
	empty, err := ValidatePatch(UpdateInput{})
	if err != nil || !empty.Empty() {
		t.Fatalf("an empty patch = %+v, %v", empty, err)
	}
}

// --- filter --------------------------------------------------------------------

func TestValidateFilter(t *testing.T) {
	// No range at all is legal here -- see the package comment.
	f, err := ValidateFilter(Filter{})
	if err != nil {
		t.Fatal(err)
	}
	switch {
	case f.Start != nil || f.End != nil:
		t.Fatalf("an unbounded filter grew a window: %+v", f)
	case f.Sort != DefaultSort:
		t.Fatalf("sort = %q, want %q", f.Sort, DefaultSort)
	case f.Limit != DefaultLimit:
		t.Fatalf("limit = %d, want %d", f.Limit, DefaultLimit)
	}

	// The bounds are reduced to days, whatever time of day the caller sent.
	noon := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	windowed, err := ValidateFilter(Filter{Start: &noon, End: &noon})
	if err != nil {
		t.Fatal(err)
	}
	if windowed.Start.Hour() != 0 || windowed.End.Hour() != 0 {
		t.Fatalf("the bounds kept a time of day: %v to %v", windowed.Start, windowed.End)
	}

	// Paging is clamped rather than refused; a negative one is a field error.
	clamped, err := ValidateFilter(Filter{Limit: MaxLimit + 1000})
	if err != nil || clamped.Limit != MaxLimit {
		t.Fatalf("limit = %d, %v; want it clamped to %d", clamped.Limit, err, MaxLimit)
	}
	for field, f := range map[string]Filter{
		"limit":  {Limit: -1},
		"offset": {Offset: -1},
		"sort":   {Sort: "amount; DROP TABLE expenses"},
	} {
		var verrs validate.Errors
		if _, err := ValidateFilter(f); !errors.As(err, &verrs) || verrs[0].Field != field {
			t.Fatalf("%s = %v, want a field error naming it", field, err)
		}
	}
}

// Sorting is a fixed map, so no request text ever reaches the ORDER BY.
func TestSortsAreAClosedSet(t *testing.T) {
	for name, fragment := range Sorts {
		if strings.ContainsAny(fragment, ";'\"()") {
			t.Fatalf("the sort fragment for %q is not a plain column and direction: %q", name, fragment)
		}
	}
	if _, ok := Sorts[DefaultSort]; !ok {
		t.Fatalf("DefaultSort %q is not in Sorts", DefaultSort)
	}
}

// --- category names --------------------------------------------------------------

func TestValidateCategoryName(t *testing.T) {
	got, err := ValidateCategoryName("  Books  and   Courses ")
	if err != nil || got != "Books and Courses" {
		t.Fatalf("= %q, %v; want the whitespace collapsed", got, err)
	}
	for _, name := range []string{"", "   ", strings.Repeat("x", MaxCategoryNameLen+1)} {
		var verrs validate.Errors
		if _, err := ValidateCategoryName(name); !errors.As(err, &verrs) || verrs[0].Field != "name" {
			t.Fatalf("%q = %v, want a field error naming name", name, err)
		}
	}
}
