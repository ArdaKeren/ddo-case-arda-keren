package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"log"
	"net/http"
	"net/mail"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"

	"github.com/qa/backend/internal/audit"
	"github.com/qa/backend/internal/httpx"
)

const (
	minPasswordLen = 10
	maxPasswordLen = 128 // bytes; caps Argon2 input so hashing cost stays predictable
)

type Handler struct {
	store      *Store
	tokens     *TokenIssuer
	audit      *audit.Logger
	refreshTTL time.Duration
	params     Argon2Params

	// Verified against when the email is unknown so response timing does not
	// reveal which accounts exist.
	dummyHash string
}

func NewHandler(store *Store, tokens *TokenIssuer, auditLog *audit.Logger, refreshTTL time.Duration) (*Handler, error) {
	dummy, err := HashPassword("dummy-password-for-timing", DefaultArgon2)
	if err != nil {
		return nil, err
	}
	return &Handler{store: store, tokens: tokens, audit: auditLog, refreshTTL: refreshTTL,
		params: DefaultArgon2, dummyHash: dummy}, nil
}

// Routes is mounted under /api/auth.
func (h *Handler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Post("/register", h.register)
	r.Post("/login", h.login)
	r.Post("/refresh", h.refresh)
	r.Post("/logout", h.logout)
	r.With(Authenticate(h.tokens), RequirePermission(PermProfileRead)).Get("/me", h.me)
	return r
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
	User         User   `json:"user"`
}

func (h *Handler) register(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Email    string `json:"email"`
		Password string `json:"password"`
		FullName string `json:"full_name"`
	}
	if err := httpx.Decode(w, r, &in); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	email, ok := NormalizeEmail(in.Email)
	if !ok {
		httpx.Error(w, http.StatusBadRequest, "invalid email")
		return
	}
	in.FullName = strings.TrimSpace(in.FullName)
	if in.FullName == "" || utf8.RuneCountInString(in.FullName) > 200 {
		httpx.Error(w, http.StatusBadRequest, "full_name is required (max 200 chars)")
		return
	}
	if msg := ValidatePassword(in.Password); msg != "" {
		httpx.Error(w, http.StatusBadRequest, msg)
		return
	}

	hash, err := HashPassword(in.Password, h.params)
	if err != nil {
		h.internal(w, err)
		return
	}
	// Self-registration always yields the least-privileged role.
	u, err := h.store.CreateUser(r.Context(), email, hash, in.FullName, RoleUser)
	if errors.Is(err, ErrEmailTaken) {
		httpx.Error(w, http.StatusConflict, "email already registered")
		return
	}
	if err != nil {
		h.internal(w, err)
		return
	}
	h.audit.Log(r, audit.Entry{ActorUserID: u.ID, Action: "auth.register", EntityType: "qa-ev-users", EntityID: u.ID})
	httpx.JSON(w, http.StatusCreated, u)
}

func (h *Handler) login(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := httpx.Decode(w, r, &in); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if len(in.Password) > maxPasswordLen {
		httpx.Error(w, http.StatusUnauthorized, "invalid credentials")
		return
	}
	email, _ := NormalizeEmail(in.Email)

	u, err := h.store.UserByEmail(r.Context(), email)
	if err != nil && !errors.Is(err, ErrNotFound) {
		h.internal(w, err)
		return
	}
	found := err == nil
	hash := u.PasswordHash
	if !found {
		hash = h.dummyHash
	}
	ok, needsRehash, verr := VerifyPassword(in.Password, hash, h.params)
	if verr != nil {
		log.Printf("auth: verify hash for user %q: %v", u.ID, verr)
	}
	if !found || !ok || u.Status != "active" {
		h.audit.Log(r, audit.Entry{ActorUserID: u.ID, Action: "auth.login_failed", EntityType: "qa-ev-users",
			EntityID: u.ID, Metadata: map[string]any{"email": email}})
		httpx.Error(w, http.StatusUnauthorized, "invalid credentials")
		return
	}

	var newHash string
	if needsRehash {
		if newHash, err = HashPassword(in.Password, h.params); err != nil {
			log.Printf("auth: rehash: %v", err)
			newHash = ""
		}
	}
	if err := h.store.RecordLogin(r.Context(), u.ID, newHash); err != nil {
		h.internal(w, err)
		return
	}

	refresh, refreshHash, err := newRefreshToken()
	if err != nil {
		h.internal(w, err)
		return
	}
	if err := h.store.CreateSession(r.Context(), u.ID, refreshHash, r.UserAgent(), httpx.ClientIP(r),
		time.Now().Add(h.refreshTTL)); err != nil {
		h.internal(w, err)
		return
	}
	h.audit.Log(r, audit.Entry{ActorUserID: u.ID, Action: "auth.login", EntityType: "qa-ev-users", EntityID: u.ID})
	h.respondTokens(w, u, refresh)
}

