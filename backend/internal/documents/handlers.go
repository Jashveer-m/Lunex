package documents

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/jashveer/lifeos/backend/internal/httpx"
	"github.com/jashveer/lifeos/backend/internal/validate"
)

// Handler adapts Service to HTTP.
type Handler struct {
	svc         *Service
	log         *slog.Logger
	maxUpload   int64
	maxMemoryMB int64
}

func NewHandler(svc *Service, log *slog.Logger, maxUpload int64) *Handler {
	if maxUpload <= 0 {
		maxUpload = MaxUploadBytes
	}
	// Anything past this spills to a temp file rather than sitting in the heap.
	// The file is read into memory afterwards anyway, but the spill keeps the
	// multipart parser from doubling peak usage on a large upload.
	return &Handler{svc: svc, log: log, maxUpload: maxUpload, maxMemoryMB: 4 << 20}
}

// Routes returns the subtree mounted at /documents. It assumes auth.RequireAuth
// is already in front of it.
func (h *Handler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Get("/", h.List)
	r.Post("/", h.Upload)
	// Registered before "/{id}" for readability only: chi matches a static
	// segment ahead of a wildcard regardless of order, and search is a POST
	// while {id} takes GET and DELETE, so the two never compete.
	r.Post("/search", h.Search)
	r.Get("/{id}", h.Get)
	r.Delete("/{id}", h.Delete)
	return r
}

// --- wire types -------------------------------------------------------------

type documentResponse struct {
	ID           string  `json:"id"`
	Filename     string  `json:"filename"`
	FileType     string  `json:"file_type"`
	Status       string  `json:"status"`
	ErrorMessage *string `json:"error_message"`
	ChunkCount   int     `json:"chunk_count"`
	// TextLength is the character count of the extracted text, and is present
	// only where that text was loaded: the single-document GET and the upload
	// response. A list leaves it out rather than reporting zero, because a list
	// does not select the text column -- and computing length() over it would
	// mean detoasting every document on the page to answer a curiosity.
	TextLength *int `json:"text_length,omitempty"`
	// ExtractedText is present only on the single-document GET. With no object
	// storage in this phase it is the only way to read a document's content
	// back, so it is not hidden behind a flag -- but a list of fifty of them
	// would be megabytes, so a list omits it.
	ExtractedText *string `json:"extracted_text,omitempty"`
	CreatedAt     string  `json:"created_at"`
	UpdatedAt     string  `json:"updated_at"`
}

type listResponse struct {
	Documents []documentResponse `json:"documents"`
	Count     int                `json:"count"`
	Limit     int                `json:"limit"`
	Offset    int                `json:"offset"`
}

type searchRequest struct {
	Query         string   `json:"query"`
	Limit         int      `json:"limit"`
	MinSimilarity float64  `json:"min_similarity"`
	DocumentIDs   []string `json:"document_ids"`
}

type searchResultResponse struct {
	ChunkID    string  `json:"chunk_id"`
	DocumentID string  `json:"document_id"`
	Filename   string  `json:"filename"`
	ChunkIndex int     `json:"chunk_index"`
	Content    string  `json:"content"`
	Similarity float64 `json:"similarity"`
}

type searchResponse struct {
	Query   string                 `json:"query"`
	Results []searchResultResponse `json:"results"`
	Count   int                    `json:"count"`
}

func toResponse(d Document, withText bool) documentResponse {
	out := documentResponse{
		ID:           d.ID.String(),
		Filename:     d.Filename,
		FileType:     d.FileType,
		Status:       d.Status,
		ErrorMessage: d.ErrorMessage,
		ChunkCount:   d.ChunkCount,
		CreatedAt:    d.CreatedAt.UTC().Format(httpx.TimeFormat),
		UpdatedAt:    d.UpdatedAt.UTC().Format(httpx.TimeFormat),
	}
	if d.ExtractedText != nil {
		n := len([]rune(*d.ExtractedText))
		out.TextLength = &n
		if withText {
			out.ExtractedText = d.ExtractedText
		}
	}
	return out
}

// --- handlers ---------------------------------------------------------------

// Upload handles POST /api/v1/documents: a multipart body with one `file` part.
func (h *Handler) Upload(w http.ResponseWriter, r *http.Request) {
	userID, ok := httpx.UserID(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized", "Authentication required.")
		return
	}

	// The size limit is enforced on the connection, before any parsing: an
	// oversized upload is cut off mid-transfer rather than buffered and then
	// rejected. The slack covers the multipart framing around the file itself.
	r.Body = http.MaxBytesReader(w, r.Body, h.maxUpload+(1<<20))
	if err := r.ParseMultipartForm(h.maxMemoryMB); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			h.tooLarge(w)
			return
		}
		httpx.WriteError(w, http.StatusBadRequest, "invalid_multipart",
			"Request body must be multipart/form-data with a `file` part.")
		return
	}
	defer r.MultipartForm.RemoveAll() //nolint:errcheck // temp-file cleanup

	file, header, err := r.FormFile("file")
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "invalid_multipart", "A `file` part is required.")
		return
	}
	defer file.Close()

	filename, verr := ValidateFilename(header.Filename)
	if verr != nil {
		httpx.WriteJSON(w, http.StatusBadRequest, httpx.ErrorBody{
			Error: "validation_failed", Message: "One or more fields are invalid.",
			Fields: validate.Errors{*verr},
		})
		return
	}

	// One byte past the limit so an exactly-at-the-limit file still reads whole
	// and a larger one is detectable without buffering the excess.
	content, err := io.ReadAll(io.LimitReader(file, h.maxUpload+1))
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			h.tooLarge(w)
			return
		}
		h.log.Error("read upload", "error", err, "user_id", userID)
		httpx.WriteError(w, http.StatusBadRequest, "invalid_multipart", "The uploaded file could not be read.")
		return
	}
	if int64(len(content)) > h.maxUpload {
		h.tooLarge(w)
		return
	}
	if len(content) == 0 {
		httpx.WriteJSON(w, http.StatusBadRequest, httpx.ErrorBody{
			Error: "validation_failed", Message: "One or more fields are invalid.",
			Fields: validate.Errors{{Field: "file", Message: "is empty"}},
		})
		return
	}

	fileType := DetectType(filename, content)
	if fileType == "" {
		httpx.WriteError(w, http.StatusUnsupportedMediaType, "unsupported_media_type",
			"Only PDF, TXT and Markdown files are supported.")
		return
	}

	doc, err := h.svc.Upload(r.Context(), userID, UploadInput{
		Filename: filename, FileType: fileType, Content: content,
	})
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	// 201 even when status is "failed": the row exists, has an id, and holds
	// the reason. See the note on Service.Upload.
	httpx.WriteJSON(w, http.StatusCreated, toResponse(doc, false))
}

