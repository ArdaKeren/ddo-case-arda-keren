package realtime

// Integration tests against a real PostgreSQL; see internal/testutil.

import (
	"context"
	"database/sql"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/qa/backend/internal/auth"
	"github.com/qa/backend/internal/queue"
	"github.com/qa/backend/internal/testutil"
)

var testSecret = []byte("0123456789abcdef0123456789abcdef")

type fixture struct {
	db      *sql.DB
	repo    *queue.Repository
	branch  string
	service string
	counter string
	tokens  *auth.TokenIssuer
	hub     *Hub
	url     string
}

func setup(t *testing.T) *fixture {
	t.Helper()
	db := testutil.DB(t)
	repo := queue.NewRepository(db, "Europe/Istanbul")
	ctx := context.Background()

	b, err := repo.CreateBranch(ctx, "RT-01", "Realtime Test")
	if err != nil {
		t.Fatal(err)
	}
	s, err := repo.CreateService(ctx, b.ID, "A", "Genel")
	if err != nil {
		t.Fatal(err)
	}
	c, err := repo.CreateCounter(ctx, b.ID, "Gise 1")
	if err != nil {
		t.Fatal(err)
	}

	hubCtx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	hub := NewHub(repo, testutil.DSN(t))
	go hub.Run(hubCtx)

	tokens := auth.NewTokenIssuer(testSecret, "qa-test", time.Hour)
	srv := httptest.NewServer(NewServer(hub, repo, tokens, nil))
	t.Cleanup(srv.Close)

	return &fixture{db: db, repo: repo, branch: b.ID, service: s.ID, counter: c.ID,
		tokens: tokens, hub: hub, url: "ws" + strings.TrimPrefix(srv.URL, "http")}
}

func (f *fixture) createTickets(t *testing.T, n int) {
	t.Helper()
	if err := f.tryCreateTickets(n); err != nil {
		t.Fatal(err)
	}
}

// tryCreateTickets is safe to call from goroutines (it does not call t.Fatal).
func (f *fixture) tryCreateTickets(n int) error {
	ctx := context.Background()
	for i := 0; i < n; i++ {
		tx, err := f.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := f.repo.CreateTicket(ctx, tx, f.service, nil); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func (f *fixture) eventIDs(t *testing.T, after int64) []int64 {
	t.Helper()
	rows, err := f.db.Query(`SELECT id FROM "qa-queue_events" WHERE branch_id = $1 AND id > $2 ORDER BY id`, f.branch, after)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		_ = rows.Scan(&id)
		ids = append(ids, id)
	}
	return ids
}

func (f *fixture) token(t *testing.T, role auth.Role, ttl time.Duration) string {
	t.Helper()
	tok, _, err := auth.NewTokenIssuer(testSecret, "qa-test", ttl).Issue("00000000-0000-0000-0000-000000000001", role)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

type serverMsg struct {
	Type        string `json:"type"`
	ID          int64  `json:"id"`
	EventType   string `json:"event_type"`
	LastEventID int64  `json:"last_event_id"`
	Replayed    int    `json:"replayed"`
	Board       struct {
		Tickets []queue.TicketView `json:"tickets"`
	} `json:"board"`
}

func (f *fixture) connect(t *testing.T, token string, lastEventID *int64) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, f.url, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.CloseNow() })
	msg := map[string]any{"type": "subscribe", "token": token, "branch_id": f.branch}
	if lastEventID != nil {
		msg["last_event_id"] = *lastEventID
	}
	if err := wsjson.Write(ctx, c, msg); err != nil {
		t.Fatal(err)
	}
	return c
}

func read(t *testing.T, c *websocket.Conn) serverMsg {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var m serverMsg
	if err := wsjson.Read(ctx, c, &m); err != nil {
		t.Fatalf("read: %v", err)
	}
	return m
}

func readEvents(t *testing.T, c *websocket.Conn, n int) []int64 {
	t.Helper()
	ids := make([]int64, 0, n)
	for len(ids) < n {
		m := read(t, c)
		if m.Type != "event" {
			t.Fatalf("expected event, got %q", m.Type)
		}
		ids = append(ids, m.ID)
	}
	return ids
}

func equalIDs(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestHubDeliversEveryEventInOrderUnderConcurrency: with many concurrent writers,
// a subscriber must receive exactly the events in the table, in id order. This
// is the property recovery relies on (no event becomes visible "behind" one the
// client has already seen).
func TestHubDeliversEveryEventInOrderUnderConcurrency(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	sub, err := f.hub.Subscribe(ctx, f.branch)
	if err != nil {
		t.Fatal(err)
	}
	defer f.hub.Unsubscribe(sub)
	start := f.eventIDs(t, 0)
	var floor int64
	if len(start) > 0 {
		floor = start[len(start)-1]
	}

	var wg sync.WaitGroup
	for w := 0; w < 30; w++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			if err := f.tryCreateTickets(10); err != nil {
				t.Error(err)
			}
		}()
		go func() {
			defer wg.Done()
			for i := 0; i < 5; i++ {
				_, _ = f.repo.CallNext(ctx, f.service, f.counter, "")
			}
		}()
	}
	wg.Wait()
	want := f.eventIDs(t, floor)

	var got []int64
	timeout := time.After(15 * time.Second)
	for len(got) < len(want) {
		select {
		case ev := <-sub.C:
			got = append(got, ev.ID)
		case <-sub.Dropped:
			t.Fatal("subscriber dropped")
		case <-timeout:
			t.Fatalf("received %d of %d events", len(got), len(want))
		}
	}
	if !equalIDs(got, want) {
		t.Fatalf("delivered ids differ from table order\n got: %v\nwant: %v", got, want)
	}
	t.Logf("%d events delivered in commit order", len(got))
}

