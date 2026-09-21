package study

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/jashveer/lifeos/backend/internal/httpx"
)

// QuizRoutes returns the subtree mounted at /quizzes. It assumes
// auth.RequireAuth is already in front of it.
//
// There is no POST that *generates* a quiz, for the reason there is no
// endpoint that generates flashcards: generation costs a model call and
// produces content the user has to read before it is theirs, which is what the
// approval flow is for. POST here takes a quiz that has already been written
// -- it is what an approved `generate_quiz` runs through -- and there is no
// PATCH at all: a quiz with a question added or changed is a different quiz
// from the one somebody has already sat, and the attempts at the old one would
// silently be scored against the new total.
func (h *Handler) QuizRoutes() chi.Router {
	r := chi.NewRouter()
	r.Get("/", h.ListQuizzes)
	r.Post("/", h.CreateQuiz)
	r.Get("/{id}", h.GetQuiz)
	r.Delete("/{id}", h.DeleteQuiz)
	r.Post("/{id}/attempts", h.StartAttempt)
	return r
}

// AttemptRoutes returns the subtree mounted at /quiz-attempts.
//
// An attempt is addressed at the top level rather than under its quiz because
// that is how it is used: a client holds one attempt id and posts answers to
// it. Every route here is the user working on their own attempt, and none of
// them is reachable by a tool -- see the package comment on quiz_service.go.
func (h *Handler) AttemptRoutes() chi.Router {
	r := chi.NewRouter()
	r.Get("/{id}", h.GetAttempt)
	r.Post("/{id}/answers", h.SubmitAnswer)
	r.Post("/{id}/complete", h.CompleteAttempt)
	return r
}

// --- wire types -------------------------------------------------------------

type quizRequest struct {
	Title       string            `json:"title"`
	StudyPlanID *uuid.UUID        `json:"study_plan_id"`
	DocumentID  *uuid.UUID        `json:"document_id"`
	Questions   []questionRequest `json:"questions"`
}

type questionRequest struct {
	Question     string   `json:"question"`
	Options      []string `json:"options"`
	CorrectIndex int      `json:"correct_index"`
	Topic        string   `json:"topic"`
}

type quizResponse struct {
	ID string `json:"id"`
	// StudyPlan and Document are the titles beside the ids, so a client
	// renders a row without a second request. Both halves of each pair are
	// null when there is no link, and the document pair becomes null when the
	// document is deleted.
	StudyPlanID   *string `json:"study_plan_id"`
	StudyPlan     *string `json:"study_plan"`
	DocumentID    *string `json:"document_id"`
	Document      *string `json:"document"`
	Title         string  `json:"title"`
	QuestionCount int     `json:"question_count"`
	AttemptCount  int     `json:"attempt_count"`
	// BestScore is the highest score over the caller's finished attempts, or
	// null if they have not finished one. Read against question_count.
	BestScore *int   `json:"best_score"`
	CreatedAt string `json:"created_at"`
	// Questions is present on a single quiz and omitted from a list.
	Questions []questionResponse `json:"questions,omitempty"`
}

// questionResponse is a question as it is *shown*, and the field it does not
// have is the point of the type.
//
// There is no correct_index here. The owner is not being kept from their own
// data -- the answer comes back the moment they answer the question, and the
// whole attempt reads back with every correct index on it -- but a quiz whose
// GET hands the client the answer key is a quiz that any client will
// accidentally spoil, and the resource exists to be answered rather than read.
// See docs/decisions.md.
type questionResponse struct {
	ID       string   `json:"id"`
	Question string   `json:"question"`
	Options  []string `json:"options"`
	Topic    *string  `json:"topic"`
}

type quizListResponse struct {
	Quizzes []quizResponse `json:"quizzes"`
	Count   int            `json:"count"`
	Limit   int            `json:"limit"`
	Offset  int            `json:"offset"`
}

type answerRequest struct {
	QuestionID    uuid.UUID `json:"question_id"`
	SelectedIndex int       `json:"selected_index"`
}

// answerResponse carries the verdict and the correct index, so answering a
// question tells the user how they did. It is where the answer key is
// revealed, one question at a time and only once it has been answered.
type answerResponse struct {
	ID            string   `json:"id"`
	QuestionID    string   `json:"question_id"`
	Question      string   `json:"question"`
	Options       []string `json:"options"`
	SelectedIndex int      `json:"selected_index"`
	CorrectIndex  int      `json:"correct_index"`
	Correct       bool     `json:"correct"`
	CreatedAt     string   `json:"created_at"`
}

