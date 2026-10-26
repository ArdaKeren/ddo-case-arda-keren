-- +goose Up
-- Append-only audit trail. actor_user_id deliberately has no FK: deleting a user
-- must not cascade into (or be blocked by) immutable audit rows.
CREATE TABLE "qa-audit_logs" (
    id            bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    occurred_at   timestamptz NOT NULL DEFAULT now(),
    actor_user_id uuid,
    action        text        NOT NULL,
    entity_type   text        NOT NULL,
    entity_id     text,
    old_data      jsonb,
    new_data      jsonb,
    metadata      jsonb       NOT NULL DEFAULT '{}'::jsonb,
    ip_address    inet,
    user_agent    text,
    request_id    text
);

CREATE INDEX "qa-audit_logs_entity_idx"   ON "qa-audit_logs" (entity_type, entity_id, occurred_at DESC);
CREATE INDEX "qa-audit_logs_actor_idx"    ON "qa-audit_logs" (actor_user_id, occurred_at DESC);
CREATE INDEX "qa-audit_logs_occurred_idx" ON "qa-audit_logs" USING brin (occurred_at);

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION qa_audit_logs_block_mutation() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION '"qa-audit_logs" is append-only: % is not allowed', TG_OP
        USING ERRCODE = 'insufficient_privilege';
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER "qa-audit_logs_no_update_delete"
    BEFORE UPDATE OR DELETE ON "qa-audit_logs"
    FOR EACH ROW EXECUTE FUNCTION qa_audit_logs_block_mutation();

CREATE TRIGGER "qa-audit_logs_no_truncate"
    BEFORE TRUNCATE ON "qa-audit_logs"
    FOR EACH STATEMENT EXECUTE FUNCTION qa_audit_logs_block_mutation();

-- Defense in depth on top of the triggers; the owner role can still bypass these,
-- so the application should connect with a non-owner role in production.
REVOKE UPDATE, DELETE, TRUNCATE ON "qa-audit_logs" FROM PUBLIC;

-- +goose Down
DROP TABLE IF EXISTS "qa-audit_logs";
DROP FUNCTION IF EXISTS qa_audit_logs_block_mutation();
