// Package admin exposes privileged user-management and audit endpoints.
package admin

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/qa/backend/internal/audit"
	"github.com/qa/backend/internal/auth"
	"github.com/qa/backend/internal/httpx"
)

type Handler struct {
	store  *auth.Store
	tokens *auth.TokenIssuer
}

func NewHandler(store *auth.Store, tokens *auth.TokenIssuer) *Handler {
	return &Handler{store: store, tokens: tokens}
}

// Routes is mounted under /api/admin. Each route declares the permission it needs.
func (h *Handler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Use(auth.Authenticate(h.tokens))
	r.With(auth.RequirePermission(auth.PermUsersRead)).Get("/users", h.listUsers)
	r.With(auth.RequirePermission(auth.PermUsersWrite)).Patch("/users/{id}/role", h.updateRole)
	r.With(auth.RequirePermission(auth.PermAuditRead)).Get("/audit-logs", h.listAuditLogs)
	return r
}

func (h *Handler) listUsers(w http.ResponseWriter, r *http.Request) {
	limit, offset := page(r)
	users, err := h.store.ListUsers(r.Context(), limit, offset)
	if err != nil {
		internal(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"users": users, "limit": limit, "offset": offset})
}

func (h *Handler) updateRole(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Role auth.Role `json:"role"`
	}
	if err := httpx.Decode(w, r, &in); err != nil || !in.Role.Valid() {
		httpx.Error(w, http.StatusBadRequest, "role must be one of: admin, staff, display, user")
		return
	}
	actor, _ := auth.PrincipalFrom(r.Context())
	targetID := chi.URLParam(r, "id")
	if targetID == actor.UserID && in.Role != auth.RoleAdmin {
		httpx.Error(w, http.StatusBadRequest, "admins cannot demote themselves")
		return
	}

	tx, err := h.store.DB().BeginTx(r.Context(), nil)
	if err != nil {
		internal(w, err)
		return
	}
	defer tx.Rollback()

	// The row-change trigger on "qa-ev-users" picks up the actor from this setting.
	if err := audit.SetActor(r.Context(), tx, actor.UserID, middleware.GetReqID(r.Context())); err != nil {
		internal(w, err)
		return
	}
	u, err := h.store.UpdateRole(r.Context(), tx, targetID, in.Role)
	if errors.Is(err, auth.ErrNotFound) {
		httpx.Error(w, http.StatusNotFound, "user not found")
		return
	}
	if err != nil {
		internal(w, err)
		return
	}
	if err := tx.Commit(); err != nil {
		internal(w, err)
		return
	}
	// Existing sessions keep the old role until their access token expires;
	// revoke them so the change is enforced at the next refresh.
	if err := h.store.RevokeAllSessions(r.Context(), u.ID); err != nil {
		log.Printf("admin: revoke sessions after role change: %v", err)
	}
	httpx.JSON(w, http.StatusOK, u)
}

type auditRow struct {
	ID          int64           `json:"id"`
	OccurredAt  time.Time       `json:"occurred_at"`
	ActorUserID *string         `json:"actor_user_id"`
	Action      string          `json:"action"`
	EntityType  string          `json:"entity_type"`
	EntityID    *string         `json:"entity_id"`
	OldData     json.RawMessage `json:"old_data"`
	NewData     json.RawMessage `json:"new_data"`
	Metadata    json.RawMessage `json:"metadata"`
	IPAddress   *string         `json:"ip_address"`
	RequestID   *string         `json:"request_id"`
}

func (h *Handler) listAuditLogs(w http.ResponseWriter, r *http.Request) {
	limit, offset := page(r)
	rows, err := h.store.DB().QueryContext(r.Context(), `
		SELECT id, occurred_at, actor_user_id, action, entity_type, entity_id,
		       old_data, new_data, metadata, host(ip_address), request_id
		FROM "qa-audit_logs" ORDER BY id DESC LIMIT $1 OFFSET $2`, limit, offset)
	if err != nil {
		internal(w, err)
		return
	}
	defer rows.Close()

	logs := []auditRow{}
	for rows.Next() {
		var a auditRow
		var oldData, newData []byte
		if err := rows.Scan(&a.ID, &a.OccurredAt, &a.ActorUserID, &a.Action, &a.EntityType, &a.EntityID,
			&oldData, &newData, &a.Metadata, &a.IPAddress, &a.RequestID); err != nil {
			internal(w, err)
			return
		}
		a.OldData, a.NewData = rawOrNull(oldData), rawOrNull(newData)
		logs = append(logs, a)
	}
	if err := rows.Err(); err != nil {
		internal(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"audit_logs": logs, "limit": limit, "offset": offset})
}

func page(r *http.Request) (limit, offset int) {
	limit, _ = strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ = strconv.Atoi(r.URL.Query().Get("offset"))
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	return limit, offset
}

func rawOrNull(b []byte) json.RawMessage {
	if b == nil {
		return json.RawMessage("null")
	}
	return b
}

func internal(w http.ResponseWriter, err error) {
	log.Printf("admin: %v", err)
	httpx.Error(w, http.StatusInternalServerError, "internal error")
}
