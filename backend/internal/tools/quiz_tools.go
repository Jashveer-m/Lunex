package tools

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/jashveer/lifeos/backend/internal/study"
)

// Phase 10b's two tools: one read, one write.
//
// There is no third. Taking a quiz -- starting an attempt, answering a
// question, finishing it -- is not a tool and is not reachable from here,
// because it is the user answering their own questions rather than the
// assistant proposing a change to their data. tools.StudyService names no
// method for it, so no tool can reach one even by mistake; the endpoints are
// in study.Handler.AttemptRoutes. See docs/decisions.md.

// quizRecord is a quiz as a tool reports it.
//
// What it does not carry is deliberate and is the same rule flashcards follow:
// no questions, no options and no answers. A quiz the assistant can quote back
// is a quiz it can spoil, and the thing the user wants from a read is which
// quizzes exist and how they went.
type quizRecord struct {
	ID            string  `json:"id"`
	Title         string  `json:"title"`
	StudyPlan     *string `json:"study_plan"`
	Document      *string `json:"document"`
	QuestionCount int     `json:"question_count"`
	AttemptCount  int     `json:"attempt_count"`
	BestScore     *int    `json:"best_score"`
}

func toQuizRecord(q study.Quiz) quizRecord {
	return quizRecord{
		ID: q.ID.String(), Title: q.Title, StudyPlan: q.StudyPlanTitle,
		Document: q.DocumentName, QuestionCount: q.QuestionCount,
		AttemptCount: q.AttemptCount, BestScore: q.BestScore,
	}
}

var quizRecordSchema = object(map[string]Schema{
	"id":             uuidField("the quiz's id"),
	"title":          str("what the quiz is called"),
	"study_plan":     str("the study plan it is filed under, or null"),
	"document":       str("the document it was made from, or null"),
	"question_count": integer("how many questions it has"),
	"attempt_count":  integer("how many times the user has taken it"),
	"best_score":     integer("the best score over their finished attempts, out of question_count, or null"),
}, "id", "title", "study_plan", "document", "question_count", "attempt_count", "best_score")

// --- search_quizzes -------------------------------------------------------------

type searchQuizzesInput struct {
	Query string `json:"query,omitempty"`
}

func searchQuizzesTool(s Services) Tool {
	return define(Tool{
		Name: SearchQuizzes,
		Description: "Look at the quizzes the user has made from their documents: what each one is on, " +
			"how many questions it has, how many times they have taken it and their best score. " +
			"It does not show the questions or the answers.",
		Permission: Read,
		Params: []Param{
			{Name: "query", Type: "string", Description: "a word or phrase from the quiz's title, if the user gave one"},
		},
		Output: object(map[string]Schema{
			"count":   integer("how many quizzes are listed"),
			"more":    boolean("whether more quizzes matched than are listed"),
			"quizzes": listOf(quizRecordSchema),
		}, "count", "more", "quizzes"),
	},
		func(_ context.Context, _ uuid.UUID, a Args) (searchQuizzesInput, error) {
			return searchQuizzesInput{
				Query: a.String("query", "q", "search", "text", "keyword", "topic", "subject", "quiz"),
			}, nil
		},
		func(ctx context.Context, userID uuid.UUID, in searchQuizzesInput) (Result, error) {
			found, more, err := search(in.Query, func(term string, limit int) ([]study.Quiz, error) {
				return s.Study.Quizzes(ctx, userID, study.QuizFilter{
					Query: term, Sort: study.DefaultQuizSort, Limit: limit,
				})
			}, func(q study.Quiz) uuid.UUID { return q.ID })
			if err != nil {
				return Result{}, fmt.Errorf("search quizzes: %w", err)
			}
			records := make([]quizRecord, 0, len(found))
			for _, q := range found {
				records = append(records, toQuizRecord(q))
			}
			return Result{
				Output:  map[string]any{"count": len(records), "more": more, "quizzes": records},
				Quizzes: found, More: more,
			}, nil
		},
		func(in searchQuizzesInput) string {
			out := "Look at the quizzes"
			if in.Query != "" {
				out += " matching " + quoted(in.Query)
			}
			return out + "."
		},
	)
}

// --- generate_quiz ---------------------------------------------------------------

// quizQuestionInput is one proposed question in the canonical input.
type quizQuestionInput struct {
	Question     string   `json:"question"`
	Options      []string `json:"options"`
	CorrectIndex int      `json:"correct_index"`
	Topic        string   `json:"topic,omitempty"`
}

