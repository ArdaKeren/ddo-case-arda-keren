-- +goose Up
CREATE TABLE "qa-ev-users" (
    id                uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    email             citext      NOT NULL,
    password_hash     text        NOT NULL,
    full_name         text        NOT NULL,
    role              text        NOT NULL DEFAULT 'user',
    status            text        NOT NULL DEFAULT 'active',
    email_verified_at timestamptz,
    last_login_at     timestamptz,
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now(),
    deleted_at        timestamptz,

    CONSTRAINT "qa-ev-users_role_chk"   CHECK (role IN ('admin', 'user')),
    CONSTRAINT "qa-ev-users_status_chk" CHECK (status IN ('active', 'suspended', 'pending'))
);

-- Email is unique among non-deleted users only, so a soft-deleted address can be reused.
CREATE UNIQUE INDEX "qa-ev-users_email_uq" ON "qa-ev-users" (email) WHERE deleted_at IS NULL;

CREATE TRIGGER "qa-ev-users_set_updated_at"
    BEFORE UPDATE ON "qa-ev-users"
    FOR EACH ROW EXECUTE FUNCTION qa_set_updated_at();

-- +goose Down
DROP TABLE IF EXISTS "qa-ev-users";
