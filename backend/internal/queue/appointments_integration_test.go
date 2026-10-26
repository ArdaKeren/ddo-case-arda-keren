package queue

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/qa/backend/internal/auth"
)

func TestValidSlot(t *testing.T) {
	now := time.Date(2026, 10, 25, 10, 7, 0, 0, time.UTC)
	cases := []struct {
		at   time.Time
		want bool
	}{
		{now.Add(53 * time.Minute), true},                       // 11:00
		{now.Add(54 * time.Minute), false},                      // 11:01, not a quarter hour
		{time.Date(2026, 10, 25, 10, 15, 0, 0, time.UTC), true}, // 8 min ahead
		{time.Date(2026, 10, 25, 10, 0, 0, 0, time.UTC), false}, // past
		{now.Add(91 * 24 * time.Hour).Truncate(AppointmentSlot), false},
	}
	for _, c := range cases {
		if got := ValidSlot(c.at, now); got != c.want {
			t.Errorf("ValidSlot(%s) = %v, want %v", c.at.Format(time.RFC3339), got, c.want)
		}
	}
}

func TestAppointmentConcurrentCheckInIssuesOneTicket(t *testing.T) {
	db := testDB(t)
	repo := NewRepository(db, "Europe/Istanbul")
	branch := mustBranch(t, repo)
	svc := mustService(t, repo, branch, "R")
	user := mustUser(t, db, auth.RoleUser)
	ctx := context.Background()

	slot := time.Now().Add(time.Hour).Truncate(AppointmentSlot)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	a, err := repo.CreateAppointment(ctx, tx, svc.ID, user, slot)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	// Double-booking the same slot is rejected.
	tx, _ = db.BeginTx(ctx, nil)
	if _, err := repo.CreateAppointment(ctx, tx, svc.ID, user, slot); !errors.Is(err, ErrSlotTaken) {
		t.Errorf("double booking: err = %v, want ErrSlotTaken", err)
	}
	tx.Rollback()

	// Too early to check in.
	if _, _, err := repo.CheckIn(ctx, a.ID, slot.Add(-time.Hour)); !errors.Is(err, ErrCheckInWindow) {
		t.Errorf("early check-in: err = %v, want ErrCheckInWindow", err)
	}

	const n = 20
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		success int
		start   = make(chan struct{})
	)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, _, err := repo.CheckIn(ctx, a.ID, slot)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				success++
			case !errors.Is(err, ErrNotCheckInnable):
				t.Errorf("check-in: %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()

	if success != 1 {
		t.Fatalf("successful check-ins = %d, want 1", success)
	}
	var tickets int
	if err := db.QueryRow(`SELECT count(*) FROM "qa-tickets" WHERE service_id = $1`, svc.ID).Scan(&tickets); err != nil {
		t.Fatal(err)
	}
	if tickets != 1 {
		t.Fatalf("tickets issued = %d, want 1", tickets)
	}

	var types []string
	rows, err := db.Query(`SELECT event_type FROM "qa-queue_events" WHERE branch_id = $1 ORDER BY id`, branch)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		types = append(types, s)
	}
	want := []string{EventAppointmentCreated, EventTicketCreated, EventAppointmentCheckedIn}
	if len(types) != len(want) {
		t.Fatalf("events = %v, want %v", types, want)
	}
	for i := range want {
		if types[i] != want[i] {
			t.Fatalf("events = %v, want %v", types, want)
		}
	}
}
