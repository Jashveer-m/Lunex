package study

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/jashveer/lifeos/backend/internal/db"
)

// The Postgres store for quizzes, their questions, and the attempts made at
// them. It is the same Repository as the plans and cards above: one type per
// module, and every statement carries `user_id = $n`.
//
// Two of the four tables have no user_id of their own -- quiz_questions and
// quiz_answers belong to a quiz and an attempt, and those belong to a user.
// Every statement below reaches them through a join that names the owner, so a
// question of somebody else's quiz is never fetched rather than fetched and
// then refused.

// --- quizzes -------------------------------------------------------------------

// quizColumns is the shared SELECT list, so the scan order cannot drift.
//
// Three of the values are correlated subqueries rather than stored columns,
// for the reason study_plans.card_count is one: a counter kept in a column is
// a counter that can be wrong, and these cannot. Each is scoped to the owner
// as well as to the quiz -- the cascade makes a foreign attempt impossible,
// and carrying the owner means the isolation holds on this statement rather
// than because of something else being true.
const quizColumns = `z.id, z.user_id, z.study_plan_id, p.title, z.document_id, d.filename,
	z.title, z.created_at,
	(SELECT count(*) FROM quiz_questions qq WHERE qq.quiz_id = z.id) AS question_count,
	(SELECT count(*) FROM quiz_attempts qa
	  WHERE qa.quiz_id = z.id AND qa.user_id = z.user_id) AS attempt_count,
	(SELECT max(qa.score) FROM quiz_attempts qa
	  WHERE qa.quiz_id = z.id AND qa.user_id = z.user_id
	    AND qa.completed_at IS NOT NULL) AS best_score`

// quizFrom is the two joins that put the plan's title and the document's name
// on every row, so a client never has to resolve an id and a deleted document
// reads back as no document rather than as a dangling one.
const quizFrom = ` FROM quizzes z
	LEFT JOIN study_plans p ON p.id = z.study_plan_id AND p.user_id = z.user_id
	LEFT JOIN documents d ON d.id = z.document_id AND d.user_id = z.user_id`

func scanQuiz(row interface{ Scan(...any) error }) (Quiz, error) {
	var q Quiz
	err := row.Scan(&q.ID, &q.UserID, &q.StudyPlanID, &q.StudyPlanTitle,
		&q.DocumentID, &q.DocumentName, &q.Title, &q.CreatedAt,
		&q.QuestionCount, &q.AttemptCount, &q.BestScore)
	return q, err
}

