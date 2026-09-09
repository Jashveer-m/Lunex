package optional_test

import (
	"encoding/json"
	"testing"

	"github.com/jashveer/lifeos/backend/internal/optional"
)

// The whole point of Field is telling "absent" from "null" apart, so that is
// what the tests pin.
func TestFieldDistinguishesAbsentFromNull(t *testing.T) {
	type body struct {
		Title    optional.Field[string] `json:"title"`
		Deadline optional.Field[string] `json:"deadline"`
	}

	for _, tc := range []struct {
		name              string
		raw               string
		setTitle, setDead bool
		wantTitle         string
		titleSupplied     bool
		deadlineCleared   bool
	}{
		{name: "absent", raw: `{}`},
		{name: "value", raw: `{"title":"hi"}`, setTitle: true, wantTitle: "hi", titleSupplied: true},
		{name: "explicit null", raw: `{"deadline":null}`, setDead: true, deadlineCleared: true},
		{
			name: "value and null together", raw: `{"title":"hi","deadline":null}`,
			setTitle: true, setDead: true, wantTitle: "hi", titleSupplied: true, deadlineCleared: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var b body
			if err := json.Unmarshal([]byte(tc.raw), &b); err != nil {
				t.Fatal(err)
			}
			if b.Title.Set != tc.setTitle {
				t.Errorf("title.Set = %t, want %t", b.Title.Set, tc.setTitle)
			}
			if b.Deadline.Set != tc.setDead {
				t.Errorf("deadline.Set = %t, want %t", b.Deadline.Set, tc.setDead)
			}
			got, ok := b.Title.Get()
			if ok != tc.titleSupplied || got != tc.wantTitle {
				t.Errorf("title.Get() = %q, %t; want %q, %t", got, ok, tc.wantTitle, tc.titleSupplied)
			}
			if b.Deadline.Cleared() != tc.deadlineCleared {
				t.Errorf("deadline.Cleared() = %t, want %t", b.Deadline.Cleared(), tc.deadlineCleared)
			}
		})
	}
}

func TestFieldRejectsWrongType(t *testing.T) {
	var f optional.Field[int]
	if err := json.Unmarshal([]byte(`"not a number"`), &f); err == nil {
		t.Fatal("a string decoded into Field[int] without error")
	}
}

func TestOfAndNull(t *testing.T) {
	v, ok := optional.Of("x").Get()
	if !ok || v != "x" {
		t.Fatalf("Of(\"x\").Get() = %q, %t", v, ok)
	}
	if n := optional.Null[string](); !n.Cleared() || !n.Set {
		t.Fatalf("Null() = %+v, want a set, cleared field", n)
	}
}
