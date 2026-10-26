// Package queue implements branches, ticket issuing, the call-next engine,
// appointments, the replayable event log and wait-time statistics.
package queue

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

var (
	ErrNotFound          = errors.New("not found")
	ErrBranchNotFound    = errors.New("branch not found or inactive")
	ErrServiceNotFound   = errors.New("service not found or inactive")
	ErrCounterNotFound   = errors.New("counter not found, inactive or in another branch")
	ErrQueueEmpty        = errors.New("no waiting tickets")
	ErrInvalidTransition = errors.New("ticket is not in a state that allows this action")
	ErrDuplicate         = errors.New("already exists")
)

const (
	StatusWaiting   = "waiting"
	StatusCalled    = "called"
	StatusCompleted = "completed"
	StatusNoShow    = "no_show"
	StatusCancelled = "cancelled"
)

// Event types written to "qa-queue_events".
const (
	EventTicketCreated        = "ticket.created"
	EventTicketCalled         = "ticket.called"
	EventTicketFinished       = "ticket.finished"
	EventAppointmentCreated   = "appointment.created"
	EventAppointmentCheckedIn = "appointment.checked_in"
)

type Branch struct {
	ID        string    `json:"id"`
	Code      string    `json:"code"`
	Name      string    `json:"name"`
	IsActive  bool      `json:"is_active"`
	CreatedAt time.Time `json:"created_at"`
}

type Service struct {
	ID        string    `json:"id"`
	BranchID  string    `json:"branch_id"`
	Code      string    `json:"code"`
	Name      string    `json:"name"`
	IsActive  bool      `json:"is_active"`
	CreatedAt time.Time `json:"created_at"`
}

type Counter struct {
	ID        string    `json:"id"`
	BranchID  string    `json:"branch_id"`
	Name      string    `json:"name"`
	IsActive  bool      `json:"is_active"`
	CreatedAt time.Time `json:"created_at"`
}

type Ticket struct {
	ID             string     `json:"id"`
	BranchID       string     `json:"branch_id"`
	ServiceID      string     `json:"service_id"`
	ServiceDay     string     `json:"service_day"`
	TicketNumber   int        `json:"ticket_number"`
	DisplayCode    string     `json:"display_code"`
	Status         string     `json:"status"`
	CustomerUserID *string    `json:"customer_user_id"`
	CounterID      *string    `json:"counter_id"`
	CalledBy       *string    `json:"called_by"`
	CreatedAt      time.Time  `json:"created_at"`
	CalledAt       *time.Time `json:"called_at"`
	FinishedAt     *time.Time `json:"finished_at"`
}

// TicketView is the display-safe projection used in board snapshots and events.
// It must stay in sync with qa_emit_ticket_event in migration 00007.
type TicketView struct {
	ID          string     `json:"id"`
	DisplayCode string     `json:"display_code"`
	ServiceID   string     `json:"service_id"`
	ServiceCode string     `json:"service_code"`
	Status      string     `json:"status"`
	CounterID   *string    `json:"counter_id"`
	CounterName *string    `json:"counter_name"`
	CreatedAt   time.Time  `json:"created_at"`
	CalledAt    *time.Time `json:"called_at"`
	FinishedAt  *time.Time `json:"finished_at"`
}

// Board is a consistent snapshot of a branch's live tickets together with the id
// of the last event already reflected in it.
type Board struct {
	BranchID    string       `json:"branch_id"`
	LastEventID int64        `json:"last_event_id"`
	Tickets     []TicketView `json:"tickets"`
}

type Event struct {
	ID         int64     `json:"id"`
	BranchID   string    `json:"branch_id"`
	Type       string    `json:"event_type"`
	Payload    []byte    `json:"-"`
	OccurredAt time.Time `json:"occurred_at"`
}

// WaitStats summarizes how long tickets waited (created_at → called_at) in a window.
// Median and P90 are nil when the window has no called tickets.
type WaitStats struct {
	ServiceID     string   `json:"service_id"`
	WindowSeconds int64    `json:"window_seconds"`
	SampleSize    int64    `json:"sample_size"`
	MedianSeconds *float64 `json:"median_seconds"`
	P90Seconds    *float64 `json:"p90_seconds"`
	WaitingNow    int64    `json:"waiting_now"`
}

