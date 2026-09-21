package study

import "testing"

// The passages every case below is judged against. They are the same two the
// service tests use, so "grounded" means the same thing in both files.
var passages = []string{auroraPassage, generatorPassage}

// The rule, stated as cases: an answer is kept when its substance is the
// document's, and dropped when it is not -- including when it is a true
// sentence about the world that the document happens not to say.
func TestGroundedInKeepsWhatTheDocumentSays(t *testing.T) {
	for name, tc := range map[string]struct {
		text string
		want bool
	}{
		"the document's own words":       {"About forty minutes.", true},
		"reworded around the same facts": {"The aurora lasted forty minutes over the tundra.", true},
		"an inflection of a document word": {
			"The guy-line on the north side has gone slack.", true},
		"a word of framing the model added": {
			"The generator needs a new fuel filter fitted.", true},
		// An aside is not framing: "which is fairly typical" is a claim the
		// document does not make, and a card is the wrong place to smuggle one
		// in. Dropping this is the rule working, not the rule misfiring.
		"an invented aside on a true answer": {
			"It lasted about forty minutes, which is fairly typical for the season.", false},

		"true of the world, absent from the document": {
			"Ulaanbaatar is the capital of Mongolia.", false},
		"plausible and invented": {
			"Marine diesel, at nine litres an hour.", false},
		"a confident non-answer": {"It depends on the conditions.", false},
		"nothing at all":         {"", false},
		"only function words":    {"It is the one that was.", false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := GroundedIn(tc.text, passages); got != tc.want {
				t.Fatalf("GroundedIn(%q) = %v, want %v (words: %v)",
					tc.text, got, tc.want, SignificantWords(tc.text))
			}
		})
	}
}

// A number is held to an exact match, unlike a word, because a near-miss on a
// number is the failure this whole file exists to catch: "400 metres" from a
// passage that says "40 metres" is a prefix, and it is wrong.
func TestANumberMustMatchExactly(t *testing.T) {
	source := []string{"The relay trips at 40 volts after 12 seconds."}
	for text, want := range map[string]bool{
		"40 volts after 12 seconds":  true,
		"400 volts after 12 seconds": false,
		"40 volts after 120 seconds": false,
	} {
		if got := GroundedIn(text, source); got != want {
			t.Fatalf("GroundedIn(%q) = %v, want %v", text, got, want)
		}
	}
}

// Short answers get no proportion: with one or two content words there is
// nothing to take two thirds of, so every word has to be in the document.
func TestAShortAnswerMustBeWhollyInTheDocument(t *testing.T) {
	source := []string{"The generator needs a new fuel filter."}
	if !GroundedIn("A fuel filter.", source) {
		t.Fatal("an answer made entirely of the document's words was dropped")
	}
	if GroundedIn("A spark plug.", source) {
		t.Fatal("a two-word invention was kept")
	}
}

// The two sides of a card are held to different standards, and this is the
// case that shows why: a well-phrased question is mostly the words of asking.
func TestTheQuestionOnlyHasToNameSomethingInTheDocument(t *testing.T) {
	card := NewCard{
		Front: "According to the notes, roughly how long was the aurora visible for?",
		Back:  "About forty minutes.",
	}
	if !CardGrounded(card, passages) {
		t.Fatalf("a well-phrased question was rejected: %v", SignificantWords(card.Front))
	}
	// But a question about something else entirely is not a card about this
	// document, whatever is on the back.
	stitched := NewCard{Front: "How does photosynthesis fix carbon?", Back: "About forty minutes."}
	if CardGrounded(stitched, passages) {
		t.Fatal("a question about nothing in the document was kept")
	}

	// And a question that names nothing at all fails the same test, which is
	// the prompt's "Name the thing in the question" enforced rather than
	// requested: a card whose front is "What is it?" is unanswerable on its
	// own, which is the only way a flashcard is ever read.
	vague := NewCard{Front: "What is it used for?", Back: "A new fuel filter."}
	if CardGrounded(vague, passages) {
		t.Fatalf("a card that names nothing was kept (words: %v)", SignificantWords(vague.Front))
	}
}

