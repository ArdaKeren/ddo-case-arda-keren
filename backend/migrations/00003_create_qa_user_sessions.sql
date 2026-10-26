-- +goose Up
CREATE TABLE "qa-user_sessions" (
    id                 uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id            uuid        NOT NULL REFERENCES "qa-ev-users" (id) ON DELETE CASCADE,
    refresh_token_hash text        NOT NULL,
    user_agent         text,
    ip_address         inet,
    expires_at         timestamptz NOT NULL,
    revoked_at         timestamptz,
    created_at         timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX "qa-user_sessions_token_uq" ON "qa-user_sessions" (refresh_token_hash);
CREATE INDEX "qa-user_sessions_user_idx" ON "qa-user_sessions" (user_id) WHERE revoked_at IS NULL;

-- +goose Down
DROP TABLE IF EXISTS "qa-user_sessions";
