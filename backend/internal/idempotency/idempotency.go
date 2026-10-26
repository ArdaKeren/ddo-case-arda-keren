// Package idempotency implements Idempotency-Key handling on top of "qa-idempotency_keys".
//
// The key is claimed inside the same transaction that performs the side effect and
// the response is stored before commit. A concurrent request with the same key
// blocks on the primary-key insert until the first transaction finishes, then either
// replays the committed response or, if the first one rolled back, claims the key
// itself. Clients therefore never observe a half-finished request.
package idempotency

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"
)

const (
	Header         = "Idempotency-Key"
	ReplayedHeader = "Idempotent-Replayed"
	TTL            = 24 * time.Hour
	maxKeyLen      = 255
)

var (
	ErrKeyReused  = errors.New("idempotency key was already used with a different request")
	ErrInProgress = errors.New("a request with this idempotency key is still in progress")
)

type Stored struct {
	StatusCode int
	Body       json.RawMessage
}

// ValidKey accepts 1-255 printable ASCII characters (UUIDs, ULIDs, etc.).
func ValidKey(key string) bool {
	if len(key) == 0 || len(key) > maxKeyLen {
		return false
	}
	for i := 0; i < len(key); i++ {
		if key[i] < 0x21 || key[i] > 0x7e {
			return false
		}
	}
	return true
}

// RequestHash fingerprints the request so a key cannot be reused for a different payload.
// JSON bodies are compacted first so whitespace differences do not count as a change.
func RequestHash(method, path string, body []byte) string {
	var compact bytes.Buffer
	if err := json.Compact(&compact, body); err != nil {
		compact.Reset()
		compact.Write(body)
	}
	h := sha256.New()
	h.Write([]byte(method + " " + path + "\n"))
	h.Write(compact.Bytes())
	return hex.EncodeToString(h.Sum(nil))
}

// Claim reserves (userID, endpoint, key) within tx.
//
// It returns (nil, nil) when the caller now owns the key and must do the work and
// call Save before committing. It returns a non-nil *Stored when an earlier request
// already completed and the caller should replay that response verbatim.
func Claim(ctx context.Context, tx *sql.Tx, userID, endpoint, key, requestHash string) (*Stored, error) {
	// Expired keys are reclaimable; deleting here avoids depending on a cleanup job.
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM "qa-idempotency_keys"
		WHERE user_id = $1 AND endpoint = $2 AND key = $3 AND expires_at <= now()`,
		userID, endpoint, key); err != nil {
		return nil, err
	}

	// status_code 0 marks a claim that has no response yet. Because Save runs in the
	// same transaction, other sessions only ever see this row once it is complete.
	var claimed bool
	err := tx.QueryRowContext(ctx, `
		INSERT INTO "qa-idempotency_keys" (user_id, endpoint, key, request_hash, status_code, response_body, expires_at)
		VALUES ($1, $2, $3, $4, 0, ''::bytea, now() + make_interval(secs => $5))
		ON CONFLICT (user_id, endpoint, key) DO NOTHING
		RETURNING true`,
		userID, endpoint, key, requestHash, TTL.Seconds()).Scan(&claimed)
	if err == nil {
		return nil, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}

	var (
		storedHash string
		s          Stored
	)
	if err := tx.QueryRowContext(ctx, `
		SELECT request_hash, status_code, response_body FROM "qa-idempotency_keys"
		WHERE user_id = $1 AND endpoint = $2 AND key = $3`,
		userID, endpoint, key).Scan(&storedHash, &s.StatusCode, &s.Body); err != nil {
		return nil, err
	}
	if storedHash != requestHash {
		return nil, ErrKeyReused
	}
	if s.StatusCode == 0 {
		return nil, ErrInProgress
	}
	return &s, nil
}

// Save records the response for a key previously claimed in the same tx.
func Save(ctx context.Context, tx *sql.Tx, userID, endpoint, key string, statusCode int, body []byte) error {
	_, err := tx.ExecContext(ctx, `
		UPDATE "qa-idempotency_keys" SET status_code = $4, response_body = $5
		WHERE user_id = $1 AND endpoint = $2 AND key = $3`,
		userID, endpoint, key, statusCode, body)
	return err
}

// DeleteExpired is for a periodic cleanup job; Claim already handles expiry lazily.
func DeleteExpired(ctx context.Context, db *sql.DB) (int64, error) {
	res, err := db.ExecContext(ctx, `DELETE FROM "qa-idempotency_keys" WHERE expires_at <= now()`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
