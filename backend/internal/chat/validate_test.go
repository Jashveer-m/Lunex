package chat

import (
	"errors"
	"strings"
	"testing"

	"github.com/jashveer/lifeos/backend/internal/validate"
)

func TestValidateTitle(t *testing.T) {
	// An omitted title is not an error: it means "name it later", and the
	// first message does.
	for _, in := range []string{"", "   ", "\n"} {
		got, err := ValidateTitle(in)
		if err != nil || got != DefaultTitle {
			t.Fatalf("ValidateTitle(%q) = %q, %v; want the default and no error", in, got, err)
		}
	}
	if got, err := ValidateTitle("  Thesis  "); err != nil || got != "Thesis" {
		t.Fatalf("ValidateTitle = %q, %v", got, err)
	}
	var verrs validate.Errors
	if _, err := ValidateTitle(strings.Repeat("a", validate.MaxTitleLen+1)); !errors.As(err, &verrs) {
		t.Fatalf("err = %v, want a validation error", err)
	}
}

func TestValidateMessage(t *testing.T) {
	if got, err := ValidateMessage("  hello  "); err != nil || got != "hello" {
		t.Fatalf("ValidateMessage = %q, %v", got, err)
	}
	for _, in := range []string{"", "   ", strings.Repeat("a", MaxMessageLen+1)} {
		var verrs validate.Errors
		if _, err := ValidateMessage(in); !errors.As(err, &verrs) || verrs[0].Field != "content" {
			t.Fatalf("ValidateMessage(len %d) = %v, want a validation error on content", len(in), err)
		}
	}
}

func TestValidateFilter(t *testing.T) {
	f, err := ValidateFilter(Filter{})
	if err != nil || f.Sort != DefaultSort || f.Limit != DefaultLimit {
		t.Fatalf("defaults = %+v, %v", f, err)
	}
	if f, _ := ValidateFilter(Filter{Sort: "-updated_at", Limit: MaxLimit + 100}); f.Limit != MaxLimit {
		t.Fatalf("limit = %d, want it clamped to %d", f.Limit, MaxLimit)
	}
	// An unknown sort is a 400, not a silent fallback -- the client asked for
	// an order it is not getting.
	var verrs validate.Errors
	if _, err := ValidateFilter(Filter{Sort: "; DROP TABLE messages"}); !errors.As(err, &verrs) {
		t.Fatalf("err = %v, want a validation error", err)
	}
	if _, err := ValidateFilter(Filter{Limit: -1}); !errors.As(err, &verrs) {
		t.Fatalf("err = %v, want a validation error", err)
	}
	if _, err := ValidateFilter(Filter{Offset: -1}); !errors.As(err, &verrs) {
		t.Fatalf("err = %v, want a validation error", err)
	}
}
