package auth

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

var (
	ErrNotFound    = errors.New("not found")
	ErrEmailTaken  = errors.New("email already registered")
	ErrTokenReused = errors.New("refresh token already used")
)

type User struct {
	ID              string     `json:"id"`
	Email           string     `json:"email"`
	FullName        string     `json:"full_name"`
	Role            Role       `json:"role"`
	Status          string     `json:"status"`
	EmailVerifiedAt *time.Time `json:"email_verified_at"`
	LastLoginAt     *time.Time `json:"last_login_at"`
	CreatedAt       time.Time  `json:"created_at"`
	PasswordHash    string     `json:"-"`
}

type Session struct {
	ID        string
	UserID    string
	ExpiresAt time.Time
	RevokedAt *time.Time
}

type Store struct{ db *sql.DB }

func NewStore(db *sql.DB) *Store { return &Store{db: db} }

func (s *Store) DB() *sql.DB { return s.db }

const userColumns = `id, email, full_name, role, status, email_verified_at, last_login_at, created_at, password_hash`

func scanUser(row interface{ Scan(...any) error }) (User, error) {
	var u User
	err := row.Scan(&u.ID, &u.Email, &u.FullName, &u.Role, &u.Status,
		&u.EmailVerifiedAt, &u.LastLoginAt, &u.CreatedAt, &u.PasswordHash)
	if errors.Is(err, sql.ErrNoRows) {
		return u, ErrNotFound
	}
	return u, err
}

func (s *Store) CreateUser(ctx context.Context, email, passwordHash, fullName string, role Role) (User, error) {
	u, err := scanUser(s.db.QueryRowContext(ctx, `
		INSERT INTO "qa-ev-users" (email, password_hash, full_name, role)
		VALUES ($1, $2, $3, $4)
		RETURNING `+userColumns, email, passwordHash, fullName, role))
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return u, ErrEmailTaken
	}
	return u, err
}

func (s *Store) UserByEmail(ctx context.Context, email string) (User, error) {
	return scanUser(s.db.QueryRowContext(ctx,
		`SELECT `+userColumns+` FROM "qa-ev-users" WHERE email = $1 AND deleted_at IS NULL`, email))
}

func (s *Store) UserByID(ctx context.Context, id string) (User, error) {
	return scanUser(s.db.QueryRowContext(ctx,
		`SELECT `+userColumns+` FROM "qa-ev-users" WHERE id = $1 AND deleted_at IS NULL`, id))
}

func (s *Store) ListUsers(ctx context.Context, limit, offset int) ([]User, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+userColumns+` FROM "qa-ev-users"
		WHERE deleted_at IS NULL ORDER BY created_at DESC LIMIT $1 OFFSET $2`, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	users := []User{}
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		users = append(users, u)
	}
	return users, rows.Err()
}

func (s *Store) HasAdmin(ctx context.Context) (bool, error) {
	var exists bool
	err := s.db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM "qa-ev-users" WHERE role = 'admin' AND deleted_at IS NULL)`).Scan(&exists)
	return exists, err
}

// RecordLogin stamps last_login_at and, when newHash is non-empty, upgrades the password hash.
func (s *Store) RecordLogin(ctx context.Context, userID, newHash string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE "qa-ev-users"
		SET last_login_at = now(), password_hash = COALESCE(NULLIF($2, ''), password_hash)
		WHERE id = $1`, userID, newHash)
	return err
}

// UpdateRole runs in tx so the caller can tag it with audit.SetActor first.
func (s *Store) UpdateRole(ctx context.Context, tx *sql.Tx, userID string, role Role) (User, error) {
	return scanUser(tx.QueryRowContext(ctx, `
		UPDATE "qa-ev-users" SET role = $2 WHERE id = $1 AND deleted_at IS NULL
		RETURNING `+userColumns, userID, role))
}

const insertSession = `
	INSERT INTO "qa-user_sessions" (user_id, refresh_token_hash, user_agent, ip_address, expires_at)
	VALUES ($1, $2, NULLIF($3, ''), NULLIF($4, '')::inet, $5)`

func (s *Store) CreateSession(ctx context.Context, userID, tokenHash, userAgent, ip string, expiresAt time.Time) error {
	_, err := s.db.ExecContext(ctx, insertSession, userID, tokenHash, userAgent, ip, expiresAt)
	return err
}

func (s *Store) SessionByTokenHash(ctx context.Context, tokenHash string) (Session, error) {
	var ss Session
	err := s.db.QueryRowContext(ctx, `
		SELECT id, user_id, expires_at, revoked_at FROM "qa-user_sessions"
		WHERE refresh_token_hash = $1`, tokenHash).Scan(&ss.ID, &ss.UserID, &ss.ExpiresAt, &ss.RevokedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ss, ErrNotFound
	}
	return ss, err
}

// RotateSession atomically revokes the old session and creates its replacement.
// It returns ErrTokenReused if a concurrent request already rotated the old session.
func (s *Store) RotateSession(ctx context.Context, oldID, userID, newHash, userAgent, ip string, expiresAt time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx,
		`UPDATE "qa-user_sessions" SET revoked_at = now() WHERE id = $1 AND revoked_at IS NULL`, oldID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrTokenReused
	}
	if _, err := tx.ExecContext(ctx, insertSession, userID, newHash, userAgent, ip, expiresAt); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) RevokeSession(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE "qa-user_sessions" SET revoked_at = now() WHERE id = $1 AND revoked_at IS NULL`, id)
	return err
}

func (s *Store) RevokeAllSessions(ctx context.Context, userID string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE "qa-user_sessions" SET revoked_at = now() WHERE user_id = $1 AND revoked_at IS NULL`, userID)
	return err
}