func (h *Handler) refresh(w http.ResponseWriter, r *http.Request) {
	var in struct {
		RefreshToken string `json:"refresh_token"`
	}
	if err := httpx.Decode(w, r, &in); err != nil || in.RefreshToken == "" {
		httpx.Error(w, http.StatusBadRequest, "refresh_token is required")
		return
	}
	ss, err := h.store.SessionByTokenHash(r.Context(), hashToken(in.RefreshToken))
	if errors.Is(err, ErrNotFound) {
		httpx.Error(w, http.StatusUnauthorized, "invalid refresh token")
		return
	}
	if err != nil {
		h.internal(w, err)
		return
	}
	if ss.RevokedAt != nil {
		h.handleReuse(w, r, ss.UserID)
		return
	}
	if time.Now().After(ss.ExpiresAt) {
		httpx.Error(w, http.StatusUnauthorized, "refresh token expired")
		return
	}

	// Re-read the user so role changes and suspensions take effect on refresh.
	u, err := h.store.UserByID(r.Context(), ss.UserID)
	if err != nil || u.Status != "active" {
		_ = h.store.RevokeSession(r.Context(), ss.ID)
		httpx.Error(w, http.StatusUnauthorized, "account unavailable")
		return
	}

	refresh, refreshHash, err := newRefreshToken()
	if err != nil {
		h.internal(w, err)
		return
	}
	err = h.store.RotateSession(r.Context(), ss.ID, u.ID, refreshHash, r.UserAgent(), httpx.ClientIP(r),
		time.Now().Add(h.refreshTTL))
	if errors.Is(err, ErrTokenReused) {
		h.handleReuse(w, r, u.ID)
		return
	}
	if err != nil {
		h.internal(w, err)
		return
	}
	h.audit.Log(r, audit.Entry{ActorUserID: u.ID, Action: "auth.refresh", EntityType: "qa-user_sessions", EntityID: ss.ID})
	h.respondTokens(w, u, refresh)
}

// handleReuse treats a replayed refresh token as theft and revokes every session of that user.
func (h *Handler) handleReuse(w http.ResponseWriter, r *http.Request, userID string) {
	if err := h.store.RevokeAllSessions(r.Context(), userID); err != nil {
		log.Printf("auth: revoke all sessions: %v", err)
	}
	h.audit.Log(r, audit.Entry{ActorUserID: userID, Action: "auth.refresh_reuse_detected",
		EntityType: "qa-ev-users", EntityID: userID})
	httpx.Error(w, http.StatusUnauthorized, "invalid refresh token")
}

func (h *Handler) logout(w http.ResponseWriter, r *http.Request) {
	var in struct {
		RefreshToken string `json:"refresh_token"`
	}
	if err := httpx.Decode(w, r, &in); err != nil || in.RefreshToken == "" {
		httpx.Error(w, http.StatusBadRequest, "refresh_token is required")
		return
	}
	ss, err := h.store.SessionByTokenHash(r.Context(), hashToken(in.RefreshToken))
	if err == nil && ss.RevokedAt == nil {
		if err := h.store.RevokeSession(r.Context(), ss.ID); err != nil {
			h.internal(w, err)
			return
		}
		h.audit.Log(r, audit.Entry{ActorUserID: ss.UserID, Action: "auth.logout", EntityType: "qa-user_sessions", EntityID: ss.ID})
	}
	// Always 204 so the endpoint cannot be used to probe token validity.
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) me(w http.ResponseWriter, r *http.Request) {
	p, _ := PrincipalFrom(r.Context())
	u, err := h.store.UserByID(r.Context(), p.UserID)
	if errors.Is(err, ErrNotFound) {
		httpx.Error(w, http.StatusUnauthorized, "account unavailable")
		return
	}
	if err != nil {
		h.internal(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"user": u, "permissions": u.Role.Permissions()})
}

func (h *Handler) respondTokens(w http.ResponseWriter, u User, refresh string) {
	access, exp, err := h.tokens.Issue(u.ID, u.Role)
	if err != nil {
		h.internal(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, tokenResponse{
		AccessToken:  access,
		TokenType:    "Bearer",
		ExpiresIn:    int64(time.Until(exp).Seconds()),
		RefreshToken: refresh,
		User:         u,
	})
}

func (h *Handler) internal(w http.ResponseWriter, err error) {
	log.Printf("auth: %v", err)
	httpx.Error(w, http.StatusInternalServerError, "internal error")
}

// Refresh tokens are opaque random strings; only their SHA-256 is stored.
func newRefreshToken() (token, hash string, err error) {
	b := make([]byte, 32)
	if _, err = rand.Read(b); err != nil {
		return "", "", err
	}
	token = base64.RawURLEncoding.EncodeToString(b)
	return token, hashToken(token), nil
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func NormalizeEmail(s string) (string, bool) {
	s = strings.TrimSpace(s)
	addr, err := mail.ParseAddress(s)
	if err != nil || addr.Address != s || len(s) > 254 {
		return s, false
	}
	return strings.ToLower(s), true
}

// ValidatePassword returns a user-facing message, or "" if the password is acceptable.
func ValidatePassword(p string) string {
	if utf8.RuneCountInString(p) < minPasswordLen {
		return "password must be at least 10 characters"
	}
	if len(p) > maxPasswordLen {
		return "password must be at most 128 bytes"
	}
	return ""
}