// CreateQuiz writes a quiz and all of its questions in one transaction.
//
// One transaction, for the reason CreateFlashcards uses one and then some: the
// user approved a quiz of six questions, and a quiz of four -- because the
// fifth insert raced a deleted plan -- is not a smaller version of what they
// approved, it is a quiz that scores out of the wrong total.
func (r *Repository) CreateQuiz(ctx context.Context, userID uuid.UUID, in CreateQuizInput) (Quiz, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return Quiz{}, fmt.Errorf("begin quiz insert: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // a committed tx rolls back to nothing

	var id uuid.UUID
	err = tx.QueryRowContext(ctx, `
		INSERT INTO quizzes (user_id, study_plan_id, document_id, title)
		VALUES ($1, $2, $3, $4)
		RETURNING id`,
		userID, in.StudyPlanID, in.DocumentID, in.Title).Scan(&id)
	if err != nil {
		return Quiz{}, fmt.Errorf("insert quiz: %w", err)
	}

	for _, q := range in.Questions {
		options, err := json.Marshal(q.Options)
		if err != nil {
			return Quiz{}, fmt.Errorf("encode quiz options: %w", err)
		}
		// clock_timestamp() rather than the transaction's now(), so the
		// questions read back in the order they were written -- which for a
		// generated quiz is the order of the document. It is the column
		// default too; it is named here so the ordering does not depend on a
		// default somebody could change.
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO quiz_questions (quiz_id, question, options, correct_index, topic, created_at)
			VALUES ($1, $2, $3, $4, $5, clock_timestamp())`,
			id, q.Question, options, q.CorrectIndex, nullString(q.Topic)); err != nil {
			return Quiz{}, fmt.Errorf("insert quiz question: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return Quiz{}, fmt.Errorf("commit quiz: %w", err)
	}
	// Read back through the joins rather than RETURNING, so a created quiz and
	// a listed one are built by the same query.
	return r.QuizByID(ctx, userID, id)
}

// QuizByID returns one quiz with its questions, in the order they were
// written.
func (r *Repository) QuizByID(ctx context.Context, userID, id uuid.UUID) (Quiz, error) {
	quiz, err := scanQuiz(r.db.QueryRowContext(ctx,
		`SELECT `+quizColumns+quizFrom+` WHERE z.id = $1 AND z.user_id = $2`, id, userID))
	if errors.Is(err, sql.ErrNoRows) {
		return Quiz{}, ErrQuizNotFound
	}
	if err != nil {
		return Quiz{}, fmt.Errorf("select quiz: %w", err)
	}
	quiz.Questions, err = r.questions(ctx, userID, id)
	if err != nil {
		return Quiz{}, err
	}
	return quiz, nil
}

// questions reads one quiz's questions. The owner is in the statement rather
// than assumed from the caller having loaded the quiz first: it is the same
// join either way, and a scoping that holds only because of what the caller
// did already is a scoping that stops holding when somebody adds a caller.
func (r *Repository) questions(ctx context.Context, userID, quizID uuid.UUID) ([]Question, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT qq.id, qq.quiz_id, qq.question, qq.options, qq.correct_index, qq.topic, qq.created_at
		FROM quiz_questions qq
		JOIN quizzes z ON z.id = qq.quiz_id
		WHERE qq.quiz_id = $1 AND z.user_id = $2
		ORDER BY qq.created_at ASC, qq.id ASC`, quizID, userID)
	if err != nil {
		return nil, fmt.Errorf("select quiz questions: %w", err)
	}
	defer rows.Close()

	out := []Question{}
	for rows.Next() {
		q, err := scanQuestion(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, q)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate quiz questions: %w", err)
	}
	return out, nil
}

func scanQuestion(row interface{ Scan(...any) error }) (Question, error) {
	var (
		q       Question
		options []byte
	)
	if err := row.Scan(&q.ID, &q.QuizID, &q.Question, &options, &q.CorrectIndex,
		&q.Topic, &q.CreatedAt); err != nil {
		return Question{}, fmt.Errorf("scan quiz question: %w", err)
	}
	if err := json.Unmarshal(options, &q.Options); err != nil {
		// The column is written by CreateQuiz from a []string and by nothing
		// else, so this is a row somebody edited by hand. Failing is right:
		// a question whose options cannot be read is one nobody can answer.
		return Question{}, fmt.Errorf("decode quiz options for %s: %w", q.ID, err)
	}
	return q, nil
}

func (r *Repository) Quizzes(ctx context.Context, userID uuid.UUID, f QuizFilter) ([]Quiz, error) {
	where := []string{"z.user_id = $1"}
	args := []any{userID}
	add := func(clause string, value any) {
		args = append(args, value)
		where = append(where, fmt.Sprintf(clause, len(args)))
	}
	if f.StudyPlanID != nil {
		add("z.study_plan_id = $%d", *f.StudyPlanID)
	}
	if f.DocumentID != nil {
		add("z.document_id = $%d", *f.DocumentID)
	}
	if f.Query != "" {
		// Title only: a quiz has no description, and the questions are not
		// searched. Searching the questions would mean a `q=` that leaks what
		// a quiz asks into a list the user is browsing before they sit it.
		// See db.Contains for the escaping.
		add(`z.title ILIKE $%d ESCAPE '\'`, db.Contains(f.Query))
	}

	order, ok := QuizSorts[f.Sort]
	if !ok {
		// Unreachable through the service, which validates first. It is here
		// because the alternative is not a wrong answer but a syntax error.
		order = QuizSorts[DefaultQuizSort]
	}
	query := `SELECT ` + quizColumns + quizFrom + ` WHERE ` + strings.Join(where, " AND ") +
		` ORDER BY ` + qualified("z", order) + `, z.id ASC` +
		` LIMIT $` + strconv.Itoa(len(args)+1) + ` OFFSET $` + strconv.Itoa(len(args)+2)
	args = append(args, f.Limit, f.Offset)

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("select quizzes: %w", err)
	}
	defer rows.Close()

	// A list carries no questions. It is a list of titles and counts, and
	// loading every quiz's questions to render one would be both wasteful and
	// a way to put the answers somewhere they were not asked for.
	out := []Quiz{}
	for rows.Next() {
		q, err := scanQuiz(rows)
		if err != nil {
			return nil, fmt.Errorf("scan quiz: %w", err)
		}
		out = append(out, q)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate quizzes: %w", err)
	}
	return out, nil
}

