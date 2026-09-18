package finance

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/jashveer/lifeos/backend/internal/httpx"
	"github.com/jashveer/lifeos/backend/internal/optional"
)

// Handler adapts Service to HTTP.
type Handler struct {
	svc *Service
	log *slog.Logger
}

func NewHandler(svc *Service, log *slog.Logger) *Handler {
	return &Handler{svc: svc, log: log}
}

// ExpenseRoutes returns the subtree mounted at /expenses. It assumes
// auth.RequireAuth is already in front of it.
//
// `/summary` is registered before `/{id}`, and chi matches a static segment
// ahead of a wildcard whatever the order -- but writing them this way makes it
// obvious that `GET /expenses/summary` is not an expense whose id is "summary".
func (h *Handler) ExpenseRoutes() chi.Router {
	r := chi.NewRouter()
	r.Get("/", h.List)
	r.Post("/", h.Create)
	r.Get("/summary", h.Summary)
	r.Get("/{id}", h.Get)
	r.Patch("/{id}", h.Update)
	r.Delete("/{id}", h.Delete)
	return r
}

// CategoryRoutes returns the subtree mounted at /expense-categories.
//
// There is no PATCH and no DELETE. Renaming a category would rewrite the label
// on every expense filed under it, and deleting one silently un-files them
// (the foreign key is ON DELETE SET NULL); both are edits to historical
// spending made through a door marked "categories", and both wait for a phase
// that can show the user what would change. See docs/decisions.md.
func (h *Handler) CategoryRoutes() chi.Router {
	r := chi.NewRouter()
	r.Get("/", h.ListCategories)
	r.Post("/", h.CreateCategory)
	return r
}

// --- wire types -------------------------------------------------------------

type categoryRequest struct {
	Name string `json:"name"`
}

type categoryResponse struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	CreatedAt string `json:"created_at"`
}

type categoryListResponse struct {
	Categories []categoryResponse `json:"categories"`
	Count      int                `json:"count"`
}

type expenseRequest struct {
	// Amount decodes a JSON number or a decimal string, exactly -- see
	// Amount.UnmarshalJSON. A client that sends 12.34 gets 12.34 stored.
	Amount            Amount     `json:"amount"`
	Currency          string     `json:"currency"`
	CategoryID        *uuid.UUID `json:"category_id"`
	Description       string     `json:"description"`
	ExpenseDate       string     `json:"expense_date"`
	RelatedDocumentID *uuid.UUID `json:"related_document_id"`
}

type expensePatchRequest struct {
	Amount            optional.Field[Amount]    `json:"amount"`
	Currency          optional.Field[string]    `json:"currency"`
	CategoryID        optional.Field[uuid.UUID] `json:"category_id"`
	Description       optional.Field[string]    `json:"description"`
	ExpenseDate       optional.Field[string]    `json:"expense_date"`
	RelatedDocumentID optional.Field[uuid.UUID] `json:"related_document_id"`
}

type expenseResponse struct {
	ID       string `json:"id"`
	Amount   Amount `json:"amount"`
	Currency string `json:"currency"`
	// Category is the name, beside the id, so a client renders a row without a
	// second request. Both are null for an expense filed under nothing.
	CategoryID        *string `json:"category_id"`
	Category          *string `json:"category"`
	Description       *string `json:"description"`
	ExpenseDate       string  `json:"expense_date"`
	RelatedDocumentID *string `json:"related_document_id"`
	CreatedAt         string  `json:"created_at"`
	UpdatedAt         string  `json:"updated_at"`
}

// listResponse echoes the window back as well as the paging, so a client can
// see exactly which range answered. Both bounds are null when the caller named
// none, which is a legal read here.
type listResponse struct {
	Expenses []expenseResponse `json:"expenses"`
	Count    int               `json:"count"`
	Start    *string           `json:"start"`
	End      *string           `json:"end"`
	Limit    int               `json:"limit"`
	Offset   int               `json:"offset"`
}

// summaryResponse is the aggregate. There is no grand total across currencies,
// on purpose: see Summary.
type summaryResponse struct {
	Start      *string            `json:"start"`
	End        *string            `json:"end"`
	Count      int                `json:"count"`
	Currencies []currencyResponse `json:"currencies"`
}

type currencyResponse struct {
	Currency   string             `json:"currency"`
	Total      Amount             `json:"total"`
	Count      int                `json:"count"`
	Categories []categoryTotalOut `json:"categories"`
}

type categoryTotalOut struct {
	CategoryID *string `json:"category_id"`
	Category   *string `json:"category"`
	Total      Amount  `json:"total"`
	Count      int     `json:"count"`
}

func toCategoryResponse(c Category) categoryResponse {
	return categoryResponse{
		ID: c.ID.String(), Name: c.Name,
		CreatedAt: c.CreatedAt.UTC().Format(httpx.TimeFormat),
	}
}

