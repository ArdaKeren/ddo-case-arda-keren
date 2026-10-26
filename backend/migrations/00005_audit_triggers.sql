-- +goose Up
-- Generic row-change auditor. The app identifies the actor per transaction with:
--   SET LOCAL qa.actor_id = '<uuid>';  SET LOCAL qa.request_id = '<id>';
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION qa_audit_row_change() RETURNS trigger AS $$
DECLARE
    v_old   jsonb;
    v_new   jsonb;
    v_actor text := nullif(current_setting('qa.actor_id', true), '');
BEGIN
    IF TG_OP IN ('UPDATE', 'DELETE') THEN
        v_old := to_jsonb(OLD) - 'password_hash';
    END IF;
    IF TG_OP IN ('INSERT', 'UPDATE') THEN
        v_new := to_jsonb(NEW) - 'password_hash';
    END IF;

    -- Skip no-op updates (e.g. only updated_at changed).
    IF TG_OP = 'UPDATE' AND (v_old - 'updated_at') = (v_new - 'updated_at') THEN
        RETURN NEW;
    END IF;

    INSERT INTO "qa-audit_logs" (actor_user_id, action, entity_type, entity_id, old_data, new_data, request_id)
    VALUES (
        v_actor::uuid,
        lower(TG_OP),
        TG_TABLE_NAME,
        coalesce(v_new ->> 'id', v_old ->> 'id'),
        v_old,
        v_new,
        nullif(current_setting('qa.request_id', true), '')
    );

    RETURN coalesce(NEW, OLD);
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER "qa-ev-users_audit"
    AFTER INSERT OR UPDATE OR DELETE ON "qa-ev-users"
    FOR EACH ROW EXECUTE FUNCTION qa_audit_row_change();

-- +goose Down
DROP TRIGGER IF EXISTS "qa-ev-users_audit" ON "qa-ev-users";
DROP FUNCTION IF EXISTS qa_audit_row_change();