type attemptResponse struct {
	ID     string `json:"id"`
	QuizID string `json:"quiz_id"`
	Quiz   string `json:"quiz"`
	// QuestionCount is the quiz's, so Score is legible: "4" means nothing and
	// "4 of 6" does.
	QuestionCount int    `json:"question_count"`
	StartedAt     string `json:"started_at"`
	// CompletedAt and Score are null together until the attempt is completed.
	CompletedAt *string          `json:"completed_at"`
	Score       *int             `json:"score"`
	Answers     []answerResponse `json:"answers"`
}

func toQuizResponse(q Quiz, withQuestions bool) quizResponse {
	out := quizResponse{
		ID: q.ID.String(), StudyPlan: q.StudyPlanTitle, Document: q.DocumentName,
		Title: q.Title, QuestionCount: q.QuestionCount, AttemptCount: q.AttemptCount,
		BestScore: q.BestScore,
		CreatedAt: q.CreatedAt.UTC().Format(httpx.TimeFormat),
	}
	if q.StudyPlanID != nil {
		id := q.StudyPlanID.String()
		out.StudyPlanID = &id
	}
	if q.DocumentID != nil {
		id := q.DocumentID.String()
		out.DocumentID = &id
	}
	if withQuestions {
		out.Questions = make([]questionResponse, 0, len(q.Questions))
		for _, question := range q.Questions {
			out.Questions = append(out.Questions, questionResponse{
				ID: question.ID.String(), Question: question.Question,
				Options: question.Options, Topic: question.Topic,
			})
		}
	}
	return out
}

func toAnswerResponse(a Answer) answerResponse {
	return answerResponse{
		ID: a.ID.String(), QuestionID: a.QuestionID.String(), Question: a.Question,
		Options: a.Options, SelectedIndex: a.SelectedIndex, CorrectIndex: a.CorrectIndex,
		Correct: a.Correct, CreatedAt: a.CreatedAt.UTC().Format(httpx.TimeFormat),
	}
}

func toAttemptResponse(a Attempt) attemptResponse {
	out := attemptResponse{
		ID: a.ID.String(), QuizID: a.QuizID.String(), Quiz: a.QuizTitle,
		QuestionCount: a.QuestionCount, Score: a.Score,
		StartedAt: a.StartedAt.UTC().Format(httpx.TimeFormat),
		Answers:   make([]answerResponse, 0, len(a.Answers)),
	}
	if a.CompletedAt != nil {
		at := a.CompletedAt.UTC().Format(httpx.TimeFormat)
		out.CompletedAt = &at
	}
	for _, ans := range a.Answers {
		out.Answers = append(out.Answers, toAnswerResponse(ans))
	}
	return out
}

// --- quiz handlers ------------------------------------------------------------