func toResponse(e Expense) expenseResponse {
	resp := expenseResponse{
		ID: e.ID.String(), Amount: e.Amount, Currency: e.Currency,
		Category:    e.CategoryName,
		Description: e.Description,
		ExpenseDate: e.Date.UTC().Format(time.DateOnly),
		CreatedAt:   e.CreatedAt.UTC().Format(httpx.TimeFormat),
		UpdatedAt:   e.UpdatedAt.UTC().Format(httpx.TimeFormat),
	}
	if e.CategoryID != nil {
		id := e.CategoryID.String()
		resp.CategoryID = &id
	}
	if e.RelatedDocumentID != nil {
		id := e.RelatedDocumentID.String()
		resp.RelatedDocumentID = &id
	}
	return resp
}

func toSummaryResponse(s Summary) summaryResponse {
	out := summaryResponse{
		Start: dayString(s.Start), End: dayString(s.End), Count: s.Count,
		Currencies: make([]currencyResponse, 0, len(s.Currencies)),
	}
	for _, c := range s.Currencies {
		row := currencyResponse{
			Currency: c.Currency, Total: c.Total, Count: c.Count,
			Categories: make([]categoryTotalOut, 0, len(c.Categories)),
		}
		for _, line := range c.Categories {
			total := categoryTotalOut{Total: line.Total, Count: line.Count}
			if line.CategoryID != nil {
				id, name := line.CategoryID.String(), line.Category
				total.CategoryID, total.Category = &id, &name
			}
			row.Categories = append(row.Categories, total)
		}
		out.Currencies = append(out.Currencies, row)
	}
	return out
}

func dayString(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := t.UTC().Format(time.DateOnly)
	return &s
}

// --- category handlers ------------------------------------------------------

// ListCategories handles GET /api/v1/expense-categories.
func (h *Handler) ListCategories(w http.ResponseWriter, r *http.Request) {
	userID, ok := httpx.UserID(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized", "Authentication required.")
		return
	}
	found, err := h.svc.Categories(r.Context(), userID)
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	out := categoryListResponse{Categories: make([]categoryResponse, 0, len(found)), Count: len(found)}
	for _, c := range found {
		out.Categories = append(out.Categories, toCategoryResponse(c))
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// CreateCategory handles POST /api/v1/expense-categories.
func (h *Handler) CreateCategory(w http.ResponseWriter, r *http.Request) {
	userID, ok := httpx.UserID(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized", "Authentication required.")
		return
	}
	var req categoryRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	c, err := h.svc.CreateCategory(r.Context(), userID, req.Name)
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, toCategoryResponse(c))
}

// --- expense handlers -------------------------------------------------------

// List handles GET /api/v1/expenses?start=&end=&category_id=.
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	userID, ok := httpx.UserID(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized", "Authentication required.")
		return
	}
	filter, ok := h.filterParams(w, r)
	if !ok {
		return
	}
	var err error
	if filter.Limit, err = intParam(r.URL.Query().Get("limit")); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "validation_failed", "limit must be a whole number.")
		return
	}
	if filter.Offset, err = intParam(r.URL.Query().Get("offset")); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "validation_failed", "offset must be a whole number.")
		return
	}

	found, err := h.svc.List(r.Context(), userID, filter)
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	// Echo back the effective query, which may have been clamped.
	effective, _ := ValidateFilter(filter)
	out := listResponse{
		Expenses: make([]expenseResponse, 0, len(found)), Count: len(found),
		Start: dayString(effective.Start), End: dayString(effective.End),
		Limit: effective.Limit, Offset: effective.Offset,
	}
	for _, e := range found {
		out.Expenses = append(out.Expenses, toResponse(e))
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// Summary handles GET /api/v1/expenses/summary?start=&end=.
//
// It takes the same filter the list does -- including `category_id`, so "how
// much on food, month by month" is one call per month rather than a client-side
// sum -- and returns totals rather than rows.
func (h *Handler) Summary(w http.ResponseWriter, r *http.Request) {
	userID, ok := httpx.UserID(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized", "Authentication required.")
		return
	}
	filter, ok := h.filterParams(w, r)
	if !ok {
		return
	}
	summary, err := h.svc.Summarize(r.Context(), userID, filter)
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, toSummaryResponse(summary))
}

// Create handles POST /api/v1/expenses.
func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	userID, ok := httpx.UserID(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized", "Authentication required.")
		return
	}
	var req expenseRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	date, err := dateParam(req.ExpenseDate)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "validation_failed",
			"expense_date must be a YYYY-MM-DD date.")
		return
	}
	expense, err := h.svc.Create(r.Context(), userID, CreateInput{
		Amount: req.Amount, Currency: req.Currency, CategoryID: req.CategoryID,
		Description: req.Description, Date: date, RelatedDocumentID: req.RelatedDocumentID,
	})
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, toResponse(expense))
}