// TestWebSocketRecoversMissedEvents: a screen disconnects, events happen while it
// is offline, and on reconnect with last_event_id it receives exactly those
// events (no gaps, no duplicates) and then continues live.
func TestWebSocketRecoversMissedEvents(t *testing.T) {
	f := setup(t)
	tok := f.token(t, auth.RoleDisplay, time.Hour)
	f.createTickets(t, 2)

	c := f.connect(t, tok, nil)
	snap := read(t, c)
	if snap.Type != "snapshot" || len(snap.Board.Tickets) != 2 {
		t.Fatalf("first message: type=%s tickets=%d, want snapshot with 2", snap.Type, len(snap.Board.Tickets))
	}
	if want := f.eventIDs(t, 0); snap.LastEventID != want[len(want)-1] {
		t.Fatalf("snapshot last_event_id=%d, want %d", snap.LastEventID, want[len(want)-1])
	}

	f.createTickets(t, 3)
	live := readEvents(t, c, 3)
	if !equalIDs(live, f.eventIDs(t, snap.LastEventID)) {
		t.Fatalf("live events %v do not match table", live)
	}
	lastSeen := live[2]
	c.Close(websocket.StatusNormalClosure, "screen going offline")

	// Offline: 4 new tickets and one call.
	f.createTickets(t, 4)
	if _, err := f.repo.CallNext(context.Background(), f.service, f.counter, ""); err != nil {
		t.Fatal(err)
	}
	missed := f.eventIDs(t, lastSeen)
	if len(missed) != 5 {
		t.Fatalf("expected 5 missed events in table, got %d", len(missed))
	}

	c2 := f.connect(t, tok, &lastSeen)
	replayed := readEvents(t, c2, len(missed))
	if !equalIDs(replayed, missed) {
		t.Fatalf("replayed %v, want %v", replayed, missed)
	}
	done := read(t, c2)
	if done.Type != "replay_complete" || done.Replayed != 5 || done.LastEventID != missed[4] {
		t.Fatalf("got %+v, want replay_complete replayed=5 last=%d", done, missed[4])
	}

	// And it is live again afterwards.
	f.createTickets(t, 1)
	next := readEvents(t, c2, 1)
	if next[0] <= done.LastEventID {
		t.Fatalf("live event id %d not after %d", next[0], done.LastEventID)
	}
}

// TestWebSocketFallsBackToSnapshot: if the client's last event has been purged
// by retention, a replay could have gaps, so the server sends a snapshot instead.
func TestWebSocketFallsBackToSnapshot(t *testing.T) {
	f := setup(t)
	tok := f.token(t, auth.RoleDisplay, time.Hour)
	f.createTickets(t, 3)
	old := f.eventIDs(t, 0)[0]

	if _, err := f.repo.DeleteEventsBefore(context.Background(), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	f.createTickets(t, 1)

	c := f.connect(t, tok, &old)
	m := read(t, c)
	if m.Type != "snapshot" || len(m.Board.Tickets) != 4 {
		t.Fatalf("got type=%s tickets=%d, want snapshot with 4 tickets", m.Type, len(m.Board.Tickets))
	}
}

func TestWebSocketAuth(t *testing.T) {
	f := setup(t)
	cases := []struct {
		name  string
		token string
		want  websocket.StatusCode
	}{
		{"garbage token", "nope", CloseUnauthorized},
		{"customer role", f.token(t, auth.RoleUser, time.Hour), CloseForbidden},
	}
	for _, c := range cases {
		conn := f.connect(t, c.token, nil)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		var m serverMsg
		err := wsjson.Read(ctx, conn, &m)
		cancel()
		if got := websocket.CloseStatus(err); got != c.want {
			t.Errorf("%s: close status %d (err %v), want %d", c.name, got, err, c.want)
		}
	}

	// A token that expires mid-connection closes the socket with 4001 so the
	// client knows to refresh and reconnect.
	conn := f.connect(t, f.token(t, auth.RoleDisplay, 1500*time.Millisecond), nil)
	if m := read(t, conn); m.Type != "snapshot" {
		t.Fatalf("got %s, want snapshot", m.Type)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var m serverMsg
	err := wsjson.Read(ctx, conn, &m)
	if got := websocket.CloseStatus(err); got != CloseUnauthorized {
		t.Fatalf("after expiry: close status %d (err %v), want %d", got, err, CloseUnauthorized)
	}
}
