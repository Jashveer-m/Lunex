package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jashveer/lifeos/backend/internal/study"
)

func TestGetWeakTopicsReportsTheServiceFiguresWithoutApproval(t *testing.T) {
	w := newWorld()
	w.study.weak[w.user] = []study.TopicStat{{
		Topic: "mast feed timing", Answers: 5, Correct: 1, Quizzes: 2,
		StudyPlans: []string{"Kestrel relay handbook"}, Documents: []string{"relay-handbook.txt"},
		LastAnswered: time.Date(2026, 9, 30, 10, 0, 0, 0, time.UTC),
	}}

	call, err := w.reg.Prepare(context.Background(), w.user, GetWeakTopics, Args{})
	if err != nil {
		t.Fatal(err)
	}
	if call.Permission != Read {
		t.Fatalf("permission = %q", call.Permission)
	}
	result, err := w.reg.RunRead(context.Background(), w.user, call)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.WeakTopics) != 1 || result.Count() != 1 {
		t.Fatalf("result = %+v", result)
	}
	raw, err := json.Marshal(result.Output)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"topic":"mast feed timing"`, `"answers":5`, `"correct":1`,
		`"correct_percent":20`, `"min_answers":3`, `"max_correct_percent":60`, `"last_answered":"2026-09-30"`} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("output %s is missing %s", raw, want)
		}
	}
	if w.totalWrites() != 0 {
		t.Fatal("a read wrote something")
	}
}

// A model that invents an argument gets the same canonical input as one that
// did not: the tool takes none, so there is no topic to guess.
func TestGetWeakTopicsIgnoresInventedArguments(t *testing.T) {
	w := newWorld()
	call, err := w.reg.Prepare(context.Background(), w.user, GetWeakTopics, Args{"topic": "calculus"})
	if err != nil {
		t.Fatal(err)
	}
	if string(call.Input) != "{}" {
		t.Fatalf("input = %s, want {}", call.Input)
	}
}

func TestGetWeakTopicsIsTheCallersOwn(t *testing.T) {
	w := newWorld()
	other := uuid.New()
	w.study.weak[other] = []study.TopicStat{{Topic: "someone else's topic", Answers: 4}}

	call, err := w.reg.Prepare(context.Background(), w.user, GetWeakTopics, Args{})
	if err != nil {
		t.Fatal(err)
	}
	result, err := w.reg.RunRead(context.Background(), w.user, call)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.WeakTopics) != 0 {
		t.Fatalf("the caller sees another user's weak topics: %+v", result.WeakTopics)
	}
	raw, _ := json.Marshal(result.Output)
	if !strings.Contains(string(raw), `"weak_topics":[]`) {
		t.Fatalf("an empty result is not an empty list: %s", raw)
	}
}
