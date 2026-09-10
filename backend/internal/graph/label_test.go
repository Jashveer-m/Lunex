package graph

import "testing"

func TestNormalizeLabel(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"  Go  ", "Go"},
		{"the  backend   project", "the backend project"},
		{`"Go"`, "Go"},
		{"Go.", "Go"},
		{"Alice,", "Alice"},
		{"'the compiler project'.", "the compiler project"},
		// Leading punctuation is part of the name, not decoration: ".NET" and
		// "NET" are different things.
		{".NET", ".NET"},
		{"#golang,", "#golang"},
		{`"C++".`, "C++"},
		{"", ""},
		{"   ", ""},
	} {
		if got := NormalizeLabel(tc.in); got != tc.want {
			t.Fatalf("NormalizeLabel(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// The stored label keeps its case -- it is what a client displays -- while the
// comparison does not. Both halves have to be true or "Go" and "go" become two
// nodes despite the fold.
func TestFoldLabelLowercasesOnlyTheComparison(t *testing.T) {
	if got := NormalizeLabel(" Go "); got != "Go" {
		t.Fatalf("NormalizeLabel lowercased the stored label: %q", got)
	}
	if FoldLabel("GO") != FoldLabel("go") || FoldLabel("Go") != "go" {
		t.Fatal("FoldLabel does not fold case")
	}
}

func TestIsSelfFoldsTheWaysAModelNamesTheUser(t *testing.T) {
	for _, alias := range []string{"the user", "The User", "I", "me", "  myself  ", "you", "User."} {
		if !IsSelf(alias) {
			t.Fatalf("IsSelf(%q) = false; it would become a second person node", alias)
		}
	}
	for _, other := range []string{"Alice", "the user's flatmate", "Go", ""} {
		if IsSelf(other) {
			t.Fatalf("IsSelf(%q) = true", other)
		}
	}
}

// The word-boundary test is what makes the mention scan usable: without it a
// two-letter node fires on every second message.
func TestMentionsRequiresAWordBoundary(t *testing.T) {
	for _, tc := range []struct {
		text, label string
		want        bool
	}{
		{"I am learning Go this term", "Go", true},
		{"go is what I picked", "Go", true},
		{"I am going to the shop", "Go", false},
		{"I use Django at work", "Go", false},
		{"the algorithm is slow", "Go", false},
		{"Golang is different", "Go", false},
		{"Go's runtime", "Go", true},
		{"(Go)", "Go", true},
		{"Go.", "Go", true},
		{"how is the backend project going?", "the backend project", true},
		{"nothing here", "Go", false},
		// Non-ASCII on both sides of the match must still count as boundaries
		// or not, by what the rune is rather than by its byte width.
		{"讲 Go 语言", "Go", true},
		{"café", "caf", false},
		// Under the floor: a one-character label would match everywhere.
		{"I use R for stats", "R", false},
	} {
		if got := Mentions(tc.text, tc.label); got != tc.want {
			t.Fatalf("Mentions(%q, %q) = %t, want %t", tc.text, tc.label, got, tc.want)
		}
	}
}

// A first occurrence that fails the boundary test must not hide a later one
// that passes -- the scan advances by a byte, not by the match.
func TestMentionsFindsALaterOccurrence(t *testing.T) {
	if !Mentions("I was going to say: Go is the one", "Go") {
		t.Fatal("the scan stopped at the first substring hit inside another word")
	}
}