// A card is refused when either side fails, and the answer is the side that
// matters: it is what the learner ends up believing.
func TestCardGroundedRequiresBothSides(t *testing.T) {
	for name, card := range map[string]NewCard{
		"invented answer": {Front: "What does the generator need?", Back: "A replacement alternator."},
		"empty answer":    {Front: "What does the generator need?", Back: ""},
		"empty question":  {Front: "", Back: "A new fuel filter."},
	} {
		t.Run(name, func(t *testing.T) {
			if CardGrounded(card, passages) {
				t.Fatalf("%+v was kept", card)
			}
		})
	}
	good := NewCard{Front: "What does the generator need?", Back: "A new fuel filter."}
	if !CardGrounded(good, passages) {
		t.Fatal("a card straight out of the document was dropped")
	}
}

// With no passages at all nothing is grounded. It is worth pinning because the
// permissive reading -- "no source, so no objection" -- is the one that would
// let a whole invented deck through on an empty document.
func TestNoPassagesGroundsNothing(t *testing.T) {
	if GroundedIn("About forty minutes.", nil) {
		t.Fatal("an answer was grounded in no passages at all")
	}
	if MentionsAny("the generator", nil) {
		t.Fatal("a question named something in no passages at all")
	}
}

func TestSignificantWordsDropsTheVocabularyOfAsking(t *testing.T) {
	got := SignificantWords("According to the passage, what is the definition of a fuel filter?")
	for _, unwanted := range []string{"according", "passage", "what", "definition", "the"} {
		for _, w := range got {
			if w == unwanted {
				t.Fatalf("%q is a content word: %v", unwanted, got)
			}
		}
	}
	if len(got) != 2 || got[0] != "fuel" || got[1] != "filter" {
		t.Fatalf("words = %v, want just the subject", got)
	}
	// A short token with a digit in it survives the length rule, because a
	// number is often the whole of an answer.
	if words := SignificantWords("40 volts"); len(words) != 2 || words[0] != "40" {
		t.Fatalf("words = %v, want the number kept", words)
	}
}

// A figure written out in words is held to exactly the rule a figure in digits
// is: matched exactly, and fatal to an answer when the passages do not contain
// it.
//
// This is a regression test for a measured failure, and the shape of it is
// worth keeping in view. Against a handbook that says "the whole bank is
// equalised every forty days", llama3.2:3b wrote a quiz question whose correct
// answer was "Every sixty days" -- and it was grounded, because "every" and
// "days" are both in the passage and two content words out of three clears
// MinGroundedShare. The rule that sinks an unsupported figure never fired,
// because "sixty" has no digit in it.
//
// For a flashcard that is a wrong fact rehearsed until it is believed. For a
// quiz it is the answer key, so it marks the learner wrong for knowing better.
func TestAFigureWrittenInWordsMustMatchExactly(t *testing.T) {
	passages := []string{
		"The battery bank is a set of six cells wired in series, and the whole " +
			"bank is equalised every forty days.",
		"The changeover takes eleven seconds to complete.",
	}
	for name, tc := range map[string]struct {
		answer string
		want   bool
	}{
		"the document's own figure":            {"Every forty days", true},
		"a different figure, in words":         {"Every sixty days", false},
		"the document's own count":             {"Six cells", true},
		"a different count":                    {"Four cells", false},
		"the document's own interval":          {"Eleven seconds", true},
		"a longer word starting the same way":  {"Fourteen seconds", false},
		"no figure at all, and well supported": {"The cells are wired in series", true},
	} {
		t.Run(name, func(t *testing.T) {
			if got := GroundedIn(tc.answer, passages); got != tc.want {
				t.Fatalf("GroundedIn(%q) = %v, want %v (words: %v)",
					tc.answer, got, tc.want, SignificantWords(tc.answer))
			}
		})
	}

	// And the prefix rule that sameWord allows for ordinary words is off for
	// figures in both directions: "four" must not match "fourteen".
	for _, pair := range [][2]string{{"four", "fourteen"}, {"six", "sixty"}, {"forty", "fortieth"}} {
		if sameWord(pair[0], pair[1]) {
			t.Fatalf("sameWord(%q, %q) = true; a figure matches only exactly", pair[0], pair[1])
		}
	}
}
