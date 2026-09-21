package study

import "testing"

// The grounding rule for a generated question, which is the claim Phase 10b
// makes: the *correct answer* has to be in the document. These tests are
// written against the two sentences in quizPassages and nothing else, so each
// one says which words it turns on.
var quizPassages = []string{
	"The mast feed is switched to the auxiliary dipole whenever the standing wave ratio " +
		"exceeds 2.4, and the changeover takes eleven seconds to complete.",
	"The battery bank is a set of six cells wired in series, and the whole bank is " +
		"equalised every forty days.",
}

func TestQuestionGroundedKeepsWhatTheDocumentAnswers(t *testing.T) {
	for name, q := range map[string]NewQuestion{
		"the answer in the document's own words": {
			Question:     "How long does the changeover to the auxiliary dipole take?",
			Options:      []string{"Eleven seconds", "Two minutes", "Half an hour", "It is instant"},
			CorrectIndex: 0,
		},
		"a figure copied exactly": {
			Question:     "Above what standing wave ratio is the mast feed switched?",
			Options:      []string{"1.1", "2.4", "9.6", "It is never switched"},
			CorrectIndex: 1,
		},
		"an answer written as a sentence, mostly the document's words": {
			Question:     "How often is the battery bank equalised?",
			Options:      []string{"Every forty days", "Once a year", "Never", "Twice a week"},
			CorrectIndex: 0,
		},
	} {
		t.Run(name, func(t *testing.T) {
			if !QuestionGrounded(q, quizPassages) {
				t.Fatalf("dropped a grounded question: %q -> %q (words: %v)",
					q.Question, q.CorrectAnswer(), SignificantWords(q.CorrectAnswer()))
			}
		})
	}
}

func TestQuestionGroundedDropsWhatTheDocumentDoesNotAnswer(t *testing.T) {
	for name, q := range map[string]NewQuestion{
		"an invented answer": {
			Question:     "What is the station's call sign?",
			Options:      []string{"VP8ROT", "ZS6BKW", "G3TXQ", "There is none"},
			CorrectIndex: 0,
		},
		// The failure the whole check exists for, in its sharpest form: a
		// figure the document does not contain, in an answer that is otherwise
		// entirely the document's words.
		"a number the document does not give": {
			Question:     "How long does the changeover take?",
			Options:      []string{"Eleven seconds", "Ninety seconds", "A minute", "Four hours"},
			CorrectIndex: 1,
		},
		"an answer with nothing in it": {
			Question:     "How long does the changeover take?",
			Options:      []string{"It depends.", "Eleven seconds"},
			CorrectIndex: 0,
		},
		// The weaker test the question itself has to pass: this answer is in
		// the document and the question is about something else entirely.
		"a question about another subject": {
			Question:     "How is chlorophyll involved in photosynthesis?",
			Options:      []string{"Eleven seconds", "Six cells"},
			CorrectIndex: 0,
		},
		// The backstop: validation catches this properly, with a field name.
		"a correct index pointing at no option": {
			Question:     "How long does the changeover take?",
			Options:      []string{"Eleven seconds"},
			CorrectIndex: 4,
		},
	} {
		t.Run(name, func(t *testing.T) {
			if QuestionGrounded(q, quizPassages) {
				t.Fatalf("kept an ungrounded question: %q -> %q", q.Question, q.CorrectAnswer())
			}
		})
	}
}

// The distractors are not checked, and this says so in the one way that
// matters: the same question passes with every wrong option replaced by
// something the document has never heard of.
func TestOnlyTheCorrectAnswerIsChecked(t *testing.T) {
	q := NewQuestion{
		Question:     "How many cells are in the battery bank?",
		Options:      []string{"Six", "A colony of penguins", "Thirty-one kilograms of flour", "Tuesday"},
		CorrectIndex: 0,
	}
	if !QuestionGrounded(q, quizPassages) {
		t.Fatal("a question with wild distractors was dropped; a distractor is supposed to be wrong")
	}
	// And moving the answer key onto one of those is what the check catches.
	q.CorrectIndex = 3
	if QuestionGrounded(q, quizPassages) {
		t.Fatal("the check passed a question whose correct answer is 'Tuesday'")
	}
}

// A question is held to exactly the same two tests a flashcard is, with the
// correct option standing in for the back. This pins that rather than leaving
// it to the reader: if CardGrounded's rule changes, this fails.
func TestAQuestionIsJudgedLikeACard(t *testing.T) {
	q := NewQuestion{
		Question:     "How long does the changeover take?",
		Options:      []string{"Two minutes", "Eleven seconds"},
		CorrectIndex: 1,
	}
	card := NewCard{Front: q.Question, Back: q.CorrectAnswer()}
	if QuestionGrounded(q, quizPassages) != CardGrounded(card, quizPassages) {
		t.Fatal("a question and the same card as a front/back pair were judged differently")
	}
}

func TestNoPassagesGroundsNoQuestion(t *testing.T) {
	q := NewQuestion{
		Question: "How long does the changeover take?",
		Options:  []string{"Eleven seconds", "Two minutes"},
	}
	for _, passages := range [][]string{nil, {}, {""}} {
		if QuestionGrounded(q, passages) {
			t.Fatalf("grounded against %v", passages)
		}
	}
}

// The same rule, at the level a quiz is judged: an answer key that contradicts
// the document's figure is not grounded, even when every other word of it is
// the document's.
//
// This is the failure that was measured -- see
// TestAFigureWrittenInWordsMustMatchExactly -- and it is worse in a quiz than
// in a deck, because the option the model marked right was sitting in the same
// list as the one that actually is.
func TestAnAnswerKeyThatContradictsAFigureIsDropped(t *testing.T) {
	passages := []string{
		"The battery bank is a set of six cells wired in series, and the whole " +
			"bank is equalised every forty days.",
	}
	q := NewQuestion{
		Question:     "How often is the whole battery bank equalised?",
		Options:      []string{"Every five days", "Every forty days", "Every sixty days", "Every ninety days"},
		CorrectIndex: 2,
	}
	if QuestionGrounded(q, passages) {
		t.Fatalf("kept a question whose answer key is %q against a document that says forty",
			q.CorrectAnswer())
	}
	// The same question with the key on the option the document supports is
	// kept -- so this is the figure being checked, not the question being
	// rejected for some other reason.
	q.CorrectIndex = 1
	if !QuestionGrounded(q, passages) {
		t.Fatalf("dropped a question whose answer key is %q, which the document states",
			q.CorrectAnswer())
	}
}
