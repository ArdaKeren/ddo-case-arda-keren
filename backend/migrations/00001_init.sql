-- +goose Up
CREATE EXTENSION IF NOT EXISTS citext;

-- Shared trigger function that keeps updated_at current on every UPDATE.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION qa_set_updated_at() RETURNS trigger AS $$
BEGIN
    NEW.updated_at := now();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

-- +goose Down
DROP FUNCTION IF EXISTS qa_set_updated_at();
DROP EXTENSION IF EXISTS citext;