// DeleteQuiz removes the quiz; its questions, its attempts and their answers
// all cascade from it.
func (r *Repository) DeleteQuiz(ctx context.Context, userID, id uuid.UUID) error {
	res, err := r.db.ExecContext(ctx,
		`DELETE FROM quizzes WHERE id = $1 AND user_id = $2`, id, userID)
	if err != nil {
		return fmt.Errorf("delete quiz: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete quiz: %w", err)
	}
	if n == 0 {
		return ErrQuizNotFound
	}
	return nil
}

// --- attempts --------------------------------------------------------------------

const attemptColumns = `a.id, a.user_id, a.quiz_id, z.title, a.started_at, a.completed_at, a.score,
	(SELECT count(*) FROM quiz_questions qq WHERE qq.quiz_id = a.quiz_id) AS question_count`

// attemptFrom joins the quiz, which is an inner join on purpose: quiz_id is
// NOT NULL and cascades, so an attempt without its quiz does not exist, and an
// inner join means the owner clause on the quiz applies to the attempt too.
const attemptFrom = ` FROM quiz_attempts a
	JOIN quizzes z ON z.id = a.quiz_id AND z.user_id = a.user_id`

func scanAttempt(row interface{ Scan(...any) error }) (Attempt, error) {
	var a Attempt
	err := row.Scan(&a.ID, &a.UserID, &a.QuizID, &a.QuizTitle,
		&a.StartedAt, &a.CompletedAt, &a.Score, &a.QuestionCount)
	return a, err
}

// CreateAttempt starts an attempt at one of the caller's quizzes.
//
// The INSERT ... SELECT is what scopes it: there is no separate "does this
// quiz belong to you" query whose answer could be stale by the time the insert
// runs. A quiz that is not the caller's selects no row, so nothing is
// inserted, and the answer is the same 404 a missing one gets.
func (r *Repository) CreateAttempt(ctx context.Context, userID, quizID uuid.UUID) (Attempt, error) {
	var id uuid.UUID
	err := r.db.QueryRowContext(ctx, `
		INSERT INTO quiz_attempts (user_id, quiz_id)
		SELECT z.user_id, z.id FROM quizzes z WHERE z.id = $1 AND z.user_id = $2
		RETURNING id`, quizID, userID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return Attempt{}, ErrQuizNotFound
	}
	if err != nil {
		return Attempt{}, fmt.Errorf("insert quiz attempt: %w", err)
	}
	return r.AttemptByID(ctx, userID, id)
}

