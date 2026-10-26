-- +goose Up
-- "display" is for lobby screens: read-only access to the live queue.
ALTER TABLE "qa-ev-users" DROP CONSTRAINT "qa-ev-users_role_chk";
ALTER TABLE "qa-ev-users" ADD CONSTRAINT "qa-ev-users_role_chk"
    CHECK (role IN ('admin', 'staff', 'display', 'user'));

CREATE TABLE "qa-branches" (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    code       text        NOT NULL,
    name       text        NOT NULL,
    is_active  boolean     NOT NULL DEFAULT true,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT "qa-branches_code_chk" CHECK (code ~ '^[A-Z0-9-]{2,16}$')
);
CREATE UNIQUE INDEX "qa-branches_code_uq" ON "qa-branches" (code);

CREATE TRIGGER "qa-branches_set_updated_at"
    BEFORE UPDATE ON "qa-branches"
    FOR EACH ROW EXECUTE FUNCTION qa_set_updated_at();

-- Rows created before branches existed are attached to a default branch.
INSERT INTO "qa-branches" (code, name)
SELECT 'MERKEZ', 'Merkez Şube'
WHERE EXISTS (SELECT 1 FROM "qa-services") OR EXISTS (SELECT 1 FROM "qa-counters");

ALTER TABLE "qa-services" ADD COLUMN branch_id uuid REFERENCES "qa-branches" (id);
UPDATE "qa-services" SET branch_id = (SELECT id FROM "qa-branches" WHERE code = 'MERKEZ');
ALTER TABLE "qa-services" ALTER COLUMN branch_id SET NOT NULL;
DROP INDEX "qa-services_code_uq";
CREATE UNIQUE INDEX "qa-services_branch_code_uq" ON "qa-services" (branch_id, code);

ALTER TABLE "qa-counters" ADD COLUMN branch_id uuid REFERENCES "qa-branches" (id);
UPDATE "qa-counters" SET branch_id = (SELECT id FROM "qa-branches" WHERE code = 'MERKEZ');
ALTER TABLE "qa-counters" ALTER COLUMN branch_id SET NOT NULL;
DROP INDEX "qa-counters_name_uq";
CREATE UNIQUE INDEX "qa-counters_branch_name_uq" ON "qa-counters" (branch_id, name);

-- Denormalized so board snapshots and events need no join through services.
ALTER TABLE "qa-tickets" ADD COLUMN branch_id uuid REFERENCES "qa-branches" (id);
UPDATE "qa-tickets" t SET branch_id = s.branch_id FROM "qa-services" s WHERE s.id = t.service_id;
ALTER TABLE "qa-tickets" ALTER COLUMN branch_id SET NOT NULL;
CREATE INDEX "qa-tickets_branch_board_idx" ON "qa-tickets" (branch_id, status)
    WHERE status IN ('waiting', 'called');

CREATE TABLE "qa-appointments" (
    id               uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    branch_id        uuid        NOT NULL REFERENCES "qa-branches" (id),
    service_id       uuid        NOT NULL REFERENCES "qa-services" (id),
    customer_user_id uuid        REFERENCES "qa-ev-users" (id) ON DELETE SET NULL,
    scheduled_at     timestamptz NOT NULL,
    status           text        NOT NULL DEFAULT 'booked',
    ticket_id        uuid        REFERENCES "qa-tickets" (id),
    checked_in_at    timestamptz,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT "qa-appointments_status_chk"
        CHECK (status IN ('booked', 'checked_in', 'cancelled', 'no_show')),
    CONSTRAINT "qa-appointments_checkin_chk"
        CHECK (status <> 'checked_in' OR (ticket_id IS NOT NULL AND checked_in_at IS NOT NULL))
);
-- A customer cannot hold two live bookings for the same service and slot.
CREATE UNIQUE INDEX "qa-appointments_customer_slot_uq"
    ON "qa-appointments" (customer_user_id, service_id, scheduled_at)
    WHERE status IN ('booked', 'checked_in');
CREATE INDEX "qa-appointments_branch_time_idx" ON "qa-appointments" (branch_id, scheduled_at);

