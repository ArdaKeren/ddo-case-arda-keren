// Package audit writes application events to the append-only "qa-audit_logs" table.
package audit

import (
	"context"
	"database/sql"
	"encoding/json"
	"log"
	"net/http"

	"github.com/go-chi/chi/v5/middleware"

	"github.com/qa/backend/internal/httpx"
)

type Entry struct {
	ActorUserID string
	Action      string
	EntityType  string
	EntityID    string
	Metadata    map[string]any
}

type Logger struct{ db *sql.DB }

func New(db *sql.DB) *Logger { return &Logger{db: db} }

// Log records e with request context (IP, user agent, request id). Failures are
// logged rather than returned so auditing never breaks the user-facing request.
func (l *Logger) Log(r *http.Request, e Entry) {
	meta, err := json.Marshal(orEmpty(e.Metadata))
	if err != nil {
		meta = []byte("{}")
	}
	_, err = l.db.ExecContext(r.Context(), `
		INSERT INTO "qa-audit_logs"
			(actor_user_id, action, entity_type, entity_id, metadata, ip_address, user_agent, request_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		nullStr(e.ActorUserID), e.Action, e.EntityType, nullStr(e.EntityID), meta,
		nullStr(httpx.ClientIP(r)), nullStr(r.UserAgent()), nullStr(middleware.GetReqID(r.Context())),
	)
	if err != nil {
		log.Printf("audit: %s: %v", e.Action, err)
	}
}

// SetActor tags the transaction so the row-change trigger records who made the change.
func SetActor(ctx context.Context, tx *sql.Tx, actorID, requestID string) error {
	_, err := tx.ExecContext(ctx,
		`SELECT set_config('qa.actor_id', $1, true), set_config('qa.request_id', $2, true)`,
		actorID, requestID)
	return err
}

func orEmpty(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}