// AttemptByID returns one attempt with the answers given so far, oldest first.
func (r *Repository) AttemptByID(ctx context.Context, userID, id uuid.UUID) (Attempt, error) {
	a, err := scanAttempt(r.db.QueryRowContext(ctx,
		`SELECT `+attemptColumns+attemptFrom+` WHERE a.id = $1 AND a.user_id = $2`, id, userID))
	if errors.Is(err, sql.ErrNoRows) {
		return Attempt{}, ErrAttemptNotFound
	}
	if err != nil {
		return Attempt{}, fmt.Errorf("select quiz attempt: %w", err)
	}
	a.Answers, err = r.answers(ctx, userID, id)
	if err != nil {
		return Attempt{}, err
	}
	return a, nil
}

// answerSelect reads an answer with the question it answers, and with the
// owner in the statement -- the answer's own table has no user_id, so the
// scoping is the join to the attempt rather than something the caller has
// established already.
//
// The question's text and its correct index are carried because the only place
// an answer is read is a review of an attempt, and "you picked 2" is not a
// review.
const answerSelect = `
	SELECT ans.id, ans.attempt_id, ans.question_id, qq.question, qq.options,
	       ans.selected_index, qq.correct_index, ans.correct, ans.created_at
	FROM quiz_answers ans
	JOIN quiz_attempts a ON a.id = ans.attempt_id
	JOIN quiz_questions qq ON qq.id = ans.question_id
	WHERE a.user_id = $1 AND `

func scanAnswer(row interface{ Scan(...any) error }) (Answer, error) {
	var (
		a       Answer
		options []byte
	)
	if err := row.Scan(&a.ID, &a.AttemptID, &a.QuestionID, &a.Question, &options,
		&a.SelectedIndex, &a.CorrectIndex, &a.Correct, &a.CreatedAt); err != nil {
		return Answer{}, err
	}
	if err := json.Unmarshal(options, &a.Options); err != nil {
		return Answer{}, fmt.Errorf("decode quiz options for %s: %w", a.QuestionID, err)
	}
	return a, nil
}