// querier is satisfied by both *sql.DB and *sql.Tx.
type querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

type Repository struct {
	db *sql.DB
	// Business-day boundary for ticket numbering, e.g. "Europe/Istanbul".
	timezone string
}

func NewRepository(db *sql.DB, timezone string) *Repository {
	return &Repository{db: db, timezone: timezone}
}

func (r *Repository) DB() *sql.DB { return r.db }

const ticketColumns = `id, branch_id, service_id, service_day::text, ticket_number, display_code, status,
	customer_user_id, counter_id, called_by, created_at, called_at, finished_at`

func scanTicket(row interface{ Scan(...any) error }) (Ticket, error) {
	var t Ticket
	err := row.Scan(&t.ID, &t.BranchID, &t.ServiceID, &t.ServiceDay, &t.TicketNumber, &t.DisplayCode, &t.Status,
		&t.CustomerUserID, &t.CounterID, &t.CalledBy, &t.CreatedAt, &t.CalledAt, &t.FinishedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return t, ErrNotFound
	}
	return t, err
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// emitTicketEvent must be the last write in tx; see qa_emit_event.
func emitTicketEvent(ctx context.Context, tx *sql.Tx, ticketID, eventType string) error {
	_, err := tx.ExecContext(ctx, `SELECT qa_emit_ticket_event($1, $2)`, ticketID, eventType)
	return err
}

func (r *Repository) CreateBranch(ctx context.Context, code, name string) (Branch, error) {
	var b Branch
	err := r.db.QueryRowContext(ctx, `
		INSERT INTO "qa-branches" (code, name) VALUES ($1, $2)
		RETURNING id, code, name, is_active, created_at`, code, name).
		Scan(&b.ID, &b.Code, &b.Name, &b.IsActive, &b.CreatedAt)
	if isUniqueViolation(err) {
		return b, ErrDuplicate
	}
	return b, err
}

func (r *Repository) ListBranches(ctx context.Context, activeOnly bool) ([]Branch, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, code, name, is_active, created_at FROM "qa-branches"
		WHERE is_active OR NOT $1 ORDER BY code`, activeOnly)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Branch{}
	for rows.Next() {
		var b Branch
		if err := rows.Scan(&b.ID, &b.Code, &b.Name, &b.IsActive, &b.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func (r *Repository) BranchExists(ctx context.Context, branchID string) (bool, error) {
	var ok bool
	err := r.db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM "qa-branches" WHERE id = $1 AND is_active)`, branchID).Scan(&ok)
	return ok, err
}

func (r *Repository) CreateService(ctx context.Context, branchID, code, name string) (Service, error) {
	var s Service
	err := r.db.QueryRowContext(ctx, `
		INSERT INTO "qa-services" (branch_id, code, name)
		SELECT id, $2, $3 FROM "qa-branches" WHERE id = $1 AND is_active
		RETURNING id, branch_id, code, name, is_active, created_at`, branchID, code, name).
		Scan(&s.ID, &s.BranchID, &s.Code, &s.Name, &s.IsActive, &s.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return s, ErrBranchNotFound
	}
	if isUniqueViolation(err) {
		return s, ErrDuplicate
	}
	return s, err
}