// Search handles POST /api/v1/documents/search.
func (h *Handler) Search(w http.ResponseWriter, r *http.Request) {
	userID, ok := httpx.UserID(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized", "Authentication required.")
		return
	}
	var req searchRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}

	q := SearchQuery{Query: req.Query, Limit: req.Limit, MinSimilarity: req.MinSimilarity}
	for _, raw := range req.DocumentIDs {
		id, err := uuid.Parse(raw)
		if err != nil {
			httpx.WriteJSON(w, http.StatusBadRequest, httpx.ErrorBody{
				Error: "validation_failed", Message: "One or more fields are invalid.",
				Fields: validate.Errors{{Field: "document_ids", Message: "must all be uuids"}},
			})
			return
		}
		// An id belonging to somebody else needs no check here: the search is
		// scoped to the caller's chunks, so a foreign id simply matches
		// nothing, which is also what a made-up id does. Neither reveals
		// whether it exists.
		q.DocumentIDs = append(q.DocumentIDs, id)
	}

	results, err := h.svc.Search(r.Context(), userID, q)
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}

	out := searchResponse{Query: req.Query, Results: make([]searchResultResponse, 0, len(results)), Count: len(results)}
	for _, res := range results {
		out.Results = append(out.Results, searchResultResponse{
			ChunkID:    res.ChunkID.String(),
			DocumentID: res.DocumentID.String(),
			Filename:   res.Filename,
			ChunkIndex: res.ChunkIndex,
			Content:    res.Content,
			Similarity: res.Similarity,
		})
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// List handles GET /api/v1/documents.
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	userID, ok := httpx.UserID(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized", "Authentication required.")
		return
	}

	q := r.URL.Query()
	filter := Filter{Status: q.Get("status"), Sort: q.Get("sort")}
	var err error
	if filter.Limit, err = intParam(q.Get("limit")); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "validation_failed", "limit must be a whole number.")
		return
	}
	if filter.Offset, err = intParam(q.Get("offset")); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "validation_failed", "offset must be a whole number.")
		return
	}

	found, err := h.svc.List(r.Context(), userID, filter)
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}

	effective, _ := ValidateFilter(filter)
	out := listResponse{
		Documents: make([]documentResponse, 0, len(found)),
		Count:     len(found), Limit: effective.Limit, Offset: effective.Offset,
	}
	for _, d := range found {
		out.Documents = append(out.Documents, toResponse(d, false))
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// Get handles GET /api/v1/documents/{id}.
func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	userID, id, ok := h.scope(w, r)
	if !ok {
		return
	}
	doc, err := h.svc.Get(r.Context(), userID, id)
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, toResponse(doc, true))
}

// Delete handles DELETE /api/v1/documents/{id}. Chunks go with it, by cascade.
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

// scope pulls the authenticated user and the path id. A malformed id answers
// 404, the same as a document belonging to somebody else.
func (h *Handler) scope(w http.ResponseWriter, r *http.Request) (uuid.UUID, uuid.UUID, bool) {
	userID, ok := httpx.UserID(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized", "Authentication required.")
		return uuid.Nil, uuid.Nil, false
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.NotFound(w, "document")
		return uuid.Nil, uuid.Nil, false
	}
	return userID, id, true
}

func (h *Handler) tooLarge(w http.ResponseWriter) {
	httpx.WriteError(w, http.StatusRequestEntityTooLarge, "payload_too_large",
		"The file is larger than "+strconv.FormatInt(h.maxUpload>>20, 10)+" MB.")
}

func intParam(v string) (int, error) {
	if v == "" {
		return 0, nil
	}
	return strconv.Atoi(v)
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
	case errors.Is(err, ErrNotFound):
		httpx.NotFound(w, "document")
	case errors.Is(err, ErrEmbedding):
		// Search cannot run without the model server. This is an outage, not a
		// bad request, and 503 is the code a client should retry on.
		h.log.Error("embedding failed", "error", err, "path", r.URL.Path)
		httpx.WriteError(w, http.StatusServiceUnavailable, "embedding_unavailable",
			"Search is temporarily unavailable because the embedding service could not be reached.")
	default:
		h.log.Error("document request failed", "error", err, "path", r.URL.Path, "method", r.Method)
		httpx.WriteError(w, http.StatusInternalServerError, "internal_error", "Something went wrong.")
	}
}