// ListQuizzes handles GET /api/v1/quizzes?q=&study_plan_id=&document_id=.
func (h *Handler) ListQuizzes(w http.ResponseWriter, r *http.Request) {
	userID, ok := httpx.UserID(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized", "Authentication required.")
		return
	}
	q := r.URL.Query()
	filter := QuizFilter{Query: q.Get("q"), Sort: q.Get("sort")}
	for _, link := range []struct {
		param string
		dst   **uuid.UUID
	}{{"study_plan_id", &filter.StudyPlanID}, {"document_id", &filter.DocumentID}} {
		raw := q.Get(link.param)
		if raw == "" {
			continue
		}
		id, err := uuid.Parse(raw)
		if err != nil {
			httpx.WriteError(w, http.StatusBadRequest, "validation_failed", link.param+" must be an id.")
			return
		}
		*link.dst = &id
	}
	var err error
	if filter.Limit, err = intParam(q.Get("limit")); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "validation_failed", "limit must be a whole number.")
		return
	}
	if filter.Offset, err = intParam(q.Get("offset")); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "validation_failed", "offset must be a whole number.")
		return
	}

	found, err := h.svc.Quizzes(r.Context(), userID, filter)
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	// Echo back the effective paging, which may have been clamped.
	effective, _ := ValidateQuizFilter(filter)
	out := quizListResponse{
		Quizzes: make([]quizResponse, 0, len(found)), Count: len(found),
		Limit: effective.Limit, Offset: effective.Offset,
	}
	for _, quiz := range found {
		out.Quizzes = append(out.Quizzes, toQuizResponse(quiz, false))
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// CreateQuiz handles POST /api/v1/quizzes.
//
// It is the path an approved `generate_quiz` writes through, and it is open to
// a client writing its own questions on the same terms the hand-written
// flashcard endpoint is: the user is the one asking.
func (h *Handler) CreateQuiz(w http.ResponseWriter, r *http.Request) {
	userID, ok := httpx.UserID(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized", "Authentication required.")
		return
	}
	var req quizRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	in := CreateQuizInput{
		Title: req.Title, StudyPlanID: req.StudyPlanID, DocumentID: req.DocumentID,
		Questions: make([]NewQuestion, 0, len(req.Questions)),
	}
	for _, q := range req.Questions {
		in.Questions = append(in.Questions, NewQuestion{
			Question: q.Question, Options: q.Options,
			CorrectIndex: q.CorrectIndex, Topic: q.Topic,
		})
	}
	quiz, err := h.svc.CreateQuiz(r.Context(), userID, in)
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, toQuizResponse(quiz, true))
}

// GetQuiz handles GET /api/v1/quizzes/{id}: the quiz and its questions, with
// no answer key on them. See questionResponse.
func (h *Handler) GetQuiz(w http.ResponseWriter, r *http.Request) {
	userID, id, ok := h.scope(w, r, "quiz")
	if !ok {
		return
	}
	quiz, err := h.svc.Quiz(r.Context(), userID, id)
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, toQuizResponse(quiz, true))
}

// DeleteQuiz handles DELETE /api/v1/quizzes/{id}. Its questions, its attempts
// and their answers go with it.
func (h *Handler) DeleteQuiz(w http.ResponseWriter, r *http.Request) {
	userID, id, ok := h.scope(w, r, "quiz")
	if !ok {
		return
	}
	if err := h.svc.DeleteQuiz(r.Context(), userID, id); err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- attempt handlers ---------------------------------------------------------

// StartAttempt handles POST /api/v1/quizzes/{id}/attempts. It takes no body:
// starting an attempt is the whole request.
func (h *Handler) StartAttempt(w http.ResponseWriter, r *http.Request) {
	userID, quizID, ok := h.scope(w, r, "quiz")
	if !ok {
		return
	}
	attempt, err := h.svc.StartAttempt(r.Context(), userID, quizID)
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, toAttemptResponse(attempt))
}

// GetAttempt handles GET /api/v1/quiz-attempts/{id}.
func (h *Handler) GetAttempt(w http.ResponseWriter, r *http.Request) {
	userID, id, ok := h.scope(w, r, "quiz attempt")
	if !ok {
		return
	}
	attempt, err := h.svc.Attempt(r.Context(), userID, id)
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, toAttemptResponse(attempt))
}

// SubmitAnswer handles POST /api/v1/quiz-attempts/{id}/answers.
//
// Direct, with no approval in front of it: the user is answering their own
// question. The response carries the verdict and the correct option, which is
// where the answer key is revealed -- one question at a time, and only once it
// has been answered.
func (h *Handler) SubmitAnswer(w http.ResponseWriter, r *http.Request) {
	userID, attemptID, ok := h.scope(w, r, "quiz attempt")
	if !ok {
		return
	}
	var req answerRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	answer, err := h.svc.SubmitAnswer(r.Context(), userID, attemptID, AnswerInput{
		QuestionID: req.QuestionID, SelectedIndex: req.SelectedIndex,
	})
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, toAnswerResponse(answer))
}

// CompleteAttempt handles POST /api/v1/quiz-attempts/{id}/complete. It takes
// no body, and answers 409 for an attempt that is already finished.
func (h *Handler) CompleteAttempt(w http.ResponseWriter, r *http.Request) {
	userID, id, ok := h.scope(w, r, "quiz attempt")
	if !ok {
		return
	}
	attempt, err := h.svc.CompleteAttempt(r.Context(), userID, id)
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, toAttemptResponse(attempt))
}