// generateQuizInput is the canonical proposal, and the questions are *in* it.
//
// It is generateFlashcardsInput's design decision applied a second time, and
// everything that comment says holds here: the model writes the questions
// while the call is being prepared, they are stored in the input, and
// approving the action writes exactly those rows. Storing the document and a
// count instead would mean the user approved "6 questions from
// relay-handbook.txt" and got six questions nobody had read -- and for a quiz
// that is worse than for a deck, because a wrong answer key does not merely
// teach the user something false, it marks them wrong for knowing better.
//
// It also makes the proposal reproducible: the same input always writes the
// same quiz, with the options in the same order, so an approval is not a
// second roll of the dice. That is why nothing shuffles the options between
// here and the database.
type generateQuizInput struct {
	DocumentID  uuid.UUID           `json:"document_id"`
	Document    string              `json:"document"`
	StudyPlanID *uuid.UUID          `json:"study_plan_id,omitempty"`
	StudyPlan   string              `json:"study_plan,omitempty"`
	Topic       string              `json:"topic,omitempty"`
	Title       string              `json:"title"`
	Questions   []quizQuestionInput `json:"questions"`
}

func generateQuizTool(s Services) Tool {
	return define(Tool{
		Name: GenerateQuiz,
		Description: "Propose a multiple-choice quiz made from one of the user's uploaded documents. " +
			"The questions and the answers are written from the document's own text and shown to the user; " +
			"the quiz is only saved after the user approves it. This does not take the quiz -- the user does that themselves.",
		Permission: Write,
		Params: []Param{
			{Name: "document", Type: "string", Required: true, Aliases: documentAliases,
				Description: "the uploaded document to make the quiz from, by filename"},
			{Name: "document_id", Type: "string", Description: "the document's id, if it is known exactly"},
			{Name: "study_plan", Type: "string", Aliases: studyPlanAliases,
				Description: "the study plan to file the quiz under, by title, if the user named one"},
			{Name: "study_plan_id", Type: "string", Description: "the study plan's id, if it is known exactly"},
			{Name: "topic", Type: "string", Aliases: []string{"about", "subject", "chapter", "section"},
				Description: "the part of the document to cover, if the user named one"},
			{Name: "count", Type: "string", Aliases: []string{"how_many", "number", "questions"},
				Description: "how many questions the user asked for, if they said a number"},
			{Name: "title", Type: "string", Aliases: []string{"name", "quiz_title", "called"},
				Description: "what to call the quiz, if the user said"},
			// Not offered to the model: written by the tool from the document.
			// See Param.Derived and the type comment above.
			{Name: "questions", Type: "array", Derived: true,
				Description: "the generated questions, written from the document while this call was prepared"},
		},
		Output: object(map[string]Schema{
			"quiz":       str("what the quiz is called"),
			"quiz_id":    uuidField("the created quiz's id"),
			"document":   str("the document the questions were made from"),
			"study_plan": str("the plan it was filed under, or null"),
			"count":      integer("how many questions the quiz has"),
			// The questions and nothing else. The options and the correct
			// answers are deliberately left out of the recorded result: it is
			// read back into later prompts, and a quiz the assistant can
			// recite the answers to is a quiz it can spoil.
			"questions": listOf(str("one question, without its options or its answer")),
		}, "quiz", "quiz_id", "document", "study_plan", "count", "questions"),
	},
		func(ctx context.Context, userID uuid.UUID, a Args) (generateQuizInput, error) {
			var in generateQuizInput
			ref := a.String(append([]string{"document", "document_id"}, documentAliases...)...)
			if ref == "" {
				return in, invalid(GenerateQuiz, "say which uploaded document to make the quiz from")
			}
			doc, err := resolveDocument(ctx, s, userID, GenerateQuiz, ref)
			if err != nil {
				return in, err
			}
			in.DocumentID, in.Document = doc.ID, doc.Filename

			gen := study.GenerateQuizInput{
				DocumentID: doc.ID,
				Topic:      a.String("topic", "about", "subject", "chapter", "section"),
				Count:      questionCount(a),
				Title:      a.String("title", "name", "quiz_title", "called"),
			}
			if planRef := a.String(append([]string{"study_plan", "study_plan_id"}, studyPlanAliases...)...); planRef != "" {
				plan, err := resolveStudyPlan(ctx, s, userID, GenerateQuiz, planRef)
				if err != nil {
					return in, err
				}
				gen.StudyPlanID, in.StudyPlanID, in.StudyPlan = &plan.ID, &plan.ID, plan.Title
			}
			in.Topic = gen.Topic

			// The model call. It happens here, during Prepare, which is
			// allowed to read and never writes: nothing is stored by this, and
			// what comes back is a proposal the user has still to approve.
			proposal, err := s.Study.ProposeQuiz(ctx, userID, gen)
			switch {
			case errors.Is(err, study.ErrNotStudyable):
				return in, invalid(GenerateQuiz,
					"%s has no text that can be used yet: tell the user to check it finished processing",
					quoted(doc.Filename))
			case errors.Is(err, study.ErrNoPassages):
				return in, invalid(GenerateQuiz,
					"nothing in %s is about %s: ask the user which part of it they mean",
					quoted(doc.Filename), quoted(gen.Topic))
			case errors.Is(err, study.ErrGeneration):
				// The model wrote nothing usable, or no answer it proposed was
				// actually in the document. Either way there is no proposal to
				// show, and saying so is better than proposing a quiz whose
				// answer key is invented -- which is the failure the grounding
				// check exists to prevent.
				return in, invalid(GenerateQuiz,
					"could not write quiz questions from %s that its own text answers: "+
						"tell the user the document does not have enough on this to make a quiz from",
					quoted(doc.Filename))
			case err != nil:
				return in, fieldProblems(GenerateQuiz, err)
			}
			in.Title = proposal.Title
			for _, q := range proposal.Questions {
				in.Questions = append(in.Questions, quizQuestionInput{
					Question: q.Question, Options: q.Options,
					CorrectIndex: q.CorrectIndex, Topic: q.Topic,
				})
			}
			return in, nil
		},
		func(ctx context.Context, userID uuid.UUID, in generateQuizInput) (Result, error) {
			questions := make([]study.NewQuestion, 0, len(in.Questions))
			for _, q := range in.Questions {
				questions = append(questions, study.NewQuestion{
					Question: q.Question, Options: q.Options,
					CorrectIndex: q.CorrectIndex, Topic: q.Topic,
				})
			}
			docID := in.DocumentID
			quiz, err := s.Study.CreateQuiz(ctx, userID, study.CreateQuizInput{
				StudyPlanID: in.StudyPlanID, DocumentID: &docID,
				Title: in.Title, Questions: questions,
			})
			if err != nil {
				// The document or the plan existed when this was proposed. If
				// one does not now, the approved write fails and is recorded
				// as having failed, rather than quietly filing a quiz under
				// nothing the user chose.
				return Result{}, fmt.Errorf("save the quiz made from %q: %w", in.Document, err)
			}
			asked := make([]string, 0, len(quiz.Questions))
			for _, q := range quiz.Questions {
				asked = append(asked, q.Question)
			}
			var plan any
			if in.StudyPlan != "" {
				plan = in.StudyPlan
			}
			return Result{
				Output: map[string]any{
					"quiz": quiz.Title, "quiz_id": quiz.ID.String(), "document": in.Document,
					"study_plan": plan, "count": len(asked), "questions": asked,
				},
				Quizzes: []study.Quiz{quiz},
			}, nil
		},
		func(in generateQuizInput) string {
			// The questions themselves, with their options and the answer
			// marked -- not "create a 6-question quiz". The whole point of
			// generating them before the approval is that the user reads what
			// they are approving, and for a quiz that has to include which
			// option is going to be marked right: an answer key the user never
			// saw is the one thing here they cannot correct afterwards.
			var b strings.Builder
			b.WriteString("Save a quiz " + quoted(in.Title))
			// The default title *is* the document's name, so naming the file
			// again would read as "a quiz "relay-handbook.txt" made from
			// "relay-handbook.txt"". It is still said whenever the title does
			// not already say it, because where the questions came from is
			// half of what is being approved.
			if !strings.Contains(in.Title, in.Document) {
				b.WriteString(" made from " + quoted(in.Document))
			}
			if in.Topic != "" {
				b.WriteString(" about " + quoted(in.Topic))
			}
			if in.StudyPlan != "" {
				b.WriteString(" under " + quoted(in.StudyPlan))
			}
			fmt.Fprintf(&b, ", %s:", plural(len(in.Questions), "question"))
			for i, q := range in.Questions {
				fmt.Fprintf(&b, "\n%d. %s", i+1, q.Question)
				for j, o := range q.Options {
					mark := ""
					if j == q.CorrectIndex {
						mark = " (correct)"
					}
					fmt.Fprintf(&b, "\n   - %s%s", o, mark)
				}
			}
			// A closing sentence rather than a trailing full stop on the last
			// option: every summary in the registry ends in one, and appending
			// it to "A new fuel filter (correct)" would read as part of the
			// option. It also says what the marker means, which is worth a
			// line on the card where the answer key is being approved.
			b.WriteString("\nThe option marked (correct) is the one the quiz will mark right.")
			return endSentence(b.String())
		},
	)
}

// questionCount reads how many questions the user asked for. Anything
// unreadable is "they did not say", which study.ValidateGenerateQuiz turns
// into the default -- there is no sense in refusing a whole generation because
// a model wrote "a few".
func questionCount(a Args) int {
	return countArg(a, "count", "how_many", "number", "questions", "n")
}
