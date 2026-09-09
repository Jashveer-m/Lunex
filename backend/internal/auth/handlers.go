package auth

import (
	"errors"
	"log/slog"
	"net/http"
)

// Handler adapts Service to HTTP.
type Handler struct {
	svc *Service
	log *slog.Logger
}

func NewHandler(svc *Service, log *slog.Logger) *Handler {
	return &Handler{svc: svc, log: log}
}

// --- wire types -------------------------------------------------------------

type registerRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
	Name     string `json:"name"`
	Timezone string `json:"timezone"`
}

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type refreshRequest struct {
	RefreshToken string `json:"refresh_token"`
}

type userResponse struct {
	ID        string `json:"id"`
	Email     string `json:"email"`
	Name      string `json:"name"`
	Timezone  string `json:"timezone"`
	CreatedAt string `json:"created_at"`
}

type tokenResponse struct {
	AccessToken           string `json:"access_token"`
	TokenType             string `json:"token_type"`
	ExpiresIn             int    `json:"expires_in"`
	AccessTokenExpiresAt  string `json:"access_token_expires_at"`
	RefreshToken          string `json:"refresh_token"`
	RefreshTokenExpiresAt string `json:"refresh_token_expires_at"`
}

type authResponse struct {
	User   userResponse  `json:"user"`
	Tokens tokenResponse `json:"tokens"`
}

func toUserResponse(a Account) userResponse {
	return userResponse{
		ID:        a.User.ID.String(),
		Email:     a.User.Email,
		Name:      a.Profile.Name,
		Timezone:  a.Profile.Timezone,
		CreatedAt: a.User.CreatedAt.UTC().Format(timeFormat),
	}
}

func toTokenResponse(p TokenPair) tokenResponse {
	return tokenResponse{
		AccessToken:           p.AccessToken,
		TokenType:             "Bearer",
		AccessTokenExpiresAt:  p.AccessExpiresAt.UTC().Format(timeFormat),
		RefreshToken:          p.RefreshToken,
		RefreshTokenExpiresAt: p.RefreshExpires.UTC().Format(timeFormat),
	}
}

// --- handlers ---------------------------------------------------------------

// Register handles POST /api/v1/auth/register.
func (h *Handler) Register(w http.ResponseWriter, r *http.Request) {
	var req registerRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	account, pair, err := h.svc.Register(r.Context(), RegisterInput{
		Email:      req.Email,
		Password:   req.Password,
		Name:       req.Name,
		Timezone:   req.Timezone,
		DeviceInfo: deviceInfo(r),
	})
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, authResponse{
		User:   toUserResponse(account),
		Tokens: h.tokens(pair),
	})
}

// Login handles POST /api/v1/auth/login.
func (h *Handler) Login(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	account, pair, err := h.svc.Login(r.Context(), LoginInput{
		Email:      req.Email,
		Password:   req.Password,
		DeviceInfo: deviceInfo(r),
	})
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, authResponse{
		User:   toUserResponse(account),
		Tokens: h.tokens(pair),
	})
}

// Refresh handles POST /api/v1/auth/refresh.
func (h *Handler) Refresh(w http.ResponseWriter, r *http.Request) {
	var req refreshRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	pair, err := h.svc.Refresh(r.Context(), req.RefreshToken)
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, h.tokens(pair))
}

// Logout handles POST /api/v1/auth/logout.
func (h *Handler) Logout(w http.ResponseWriter, r *http.Request) {
	var req refreshRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	if err := h.svc.Logout(r.Context(), req.RefreshToken); err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Me handles GET /api/v1/me and requires RequireAuth in front of it.
func (h *Handler) Me(w http.ResponseWriter, r *http.Request) {
	userID, ok := UserIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Authentication required.")
		return
	}
	account, err := h.svc.Me(r.Context(), userID)
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toUserResponse(account))
}

// --- helpers ----------------------------------------------------------------

const timeFormat = "2006-01-02T15:04:05.000Z"

func (h *Handler) tokens(p TokenPair) tokenResponse {
	resp := toTokenResponse(p)
	resp.ExpiresIn = int(p.AccessExpiresAt.Sub(h.svc.now()).Round(0).Seconds())
	if resp.ExpiresIn < 0 {
		resp.ExpiresIn = 0
	}
	return resp
}

// deviceInfo records something useful about the client on the session row
// without storing anything the user did not already send.
func deviceInfo(r *http.Request) string {
	ua := r.UserAgent()
	if len(ua) > maxDeviceInfoLen {
		ua = ua[:maxDeviceInfoLen]
	}
	return ua
}

func (h *Handler) writeServiceError(w http.ResponseWriter, r *http.Request, err error) {
	var verrs ValidationErrors
	switch {
	case errors.As(err, &verrs):
		writeJSON(w, http.StatusBadRequest, ErrorBody{
			Error:   "validation_failed",
			Message: "One or more fields are invalid.",
			Fields:  verrs,
		})
	case errors.Is(err, ErrEmailTaken):
		// 409 rather than 400: the payload is well-formed, the state conflicts.
		writeError(w, http.StatusConflict, "email_taken", "An account with that email already exists.")
	case errors.Is(err, ErrInvalidCredentials):
		writeError(w, http.StatusUnauthorized, "invalid_credentials", "Invalid email or password.")
	case errors.Is(err, ErrInvalidToken):
		writeError(w, http.StatusUnauthorized, "invalid_token", "The token is invalid or has expired.")
	default:
		// Internal detail stays in the log, never in the response.
		h.log.Error("auth request failed", "error", err, "path", r.URL.Path, "method", r.Method)
		writeError(w, http.StatusInternalServerError, "internal_error", "Something went wrong.")
	}
}