// answers reads one attempt's answers, oldest first -- the order they were
// given in, which holds inside a batch as well as between requests because the
// column is stamped with clock_timestamp(); see migration 000011.
func (r *Repository) answers(ctx context.Context, userID, attemptID uuid.UUID) ([]Answer, error) {
	rows, err := r.db.QueryContext(ctx,
		answerSelect+`ans.attempt_id = $2 ORDER BY ans.created_at ASC, ans.id ASC`,
		userID, attemptID)
	if err != nil {
		return nil, fmt.Errorf("select quiz answers: %w", err)
	}
	defer rows.Close()

	out := []Answer{}
	for rows.Next() {
		a, err := scanAnswer(rows)
		if err != nil {
			return nil, fmt.Errorf("scan quiz answer: %w", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate quiz answers: %w", err)
	}
	return out, nil
}

// answerByID reads back one answer, which is what submitting one returns.
func (r *Repository) answerByID(ctx context.Context, userID, id uuid.UUID) (Answer, error) {
	a, err := scanAnswer(r.db.QueryRowContext(ctx, answerSelect+`ans.id = $2`, userID, id))
	if err != nil {
		return Answer{}, fmt.Errorf("select quiz answer: %w", err)
	}
	return a, nil
}

// QuestionForAttempt loads one question of the quiz an attempt is at, scoped
// to the owner.
//
// It is the lookup grading needs: the correct index to compare against, and
// the option count to bound the submitted one. Reaching the question *through
// the attempt* is the point -- a question id that belongs to another quiz of
// the caller's own is as much "not a question in this attempt" as one that
// belongs to a stranger, and both get the same answer.
func (r *Repository) QuestionForAttempt(ctx context.Context, userID, attemptID, questionID uuid.UUID) (Question, error) {
	q, err := scanQuestion(r.db.QueryRowContext(ctx, `
		SELECT qq.id, qq.quiz_id, qq.question, qq.options, qq.correct_index, qq.topic, qq.created_at
		FROM quiz_questions qq
		JOIN quiz_attempts a ON a.quiz_id = qq.quiz_id
		WHERE qq.id = $1 AND a.id = $2 AND a.user_id = $3`, questionID, attemptID, userID))
	if errors.Is(err, sql.ErrNoRows) {
		return Question{}, ErrQuestionNotFound
	}
	if err != nil {
		return Question{}, err
	}
	return q, nil
}

// CreateAnswer records one graded answer.
//
// The INSERT ... SELECT carries the three conditions that make it legal --
// the attempt is the caller's, it is still open, and the question is one of
// its quiz's -- so none of them is a check that could go stale between the
// asking and the writing. The service checks them too, because it has better
// errors to give; this is what makes them true.
//
// Zero rows means one of the three stopped holding between the service's read
// and here: the attempt was completed or deleted, or the question was. The row
// it named is gone or closed either way, so "no such attempt" is the honest
// answer, and it keeps a race from surfacing as a 500.
func (r *Repository) CreateAnswer(ctx context.Context, userID, attemptID uuid.UUID, in AnswerInput, correct bool) (Answer, error) {
	var id uuid.UUID
	err := r.db.QueryRowContext(ctx, `
		INSERT INTO quiz_answers (attempt_id, question_id, selected_index, correct)
		SELECT a.id, qq.id, $3, $4
		FROM quiz_attempts a
		JOIN quiz_questions qq ON qq.quiz_id = a.quiz_id AND qq.id = $2
		WHERE a.id = $1 AND a.user_id = $5 AND a.completed_at IS NULL
		RETURNING id`,
		attemptID, in.QuestionID, in.SelectedIndex, correct, userID).Scan(&id)
	switch {
	case IsUniqueViolation(err):
		// Two submissions of the same question raced the service's check. The
		// constraint is what stops the second one, and this is its answer.
		return Answer{}, ErrAlreadyAnswered
	case errors.Is(err, sql.ErrNoRows):
		return Answer{}, ErrAttemptNotFound
	case err != nil:
		return Answer{}, fmt.Errorf("insert quiz answer: %w", err)
	}

	// Read back through the join rather than RETURNING, so a submitted answer
	// and a listed one are built by the same query and carry the question the
	// same way.
	return r.answerByID(ctx, userID, id)
}

// CompleteAttempt finalises an attempt and scores it.
//
// The score is computed in the same statement that closes the attempt, out of
// the rows themselves, so there is no window in which an attempt is complete
// and unscored and no way for the number to disagree with the answers it came
// from. `completed_at IS NULL` in the WHERE is what makes it once-only: a
// second completion matches no row.
//
// Unanswered questions are not counted as wrong, because they are not counted
// at all: the score is how many were right, and the attempt carries the quiz's
// question count beside it so "4" is read as "4 of 6".
func (r *Repository) CompleteAttempt(ctx context.Context, userID, id uuid.UUID) (Attempt, error) {
	var updated uuid.UUID
	err := r.db.QueryRowContext(ctx, `
		UPDATE quiz_attempts
		SET completed_at = now(),
		    score = (SELECT count(*) FROM quiz_answers ans
		              WHERE ans.attempt_id = quiz_attempts.id AND ans.correct)
		WHERE id = $1 AND user_id = $2 AND completed_at IS NULL
		RETURNING id`, id, userID).Scan(&updated)
	if errors.Is(err, sql.ErrNoRows) {
		// Either there is no such attempt of the caller's, or there is one and
		// it is already finished. Only a read can tell those apart, and they
		// are different answers -- a 404 and a 409 -- so it is worth the
		// second query on this path.
		if _, err := r.AttemptByID(ctx, userID, id); err == nil {
			return Attempt{}, ErrAttemptComplete
		}
		return Attempt{}, ErrAttemptNotFound
	}
	if err != nil {
		return Attempt{}, fmt.Errorf("complete quiz attempt: %w", err)
	}
	return r.AttemptByID(ctx, userID, updated)
}
