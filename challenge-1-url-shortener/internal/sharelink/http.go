package sharelink

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	viewerCookie   = "yp_vid"
	maxRequestBody = 1 << 10
)

// UserFunc returns the authenticated user id, or 0 for anonymous visitors.
// In YouPass it reads the id from the existing JWT middleware.
type UserFunc func(r *http.Request) int64

type Handler struct {
	svc     *Service
	views   *ViewCounter
	subs    Submissions
	user    UserFunc
	baseURL string // e.g. https://youpass.vn
	log     *slog.Logger
}

func NewHandler(svc *Service, views *ViewCounter, subs Submissions, user UserFunc, baseURL string, log *slog.Logger) *Handler {
	return &Handler{svc: svc, views: views, subs: subs, user: user, baseURL: strings.TrimRight(baseURL, "/"), log: log}
}

func (h *Handler) Register(mux *http.ServeMux) {
	// Owner APIs (authenticated).
	mux.HandleFunc("POST /api/v1/submissions/{id}/share", h.requireUser(h.create))
	mux.HandleFunc("GET /api/v1/shares/{code}", h.requireUser(h.get))
	mux.HandleFunc("PATCH /api/v1/shares/{code}", h.requireUser(h.update))
	mux.HandleFunc("DELETE /api/v1/shares/{code}", h.requireUser(h.delete))

	// Public APIs used by the /s/:code page.
	mux.HandleFunc("GET /api/v1/s/{code}", h.resolve)
	mux.HandleFunc("POST /api/v1/s/{code}/views", h.recordView)
}

type linkResponse struct {
	Code      string     `json:"code"`
	URL       string     `json:"url"`
	Enabled   bool       `json:"enabled"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	ViewCount int64      `json:"view_count"`
	CreatedAt time.Time  `json:"created_at"`
}

func (h *Handler) toResponse(r *http.Request, l *Link) linkResponse {
	return linkResponse{
		Code: l.Code, URL: h.baseURL + "/s/" + l.Code, Enabled: l.Status == StatusActive,
		ExpiresAt: l.ExpiresAt, CreatedAt: l.CreatedAt,
		ViewCount: l.ViewCount + h.views.Pending(r.Context(), l.Code),
	}
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request, userID int64) {
	submissionID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_submission_id")
		return
	}
	var body struct {
		ExpiresInHours int `json:"expires_in_hours"` // 0 = never expires
	}
	if !decodeOptionalBody(w, r, &body) {
		return
	}
	var opts CreateOptions
	if body.ExpiresInHours > 0 {
		t := time.Now().Add(time.Duration(body.ExpiresInHours) * time.Hour)
		opts.ExpiresAt = &t
	}

	link, created, err := h.svc.Create(r.Context(), userID, submissionID, opts)
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, h.toResponse(r, link))
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request, userID int64) {
	link, err := h.svc.Get(r.Context(), userID, r.PathValue("code"))
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, h.toResponse(r, link))
}

func (h *Handler) update(w http.ResponseWriter, r *http.Request, userID int64) {
	var body struct {
		Enabled *bool `json:"enabled"`
	}
	if !decodeOptionalBody(w, r, &body) {
		return
	}
	if body.Enabled == nil {
		writeError(w, http.StatusBadRequest, "enabled_required")
		return
	}
	link, err := h.svc.SetEnabled(r.Context(), userID, r.PathValue("code"), *body.Enabled)
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, h.toResponse(r, link))
}

func (h *Handler) delete(w http.ResponseWriter, r *http.Request, userID int64) {
	if err := h.svc.Delete(r.Context(), userID, r.PathValue("code")); err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// resolve serves the public page. It is a pure read: counting happens in
// recordView, so link-preview crawlers (Facebook, Zalo), prefetches and SSR
// fetches do not inflate the counter.
func (h *Handler) resolve(w http.ResponseWriter, r *http.Request) {
	// Revocation must take effect immediately, so neither browsers nor
	// shared proxies may cache the response. Students' work must not be
	// indexed by search engines either.
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Robots-Tag", "noindex, nofollow")
	w.Header().Set("Referrer-Policy", "no-referrer")

	link, err := h.svc.Resolve(r.Context(), r.PathValue("code"))
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	view, err := h.subs.PublicView(r.Context(), link.SubmissionID)
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"code":       link.Code,
		"is_owner":   h.user(r) == link.OwnerID,
		"submission": view,
	})
}

// recordView is called by the page after it renders, via
// navigator.sendBeacon. It always answers 202 so it cannot be used to probe
// which codes exist.
func (h *Handler) recordView(w http.ResponseWriter, r *http.Request) {
	defer w.WriteHeader(http.StatusAccepted)

	if isBot(r) {
		return
	}
	link, err := h.svc.Resolve(r.Context(), r.PathValue("code"))
	if err != nil {
		return
	}
	userID := h.user(r)
	if userID != 0 && userID == link.OwnerID {
		return // students re-opening their own link do not count
	}
	h.views.Enqueue(link.Code, viewerKey(w, r, userID))
}

// viewerKey identifies a viewer for deduplication: the user id when logged
// in, otherwise a first-party cookie set on the first visit.
func viewerKey(w http.ResponseWriter, r *http.Request, userID int64) string {
	if userID != 0 {
		return "u" + strconv.FormatInt(userID, 10)
	}
	if c, err := r.Cookie(viewerCookie); err == nil && validVisitorID(c.Value) {
		return "c" + c.Value
	}
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	id := hex.EncodeToString(b)
	http.SetCookie(w, &http.Cookie{
		Name: viewerCookie, Value: id, Path: "/", MaxAge: 365 * 24 * 3600,
		HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode,
	})
	return "c" + id
}

func validVisitorID(s string) bool {
	if len(s) != 32 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

var botUA = regexp.MustCompile(`(?i)bot|crawl|spider|slurp|facebookexternalhit|zalo|preview|headless|curl|wget|python-requests|go-http-client`)

func isBot(r *http.Request) bool {
	ua := r.UserAgent()
	return ua == "" || botUA.MatchString(ua) ||
		strings.Contains(r.Header.Get("Sec-Purpose"), "prefetch") ||
		r.Header.Get("Purpose") == "prefetch"
}

func (h *Handler) requireUser(next func(http.ResponseWriter, *http.Request, int64)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID := h.user(r)
		if userID == 0 {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next(w, r, userID)
	}
}

func (h *Handler) writeServiceError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, ErrNotFound), errors.Is(err, ErrSubmissionNotFound):
		writeError(w, http.StatusNotFound, "not_found")
	case errors.Is(err, ErrForbidden):
		writeError(w, http.StatusForbidden, "forbidden")
	default:
		h.log.ErrorContext(r.Context(), "sharelink: request failed", "path", r.URL.Path, "err", err)
		writeError(w, http.StatusInternalServerError, "internal_error")
	}
}

func decodeOptionalBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	if r.ContentLength == 0 {
		return true
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body")
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, map[string]string{"error": code})
}