// Get handles GET /api/v1/expenses/{id}.
func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	userID, id, ok := h.scope(w, r)
	if !ok {
		return
	}
	expense, err := h.svc.Get(r.Context(), userID, id)
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, toResponse(expense))
}

// Update handles PATCH /api/v1/expenses/{id}.
func (h *Handler) Update(w http.ResponseWriter, r *http.Request) {
	userID, id, ok := h.scope(w, r)
	if !ok {
		return
	}
	var req expensePatchRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	in := UpdateInput{
		Amount: req.Amount, Currency: req.Currency, CategoryID: req.CategoryID,
		Description: req.Description, RelatedDocumentID: req.RelatedDocumentID,
	}
	// The date arrives as a string so "2026-09-17" and null stay different
	// things; an unreadable one is a 400 before the service sees it.
	if raw, ok := req.ExpenseDate.Get(); ok {
		day, err := dateParam(raw)
		if err != nil || day.IsZero() {
			httpx.WriteError(w, http.StatusBadRequest, "validation_failed",
				"expense_date must be a YYYY-MM-DD date.")
			return
		}
		in.Date = optional.Of(day)
	} else if req.ExpenseDate.Cleared() {
		in.Date = optional.Null[time.Time]()
	}

	expense, err := h.svc.Update(r.Context(), userID, id, in)
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, toResponse(expense))
}

// Delete handles DELETE /api/v1/expenses/{id}.
func (h *Handler) Delete(w http.ResponseWriter, r *http.Request) {
	userID, id, ok := h.scope(w, r)
	if !ok {
		return
	}
	if err := h.svc.Delete(r.Context(), userID, id); err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- helpers ----------------------------------------------------------------

// filterParams reads the query parameters the list and the summary share. It
// writes the error response itself and reports whether parsing succeeded.
func (h *Handler) filterParams(w http.ResponseWriter, r *http.Request) (Filter, bool) {
	q := r.URL.Query()
	f := Filter{Query: q.Get("q"), Sort: q.Get("sort")}

	for name, dst := range map[string]**time.Time{"start": &f.Start, "end": &f.End} {
		raw := q.Get(name)
		if raw == "" {
			continue
		}
		day, err := dateParam(raw)
		if err != nil {
			httpx.WriteError(w, http.StatusBadRequest, "validation_failed",
				name+" must be a YYYY-MM-DD date.")
			return Filter{}, false
		}
		*dst = &day
	}

	switch raw := q.Get("category_id"); raw {
	case "":
	case "none", "null":
		// The one filter value that is not an id: "what have I not filed".
		f.Uncategorized = true
	default:
		id, err := uuid.Parse(raw)
		if err != nil {
			httpx.WriteError(w, http.StatusBadRequest, "validation_failed",
				"category_id must be an id, or \"none\" for the expenses with no category.")
			return Filter{}, false
		}
		f.CategoryID = &id
	}
	return f, true
}

// scope pulls the authenticated user and the path id. A malformed id is a 404
// rather than a 400, for the reason tasks.Handler.scope gives.
func (h *Handler) scope(w http.ResponseWriter, r *http.Request) (uuid.UUID, uuid.UUID, bool) {
	userID, ok := httpx.UserID(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized", "Authentication required.")
		return uuid.Nil, uuid.Nil, false
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.NotFound(w, "expense")
		return uuid.Nil, uuid.Nil, false
	}
	return userID, id, true
}

func intParam(v string) (int, error) {
	if v == "" {
		return 0, nil
	}
	return strconv.Atoi(v)
}

// dateParam reads a day. Only YYYY-MM-DD is accepted, and an RFC 3339
// timestamp is read for the day it falls on -- an expense has no time of day
// and one should not be invented for it.
//
// An empty value is the zero time, which the service reports as the missing
// required field it is, so "no date" and "an unreadable date" stay different
// answers.
func dateParam(v string) (time.Time, error) {
	if v == "" {
		return time.Time{}, nil
	}
	if t, err := time.Parse(time.DateOnly, v); err == nil {
		return t, nil
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return time.Time{}, err
	}
	return Day(t), nil
}

func (h *Handler) writeServiceError(w http.ResponseWriter, r *http.Request, err error) {
	var verrs httpx.ValidationErrors
	switch {
	case errors.As(err, &verrs):
		httpx.WriteJSON(w, http.StatusBadRequest, httpx.ErrorBody{
			Error:   "validation_failed",
			Message: "One or more fields are invalid.",
			Fields:  verrs,
		})
	case errors.Is(err, ErrCategoryNotFound):
		httpx.NotFound(w, "category")
	case errors.Is(err, ErrNotFound):
		httpx.NotFound(w, "expense")
	default:
		// Internal detail stays in the log, never in the response.
		h.log.Error("finance request failed", "error", err, "path", r.URL.Path, "method", r.Method)
		httpx.WriteError(w, http.StatusInternalServerError, "internal_error", "Something went wrong.")
	}
}
