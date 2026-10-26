package queue

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

const (
	AppointmentBooked    = "booked"
	AppointmentCheckedIn = "checked_in"

	// Slots start on quarter hours and can be booked up to 90 days ahead.
	AppointmentSlot     = 15 * time.Minute
	AppointmentMaxAhead = 90 * 24 * time.Hour
	AppointmentMinAhead = 5 * time.Minute
	CheckInOpensBefore  = 30 * time.Minute
	CheckInClosesAfter  = 15 * time.Minute
)

var (
	ErrSlotTaken       = errors.New("you already have a booking for this service at that time")
	ErrCheckInWindow   = errors.New("check-in is only possible from 30 minutes before until 15 minutes after the appointment")
	ErrNotCheckInnable = errors.New("appointment is not in booked state")
)

type Appointment struct {
	ID             string     `json:"id"`
	BranchID       string     `json:"branch_id"`
	ServiceID      string     `json:"service_id"`
	CustomerUserID *string    `json:"customer_user_id"`
	ScheduledAt    time.Time  `json:"scheduled_at"`
	Status         string     `json:"status"`
	TicketID       *string    `json:"ticket_id"`
	CheckedInAt    *time.Time `json:"checked_in_at"`
	CreatedAt      time.Time  `json:"created_at"`
}

const appointmentColumns = `id, branch_id, service_id, customer_user_id, scheduled_at, status,
	ticket_id, checked_in_at, created_at`

func scanAppointment(row interface{ Scan(...any) error }) (Appointment, error) {
	var a Appointment
	err := row.Scan(&a.ID, &a.BranchID, &a.ServiceID, &a.CustomerUserID, &a.ScheduledAt, &a.Status,
		&a.TicketID, &a.CheckedInAt, &a.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return a, ErrNotFound
	}
	return a, err
}

// ValidSlot reports whether t is a bookable slot start relative to now.
func ValidSlot(t, now time.Time) bool {
	return t.Truncate(AppointmentSlot).Equal(t) &&
		t.After(now.Add(AppointmentMinAhead)) &&
		t.Before(now.Add(AppointmentMaxAhead))
}

// CreateAppointment books a slot within tx and emits appointment.created.
func (r *Repository) CreateAppointment(ctx context.Context, tx *sql.Tx, serviceID, customerUserID string, at time.Time) (Appointment, error) {
	var branchID string
	err := tx.QueryRowContext(ctx, `
		SELECT s.branch_id FROM "qa-services" s JOIN "qa-branches" b ON b.id = s.branch_id
		WHERE s.id = $1 AND s.is_active AND b.is_active`, serviceID).Scan(&branchID)
	if errors.Is(err, sql.ErrNoRows) {
		return Appointment{}, ErrServiceNotFound
	}
	if err != nil {
		return Appointment{}, err
	}

	a, err := scanAppointment(tx.QueryRowContext(ctx, `
		INSERT INTO "qa-appointments" (branch_id, service_id, customer_user_id, scheduled_at)
		VALUES ($1, $2, $3, $4)
		RETURNING `+appointmentColumns, branchID, serviceID, customerUserID, at))
	if isUniqueViolation(err) {
		return a, ErrSlotTaken
	}
	if err != nil {
		return a, err
	}
	return a, emitAppointmentEvent(ctx, tx, a, EventAppointmentCreated)
}

func (r *Repository) AppointmentByID(ctx context.Context, id string) (Appointment, error) {
	return scanAppointment(r.db.QueryRowContext(ctx,
		`SELECT `+appointmentColumns+` FROM "qa-appointments" WHERE id = $1`, id))
}

// CheckIn turns a booked appointment into a waiting ticket in one transaction.
func (r *Repository) CheckIn(ctx context.Context, appointmentID string, now time.Time) (Appointment, Ticket, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return Appointment{}, Ticket{}, err
	}
	defer tx.Rollback()

	// FOR UPDATE so two concurrent check-ins cannot both issue a ticket.
	a, err := scanAppointment(tx.QueryRowContext(ctx,
		`SELECT `+appointmentColumns+` FROM "qa-appointments" WHERE id = $1 FOR UPDATE`, appointmentID))
	if err != nil {
		return a, Ticket{}, err
	}
	if a.Status != AppointmentBooked {
		return a, Ticket{}, ErrNotCheckInnable
	}
	if now.Before(a.ScheduledAt.Add(-CheckInOpensBefore)) || now.After(a.ScheduledAt.Add(CheckInClosesAfter)) {
		return a, Ticket{}, ErrCheckInWindow
	}

	// CreateTicket emits ticket.created; the appointment event goes last.
	t, err := r.CreateTicket(ctx, tx, a.ServiceID, a.CustomerUserID)
	if err != nil {
		return a, t, err
	}
	a, err = scanAppointment(tx.QueryRowContext(ctx, `
		UPDATE "qa-appointments" SET status = 'checked_in', ticket_id = $2, checked_in_at = now()
		WHERE id = $1 RETURNING `+appointmentColumns, a.ID, t.ID))
	if err != nil {
		return a, t, err
	}
	if err := emitAppointmentEvent(ctx, tx, a, EventAppointmentCheckedIn); err != nil {
		return a, t, err
	}
	return a, t, tx.Commit()
}

// Appointment events omit the customer so they are safe to send to lobby displays.
func emitAppointmentEvent(ctx context.Context, tx *sql.Tx, a Appointment, eventType string) error {
	_, err := tx.ExecContext(ctx, `
		SELECT qa_emit_event($1, $2, jsonb_build_object('appointment', jsonb_build_object(
			'id', a.id, 'service_id', a.service_id, 'service_code', s.code,
			'scheduled_at', a.scheduled_at, 'status', a.status, 'ticket_id', a.ticket_id)))
		FROM "qa-appointments" a JOIN "qa-services" s ON s.id = a.service_id
		WHERE a.id = $3`, a.BranchID, eventType, a.ID)
	return err
}