// ListServices returns services of one branch, or of all branches when branchID is "".
func (r *Repository) ListServices(ctx context.Context, branchID string, activeOnly bool) ([]Service, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, branch_id, code, name, is_active, created_at FROM "qa-services"
		WHERE (branch_id = $1::uuid OR $1 IS NULL) AND (is_active OR NOT $2)
		ORDER BY branch_id, code`, nullStr(branchID), activeOnly)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Service{}
	for rows.Next() {
		var s Service
		if err := rows.Scan(&s.ID, &s.BranchID, &s.Code, &s.Name, &s.IsActive, &s.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (r *Repository) CreateCounter(ctx context.Context, branchID, name string) (Counter, error) {
	var c Counter
	err := r.db.QueryRowContext(ctx, `
		INSERT INTO "qa-counters" (branch_id, name)
		SELECT id, $2 FROM "qa-branches" WHERE id = $1 AND is_active
		RETURNING id, branch_id, name, is_active, created_at`, branchID, name).
		Scan(&c.ID, &c.BranchID, &c.Name, &c.IsActive, &c.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return c, ErrBranchNotFound
	}
	if isUniqueViolation(err) {
		return c, ErrDuplicate
	}
	return c, err
}

func (r *Repository) ListCounters(ctx context.Context, branchID string) ([]Counter, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, branch_id, name, is_active, created_at FROM "qa-counters"
		WHERE branch_id = $1 ORDER BY name`, branchID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Counter{}
	for rows.Next() {
		var c Counter
		if err := rows.Scan(&c.ID, &c.BranchID, &c.Name, &c.IsActive, &c.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// CreateTicket issues the next number for serviceID within tx and emits ticket.created.
//
// The sequence upsert takes a row lock on ("qa-ticket_sequences", service, day), so
// concurrent issuers for the same service serialize on that row only for the
// remainder of their transaction, and numbers stay unique and gap-free. Keep the
// surrounding transaction short.
func (r *Repository) CreateTicket(ctx context.Context, tx *sql.Tx, serviceID string, customerUserID *string) (Ticket, error) {
	var code, branchID, day string
	err := tx.QueryRowContext(ctx, `
		SELECT s.code, s.branch_id, (now() AT TIME ZONE $2)::date::text
		FROM "qa-services" s JOIN "qa-branches" b ON b.id = s.branch_id
		WHERE s.id = $1 AND s.is_active AND b.is_active`, serviceID, r.timezone).Scan(&code, &branchID, &day)
	if errors.Is(err, sql.ErrNoRows) {
		return Ticket{}, ErrServiceNotFound
	}
	if err != nil {
		return Ticket{}, err
	}

	var number int
	if err := tx.QueryRowContext(ctx, `
		INSERT INTO "qa-ticket_sequences" AS s (service_id, service_day, last_number)
		VALUES ($1, $2::date, 1)
		ON CONFLICT (service_id, service_day) DO UPDATE SET last_number = s.last_number + 1
		RETURNING last_number`, serviceID, day).Scan(&number); err != nil {
		return Ticket{}, err
	}

	// %03d pads to three digits but, unlike SQL lpad, never truncates numbers above 999.
	t, err := scanTicket(tx.QueryRowContext(ctx, `
		INSERT INTO "qa-tickets" (branch_id, service_id, service_day, ticket_number, display_code, customer_user_id)
		VALUES ($1, $2, $3::date, $4, $5, $6)
		RETURNING `+ticketColumns,
		branchID, serviceID, day, number, fmt.Sprintf("%s-%03d", code, number), customerUserID))
	if err != nil {
		return t, err
	}
	return t, emitTicketEvent(ctx, tx, t.ID, EventTicketCreated)
}

func (r *Repository) TicketByID(ctx context.Context, id string) (Ticket, error) {
	return scanTicket(r.db.QueryRowContext(ctx, `SELECT `+ticketColumns+` FROM "qa-tickets" WHERE id = $1`, id))
}

// Position returns how many waiting tickets of the same service are ahead of t
// (0 = next to be called). Only meaningful while t is waiting.
func (r *Repository) Position(ctx context.Context, q querier, t Ticket) (int64, error) {
	var n int64
	err := q.QueryRowContext(ctx, `
		SELECT count(*) FROM "qa-tickets"
		WHERE service_id = $1 AND status = 'waiting'
		  AND (service_day, ticket_number) < ($2::date, $3)`,
		t.ServiceID, t.ServiceDay, t.TicketNumber).Scan(&n)
	return n, err
}

// CallNext atomically assigns the oldest waiting ticket of serviceID to counterID.
//
// FOR UPDATE SKIP LOCKED makes concurrent callers each lock a *different* row
// instead of all reading the same "next" ticket: a row another transaction has
// already locked is skipped rather than waited on. Combined with the status
// re-check in the UPDATE, a ticket can never be handed to two counters, and callers
// never block one another. Returns ErrQueueEmpty when nothing is left to call.
func (r *Repository) CallNext(ctx context.Context, serviceID, counterID, staffUserID string) (Ticket, error) {
	var ok bool
	err := r.db.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM "qa-counters" c JOIN "qa-services" s ON s.branch_id = c.branch_id
			WHERE c.id = $1 AND c.is_active AND s.id = $2)`, counterID, serviceID).Scan(&ok)
	if err != nil {
		return Ticket{}, err
	}
	if !ok {
		return Ticket{}, ErrCounterNotFound
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return Ticket{}, err
	}
	defer tx.Rollback()

	t, err := scanTicket(tx.QueryRowContext(ctx, `
		WITH next AS (
			SELECT id AS next_id FROM "qa-tickets"
			WHERE service_id = $1 AND status = 'waiting'
			ORDER BY service_day, ticket_number
			LIMIT 1
			FOR UPDATE SKIP LOCKED
		)
		UPDATE "qa-tickets"
		SET status = 'called', called_at = now(), counter_id = $2, called_by = $3
		FROM next
		WHERE id = next.next_id AND status = 'waiting'
		RETURNING `+ticketColumns, serviceID, counterID, nullStr(staffUserID)))
	if errors.Is(err, ErrNotFound) {
		return Ticket{}, ErrQueueEmpty
	}
	if err != nil {
		return Ticket{}, err
	}
	if err := emitTicketEvent(ctx, tx, t.ID, EventTicketCalled); err != nil {
		return Ticket{}, err
	}
	return t, tx.Commit()
}

// Finish moves a called ticket to completed or no_show.
func (r *Repository) Finish(ctx context.Context, ticketID, outcome string) (Ticket, error) {
	if outcome != StatusCompleted && outcome != StatusNoShow {
		return Ticket{}, ErrInvalidTransition
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return Ticket{}, err
	}
	defer tx.Rollback()

	t, err := scanTicket(tx.QueryRowContext(ctx, `
		UPDATE "qa-tickets" SET status = $2, finished_at = now()
		WHERE id = $1 AND status = 'called'
		RETURNING `+ticketColumns, ticketID, outcome))
	if errors.Is(err, ErrNotFound) {
		if _, lookupErr := r.TicketByID(ctx, ticketID); lookupErr == nil {
			return Ticket{}, ErrInvalidTransition
		}
		return Ticket{}, ErrNotFound
	}
	if err != nil {
		return Ticket{}, err
	}
	if err := emitTicketEvent(ctx, tx, t.ID, EventTicketFinished); err != nil {
		return Ticket{}, err
	}
	return t, tx.Commit()
}

// Board returns the branch's waiting tickets and recently called ones, plus the
// last event id, read from one REPEATABLE READ snapshot so that replaying events
// with id > LastEventID on top of it yields exactly the current state.
func (r *Repository) Board(ctx context.Context, branchID string) (Board, error) {
	b := Board{BranchID: branchID, Tickets: []TicketView{}}
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return b, err
	}
	defer tx.Rollback()

	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(id), 0) FROM "qa-queue_events" WHERE branch_id = $1`, branchID).Scan(&b.LastEventID); err != nil {
		return b, err
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT t.id, t.display_code, t.service_id, s.code, t.status, t.counter_id, c.name,
		       t.created_at, t.called_at, t.finished_at
		FROM "qa-tickets" t
		JOIN "qa-services" s ON s.id = t.service_id
		LEFT JOIN "qa-counters" c ON c.id = t.counter_id
		WHERE t.branch_id = $1
		  AND (t.status = 'waiting' OR (t.status = 'called' AND t.called_at > now() - interval '2 hours'))
		ORDER BY t.service_day, t.created_at, t.ticket_number
		LIMIT 1000`, branchID)
	if err != nil {
		return b, err
	}
	defer rows.Close()
	for rows.Next() {
		var v TicketView
		if err := rows.Scan(&v.ID, &v.DisplayCode, &v.ServiceID, &v.ServiceCode, &v.Status, &v.CounterID,
			&v.CounterName, &v.CreatedAt, &v.CalledAt, &v.FinishedAt); err != nil {
			return b, err
		}
		b.Tickets = append(b.Tickets, v)
	}
	if err := rows.Err(); err != nil {
		return b, err
	}
	return b, tx.Commit()
}

// EventsAfter returns up to limit events of branchID with id > afterID, oldest first.
func (r *Repository) EventsAfter(ctx context.Context, branchID string, afterID int64, limit int) ([]Event, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, branch_id, event_type, payload, occurred_at FROM "qa-queue_events"
		WHERE branch_id = $1 AND id > $2 ORDER BY id LIMIT $3`, branchID, afterID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.ID, &e.BranchID, &e.Type, &e.Payload, &e.OccurredAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// CanResumeFrom reports whether a client that last saw eventID can be brought up
// to date by replay alone: its last event must still be retained (retention
// deletes oldest-first, so everything after it is retained too) and the backlog
// must not exceed maxBacklog.
func (r *Repository) CanResumeFrom(ctx context.Context, branchID string, eventID int64, maxBacklog int) (bool, error) {
	var exists bool
	var backlog int
	err := r.db.QueryRowContext(ctx, `
		SELECT
			EXISTS (SELECT 1 FROM "qa-queue_events" WHERE branch_id = $1 AND id = $2),
			(SELECT count(*) FROM (
				SELECT 1 FROM "qa-queue_events" WHERE branch_id = $1 AND id > $2 LIMIT $3 + 1) x)`,
		branchID, eventID, maxBacklog).Scan(&exists, &backlog)
	if err != nil {
		return false, err
	}
	return exists && backlog <= maxBacklog, nil
}

// LatestEventID returns the newest event id of branchID, or 0.
func (r *Repository) LatestEventID(ctx context.Context, branchID string) (int64, error) {
	var id int64
	err := r.db.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(id), 0) FROM "qa-queue_events" WHERE branch_id = $1`, branchID).Scan(&id)
	return id, err
}

// DeleteEventsBefore enforces event retention.
func (r *Repository) DeleteEventsBefore(ctx context.Context, cutoff time.Time) (int64, error) {
	res, err := r.db.ExecContext(ctx, `DELETE FROM "qa-queue_events" WHERE occurred_at < $1`, cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// WaitTimeStats computes the median and 90th percentile of the time tickets spent
// waiting (created_at → called_at) for tickets called within the last `window`.
//
// percentile_cont interpolates between neighbouring samples, so with few samples
// P90 is an estimate rather than an observed value; callers should look at
// SampleSize before trusting it.
func (r *Repository) WaitTimeStats(ctx context.Context, q querier, serviceID string, window time.Duration) (WaitStats, error) {
	st := WaitStats{ServiceID: serviceID, WindowSeconds: int64(window.Seconds())}
	var median, p90 sql.NullFloat64
	err := q.QueryRowContext(ctx, `
		SELECT
			count(*),
			percentile_cont(0.5) WITHIN GROUP (ORDER BY wait_s),
			percentile_cont(0.9) WITHIN GROUP (ORDER BY wait_s),
			(SELECT count(*) FROM "qa-tickets" WHERE service_id = $1 AND status = 'waiting')
		FROM (
			SELECT EXTRACT(EPOCH FROM called_at - created_at)::double precision AS wait_s
			FROM "qa-tickets"
			WHERE service_id = $1
			  AND called_at IS NOT NULL
			  AND called_at >= now() - make_interval(secs => $2)
		) w`, serviceID, window.Seconds()).Scan(&st.SampleSize, &median, &p90, &st.WaitingNow)
	if err != nil {
		return st, err
	}
	if median.Valid {
		st.MedianSeconds = &median.Float64
	}
	if p90.Valid {
		st.P90Seconds = &p90.Float64
	}
	return st, nil
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}