CREATE TRIGGER "qa-appointments_set_updated_at"
    BEFORE UPDATE ON "qa-appointments"
    FOR EACH ROW EXECUTE FUNCTION qa_set_updated_at();

-- Ordered event log that WebSocket clients replay from after reconnecting.
CREATE TABLE "qa-queue_events" (
    id          bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    branch_id   uuid        NOT NULL REFERENCES "qa-branches" (id) ON DELETE CASCADE,
    event_type  text        NOT NULL,
    payload     jsonb       NOT NULL,
    occurred_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX "qa-queue_events_branch_idx" ON "qa-queue_events" (branch_id, id);
CREATE INDEX "qa-queue_events_occurred_idx" ON "qa-queue_events" USING brin (occurred_at);

-- qa_emit_event appends an event and notifies listeners.
--
-- The per-branch transaction-scoped advisory lock is what makes recovery safe: it
-- is held until commit, so within one branch event ids become visible strictly in
-- id order. A reader that has seen id N can therefore ask for "id > N" and never
-- miss an event that commits later with a smaller id. Call it as the last write of
-- a transaction to keep the serialized section short.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION qa_emit_event(p_branch uuid, p_type text, p_payload jsonb) RETURNS bigint AS $$
DECLARE
    v_id bigint;
BEGIN
    PERFORM pg_advisory_xact_lock(hashtextextended('qa-queue_events:' || p_branch::text, 0));
    INSERT INTO "qa-queue_events" (branch_id, event_type, payload)
    VALUES (p_branch, p_type, p_payload)
    RETURNING id INTO v_id;
    PERFORM pg_notify('qa_queue_events', p_branch::text);
    RETURN v_id;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

-- Emits a ticket event with the public, display-safe view of the ticket
-- (no customer identity).
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION qa_emit_ticket_event(p_ticket uuid, p_type text) RETURNS bigint AS $$
DECLARE
    v_branch  uuid;
    v_payload jsonb;
BEGIN
    SELECT t.branch_id, jsonb_build_object('ticket', jsonb_build_object(
               'id', t.id,
               'display_code', t.display_code,
               'service_id', t.service_id,
               'service_code', s.code,
               'status', t.status,
               'counter_id', t.counter_id,
               'counter_name', c.name,
               'created_at', t.created_at,
               'called_at', t.called_at,
               'finished_at', t.finished_at))
    INTO v_branch, v_payload
    FROM "qa-tickets" t
    JOIN "qa-services" s ON s.id = t.service_id
    LEFT JOIN "qa-counters" c ON c.id = t.counter_id
    WHERE t.id = p_ticket;

    IF v_branch IS NULL THEN
        RAISE EXCEPTION 'ticket % not found', p_ticket;
    END IF;
    RETURN qa_emit_event(v_branch, p_type, v_payload);
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

-- +goose Down
DROP FUNCTION IF EXISTS qa_emit_ticket_event(uuid, text);
DROP FUNCTION IF EXISTS qa_emit_event(uuid, text, jsonb);
DROP TABLE IF EXISTS "qa-queue_events";
DROP TABLE IF EXISTS "qa-appointments";
DROP INDEX IF EXISTS "qa-tickets_branch_board_idx";
ALTER TABLE "qa-tickets" DROP COLUMN IF EXISTS branch_id;
DROP INDEX IF EXISTS "qa-counters_branch_name_uq";
ALTER TABLE "qa-counters" DROP COLUMN IF EXISTS branch_id;
CREATE UNIQUE INDEX "qa-counters_name_uq" ON "qa-counters" (name);
DROP INDEX IF EXISTS "qa-services_branch_code_uq";
ALTER TABLE "qa-services" DROP COLUMN IF EXISTS branch_id;
CREATE UNIQUE INDEX "qa-services_code_uq" ON "qa-services" (code);
DROP TABLE IF EXISTS "qa-branches";
UPDATE "qa-ev-users" SET role = 'user' WHERE role = 'display';
ALTER TABLE "qa-ev-users" DROP CONSTRAINT "qa-ev-users_role_chk";
ALTER TABLE "qa-ev-users" ADD CONSTRAINT "qa-ev-users_role_chk"
    CHECK (role IN ('admin', 'staff', 'user'));
