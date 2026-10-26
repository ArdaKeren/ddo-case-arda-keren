-- +goose Up
-- Staff can call tickets but cannot manage users.
ALTER TABLE "qa-ev-users" DROP CONSTRAINT "qa-ev-users_role_chk";
ALTER TABLE "qa-ev-users" ADD CONSTRAINT "qa-ev-users_role_chk" CHECK (role IN ('admin', 'staff', 'user'));

CREATE TABLE "qa-services" (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    code       text        NOT NULL,
    name       text        NOT NULL,
    is_active  boolean     NOT NULL DEFAULT true,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),

    -- code prefixes the display number (A-007), so keep it short and upper-case.
    CONSTRAINT "qa-services_code_chk" CHECK (code ~ '^[A-Z]{1,3}$')
);
CREATE UNIQUE INDEX "qa-services_code_uq" ON "qa-services" (code);

CREATE TRIGGER "qa-services_set_updated_at"
    BEFORE UPDATE ON "qa-services"
    FOR EACH ROW EXECUTE FUNCTION qa_set_updated_at();

CREATE TABLE "qa-counters" (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    name       text        NOT NULL,
    is_active  boolean     NOT NULL DEFAULT true,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX "qa-counters_name_uq" ON "qa-counters" (name);

-- One row per service per day; the row lock taken by the upsert serializes
-- concurrent ticket creation so numbers are unique and gap-free.
CREATE TABLE "qa-ticket_sequences" (
    service_id  uuid    NOT NULL REFERENCES "qa-services" (id) ON DELETE CASCADE,
    service_day date    NOT NULL,
    last_number integer NOT NULL,
    PRIMARY KEY (service_id, service_day)
);

CREATE TABLE "qa-tickets" (
    id               uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    service_id       uuid        NOT NULL REFERENCES "qa-services" (id),
    service_day      date        NOT NULL,
    ticket_number    integer     NOT NULL,
    display_code     text        NOT NULL,
    status           text        NOT NULL DEFAULT 'waiting',
    customer_user_id uuid        REFERENCES "qa-ev-users" (id) ON DELETE SET NULL,
    counter_id       uuid        REFERENCES "qa-counters" (id),
    called_by        uuid        REFERENCES "qa-ev-users" (id) ON DELETE SET NULL,
    created_at       timestamptz NOT NULL DEFAULT now(),
    called_at        timestamptz,
    finished_at      timestamptz,

    CONSTRAINT "qa-tickets_status_chk"
        CHECK (status IN ('waiting', 'called', 'completed', 'no_show', 'cancelled')),
    CONSTRAINT "qa-tickets_called_chk"
        CHECK (status = 'waiting' OR status = 'cancelled' OR (called_at IS NOT NULL AND counter_id IS NOT NULL)),
    CONSTRAINT "qa-tickets_finished_chk"
        CHECK (status NOT IN ('completed', 'no_show') OR finished_at IS NOT NULL)
);

CREATE UNIQUE INDEX "qa-tickets_number_uq" ON "qa-tickets" (service_id, service_day, ticket_number);

-- Drives the call-next scan; stays small because only waiting tickets are indexed.
CREATE INDEX "qa-tickets_waiting_idx" ON "qa-tickets" (service_id, service_day, ticket_number)
    WHERE status = 'waiting';

-- Drives the wait-time percentile query.
CREATE INDEX "qa-tickets_called_idx" ON "qa-tickets" (service_id, called_at)
    WHERE called_at IS NOT NULL;

CREATE INDEX "qa-tickets_customer_idx" ON "qa-tickets" (customer_user_id, created_at DESC);

-- Stored responses for Idempotency-Key replays. Scoped per user and endpoint so
-- one client's key can never collide with, or reveal, another client's response.
CREATE TABLE "qa-idempotency_keys" (
    user_id       uuid        NOT NULL REFERENCES "qa-ev-users" (id) ON DELETE CASCADE,
    endpoint      text        NOT NULL,
    key           text        NOT NULL,
    request_hash  text        NOT NULL,
    status_code   integer     NOT NULL,
    -- bytea, not jsonb: replays must be byte-identical and jsonb reorders keys.
    response_body bytea       NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    expires_at    timestamptz NOT NULL,
    PRIMARY KEY (user_id, endpoint, key),
    CONSTRAINT "qa-idempotency_keys_key_chk" CHECK (length(key) BETWEEN 1 AND 255)
);
CREATE INDEX "qa-idempotency_keys_expires_idx" ON "qa-idempotency_keys" (expires_at);

-- +goose Down
DROP TABLE IF EXISTS "qa-idempotency_keys";
DROP TABLE IF EXISTS "qa-tickets";
DROP TABLE IF EXISTS "qa-ticket_sequences";
DROP TABLE IF EXISTS "qa-counters";
DROP TABLE IF EXISTS "qa-services";
UPDATE "qa-ev-users" SET role = 'user' WHERE role = 'staff';
ALTER TABLE "qa-ev-users" DROP CONSTRAINT "qa-ev-users_role_chk";
ALTER TABLE "qa-ev-users" ADD CONSTRAINT "qa-ev-users_role_chk" CHECK (role IN ('admin', 'user'));
