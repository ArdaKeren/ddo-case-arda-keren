package queue

// Integration tests against a real PostgreSQL. They need TEST_DATABASE_URL to point
// at a disposable database: queue tables are TRUNCATEd before every test.
//
//	TEST_DATABASE_URL=postgres://qa:qa_secret@localhost:5432/qa_test?sslmode=disable go test ./internal/queue -v

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/qa/backend/internal/audit"
	"github.com/qa/backend/internal/auth"
	"github.com/qa/backend/internal/idempotency"
	"github.com/qa/backend/internal/testutil"
)

func testDB(t *testing.T) *sql.DB { return testutil.DB(t) }

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func mustUser(t *testing.T, db *sql.DB, role auth.Role) string {
	t.Helper()
	var id string
	err := db.QueryRow(`INSERT INTO "qa-ev-users" (email, password_hash, full_name, role)
		VALUES ($1, 'x', 'Test User', $2) RETURNING id`, "t-"+randHex(6)+"@example.com", role).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func mustBranch(t *testing.T, repo *Repository) string {
	t.Helper()
	b, err := repo.CreateBranch(context.Background(), "T-"+strings.ToUpper(randHex(3)), "Test Branch")
	if err != nil {
		t.Fatal(err)
	}
	return b.ID
}

func mustService(t *testing.T, repo *Repository, branchID, code string) Service {
	t.Helper()
	s, err := repo.CreateService(context.Background(), branchID, code, "Service "+code)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func mustCounters(t *testing.T, repo *Repository, branchID string, n int) []string {
	t.Helper()
	ids := make([]string, n)
	for i := range ids {
		c, err := repo.CreateCounter(context.Background(), branchID, fmt.Sprintf("Gise %d", i+1))
		if err != nil {
			t.Fatal(err)
		}
		ids[i] = c.ID
	}
	return ids
}

func mustTickets(t *testing.T, repo *Repository, serviceID string, n int) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < n; i++ {
		tx, err := repo.DB().BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := repo.CreateTicket(ctx, tx, serviceID, nil); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
}

type callFn func(ctx context.Context, counterID string) (ticketID string, err error)

type raceResult struct {
	assignments map[string][]string // ticket id -> counters that were told they got it
	calls       int
}

// runCallers releases `workers` goroutines at the same instant; each keeps calling
// until the queue reports empty.
func runCallers(t *testing.T, workers int, counters []string, call callFn) raceResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	var (
		mu    sync.Mutex
		res   = raceResult{assignments: map[string][]string{}}
		wg    sync.WaitGroup
		start = make(chan struct{})
		errs  = make(chan error, workers)
	)
	for w := 0; w < workers; w++ {
		counterID := counters[w%len(counters)]
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for {
				id, err := call(ctx, counterID)
				if errors.Is(err, ErrQueueEmpty) {
					return
				}
				if err != nil {
					errs <- err
					return
				}
				mu.Lock()
				res.assignments[id] = append(res.assignments[id], counterID)
				res.calls++
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("caller error: %v", err)
	}
	return res
}

// TestCallNextNoRaceUnderConcurrency is the core proof: many counters calling at
// once must hand out every ticket exactly once.
func TestCallNextNoRaceUnderConcurrency(t *testing.T) {
	db := testDB(t)
	repo := NewRepository(db, "Europe/Istanbul")
	branch := mustBranch(t, repo)
	staff := mustUser(t, db, auth.RoleStaff)
	svc := mustService(t, repo, branch, "A")
	counters := mustCounters(t, repo, branch, 10)

	const tickets, workers = 500, 50
	mustTickets(t, repo, svc.ID, tickets)

	res := runCallers(t, workers, counters, func(ctx context.Context, counterID string) (string, error) {
		tk, err := repo.CallNext(ctx, svc.ID, counterID, staff)
		return tk.ID, err
	})

	if res.calls != tickets {
		t.Errorf("successful calls = %d, want %d", res.calls, tickets)
	}
	if len(res.assignments) != tickets {
		t.Errorf("distinct tickets called = %d, want %d", len(res.assignments), tickets)
	}
	for id, cs := range res.assignments {
		if len(cs) != 1 {
			t.Errorf("ticket %s handed to %d counters: %v", id, len(cs), cs)
		}
	}

	// The database must agree with what each caller was told.
	rows, err := db.Query(`SELECT id, status, counter_id FROM "qa-tickets" WHERE service_id = $1`, svc.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var id, status string
		var counter sql.NullString
		if err := rows.Scan(&id, &status, &counter); err != nil {
			t.Fatal(err)
		}
		n++
		if status != StatusCalled {
			t.Errorf("ticket %s status = %s, want called", id, status)
		}
		if cs := res.assignments[id]; len(cs) == 1 && cs[0] != counter.String {
			t.Errorf("ticket %s: caller got counter %s but DB says %s", id, cs[0], counter.String)
		}
	}
	if n != tickets {
		t.Errorf("tickets in DB = %d, want %d", n, tickets)
	}
}

// TestNaiveCallNextRaces runs the same harness against an unlocked
// SELECT-then-UPDATE. It shows the harness can detect double assignment; if the
// scheduler happens not to interleave this run, the test skips instead of failing.
func TestNaiveCallNextRaces(t *testing.T) {
	db := testDB(t)
	repo := NewRepository(db, "Europe/Istanbul")
	branch := mustBranch(t, repo)
	svc := mustService(t, repo, branch, "N")
	counters := mustCounters(t, repo, branch, 10)
	mustTickets(t, repo, svc.ID, 300)

	naive := func(ctx context.Context, counterID string) (string, error) {
		var id string
		err := db.QueryRowContext(ctx, `SELECT id FROM "qa-tickets"
			WHERE service_id = $1 AND status = 'waiting'
			ORDER BY service_day, ticket_number LIMIT 1`, svc.ID).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrQueueEmpty
		}
		if err != nil {
			return "", err
		}
		_, err = db.ExecContext(ctx, `UPDATE "qa-tickets"
			SET status = 'called', called_at = now(), counter_id = $2 WHERE id = $1`, id, counterID)
		return id, err
	}
	res := runCallers(t, 50, counters, naive)

	dupes := 0
	for _, cs := range res.assignments {
		if len(cs) > 1 {
			dupes++
		}
	}
	if dupes == 0 {
		t.Skip("naive implementation happened not to race this run")
	}
	t.Logf("control: naive SELECT+UPDATE handed %d tickets to more than one counter (%d calls for %d tickets)",
		dupes, res.calls, len(res.assignments))
}

// TestCallNextSkipsLockedRows shows a caller is not blocked by a ticket another
// transaction is holding: it gets the next ticket immediately.
func TestCallNextSkipsLockedRows(t *testing.T) {
	db := testDB(t)
	repo := NewRepository(db, "Europe/Istanbul")
	branch := mustBranch(t, repo)
	svc := mustService(t, repo, branch, "S")
	counters := mustCounters(t, repo, branch, 1)
	mustTickets(t, repo, svc.ID, 2)

	ctx := context.Background()
	holder, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback()
	var lockedID string
	if err := holder.QueryRowContext(ctx, `SELECT id FROM "qa-tickets" WHERE service_id = $1
		ORDER BY ticket_number LIMIT 1 FOR UPDATE`, svc.ID).Scan(&lockedID); err != nil {
		t.Fatal(err)
	}

	callCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	got, err := repo.CallNext(callCtx, svc.ID, counters[0], "")
	if err != nil {
		t.Fatalf("CallNext while first row locked: %v (it should not wait for the lock)", err)
	}
	if got.ID == lockedID || got.TicketNumber != 2 {
		t.Fatalf("got ticket #%d (%s), want #2, not the locked #1", got.TicketNumber, got.ID)
	}

	// With #1 still locked and #2 called, the queue looks empty to other callers.
	if _, err := repo.CallNext(callCtx, svc.ID, counters[0], ""); !errors.Is(err, ErrQueueEmpty) {
		t.Fatalf("second call: err = %v, want ErrQueueEmpty", err)
	}
}

func TestCreateTicketConcurrentNumbering(t *testing.T) {
	db := testDB(t)
	repo := NewRepository(db, "Europe/Istanbul")
	branch := mustBranch(t, repo)
	svc := mustService(t, repo, branch, "B")

	const n = 100
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		numbers []int
		codes   = map[string]bool{}
		start   = make(chan struct{})
	)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			ctx := context.Background()
			tx, err := db.BeginTx(ctx, nil)
			if err != nil {
				t.Error(err)
				return
			}
			defer tx.Rollback()
			tk, err := repo.CreateTicket(ctx, tx, svc.ID, nil)
			if err != nil {
				t.Error(err)
				return
			}
			if err := tx.Commit(); err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			numbers = append(numbers, tk.TicketNumber)
			codes[tk.DisplayCode] = true
			mu.Unlock()
		}()
	}
	close(start)
	wg.Wait()

	sort.Ints(numbers)
	for i, got := range numbers {
		if got != i+1 {
			t.Fatalf("numbers not 1..%d without gaps or duplicates: %v", n, numbers)
		}
	}
	if !codes["B-001"] || !codes["B-100"] || len(codes) != n {
		t.Errorf("unexpected display codes (%d distinct)", len(codes))
	}
}

func ticketServer(t *testing.T, db *sql.DB) (*httptest.Server, *auth.TokenIssuer) {
	t.Helper()
	tokens := auth.NewTokenIssuer([]byte("0123456789abcdef0123456789abcdef"), "qa-test", time.Hour)
	h := NewHandler(NewRepository(db, "Europe/Istanbul"), tokens, audit.New(db))
	r := chi.NewRouter()
	r.Mount("/api/tickets", h.TicketRoutes())
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv, tokens
}

type httpResult struct {
	status   int
	body     string
	replayed bool
}

func postTicket(t *testing.T, url, token, key string, body any) httpResult {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, url+"/api/tickets", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set(idempotency.Header, key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Error(err)
		return httpResult{}
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	return httpResult{status: resp.StatusCode, body: buf.String(), replayed: resp.Header.Get(idempotency.ReplayedHeader) == "true"}
}

// TestCreateTicketIdempotencyConcurrent fires the same Idempotency-Key many times at
// once: exactly one ticket may be created and every response must be identical.
func TestCreateTicketIdempotencyConcurrent(t *testing.T) {
	db := testDB(t)
	repo := NewRepository(db, "Europe/Istanbul")
	branch := mustBranch(t, repo)
	svc := mustService(t, repo, branch, "C")
	other := mustService(t, repo, branch, "D")
	userID := mustUser(t, db, auth.RoleUser)
	srv, tokens := ticketServer(t, db)
	token, _, _ := tokens.Issue(userID, auth.RoleUser)

	const n = 25
	key := "key-" + randHex(8)
	results := make([]httpResult, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i] = postTicket(t, srv.URL, token, key, map[string]string{"service_id": svc.ID})
		}(i)
	}
	close(start)
	wg.Wait()

	replays := 0
	for i, r := range results {
		if r.status != http.StatusCreated {
			t.Fatalf("request %d: status %d body %s", i, r.status, r.body)
		}
		if r.body != results[0].body {
			t.Errorf("request %d body differs:\n%s\nvs\n%s", i, r.body, results[0].body)
		}
		if r.replayed {
			replays++
		}
	}
	if replays != n-1 {
		t.Errorf("replayed responses = %d, want %d", replays, n-1)
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM "qa-tickets" WHERE service_id = $1`, svc.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("tickets created = %d, want exactly 1", count)
	}

	// Same key, different payload → 422 and still no new ticket.
	if r := postTicket(t, srv.URL, token, key, map[string]string{"service_id": other.ID}); r.status != http.StatusUnprocessableEntity {
		t.Errorf("reused key with different body: status %d, want 422", r.status)
	}
	// Missing key → 400.
	if r := postTicket(t, srv.URL, token, "", map[string]string{"service_id": svc.ID}); r.status != http.StatusBadRequest {
		t.Errorf("missing key: status %d, want 400", r.status)
	}
	// A fresh key creates a second ticket.
	if r := postTicket(t, srv.URL, token, "key-"+randHex(8), map[string]string{"service_id": svc.ID}); r.status != http.StatusCreated || r.replayed {
		t.Errorf("fresh key: status %d replayed %v", r.status, r.replayed)
	}
	// Another user may use the same key string without colliding.
	otherUser := mustUser(t, db, auth.RoleUser)
	otherToken, _, _ := tokens.Issue(otherUser, auth.RoleUser)
	if r := postTicket(t, srv.URL, otherToken, key, map[string]string{"service_id": svc.ID}); r.status != http.StatusCreated || r.replayed {
		t.Errorf("same key, other user: status %d replayed %v", r.status, r.replayed)
	}
	if err := db.QueryRow(`SELECT count(*) FROM "qa-tickets" WHERE service_id = $1`, svc.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 3 {
		t.Errorf("tickets after follow-up requests = %d, want 3", count)
	}
}

func TestWaitTimeStats(t *testing.T) {
	db := testDB(t)
	repo := NewRepository(db, "Europe/Istanbul")
	branch := mustBranch(t, repo)
	svc := mustService(t, repo, branch, "W")
	empty := mustService(t, repo, branch, "E")
	counters := mustCounters(t, repo, branch, 1)
	ctx := context.Background()

	// Waits of 10,20,...,100 seconds, all called within the last hour.
	for i := 1; i <= 10; i++ {
		_, err := db.Exec(`INSERT INTO "qa-tickets"
			(branch_id, service_id, service_day, ticket_number, display_code, status, counter_id, created_at, called_at)
			VALUES ($6, $1, current_date, $2, $3, 'called', $4,
			        now() - interval '30 minutes' - make_interval(secs => $5), now() - interval '30 minutes')`,
			svc.ID, i, fmt.Sprintf("W-%03d", i), counters[0], i*10, branch)
		if err != nil {
			t.Fatal(err)
		}
	}
	// Outside the 2h window: must be ignored even though it is a huge outlier.
	if _, err := db.Exec(`INSERT INTO "qa-tickets"
		(branch_id, service_id, service_day, ticket_number, display_code, status, counter_id, created_at, called_at)
		VALUES ($3, $1, current_date, 11, 'W-11', 'called', $2, now() - interval '5 hours', now() - interval '3 hours')`,
		svc.ID, counters[0], branch); err != nil {
		t.Fatal(err)
	}
	// Two still waiting: counted in waiting_now, not in the sample.
	for i := 12; i <= 13; i++ {
		if _, err := db.Exec(`INSERT INTO "qa-tickets" (branch_id, service_id, service_day, ticket_number, display_code)
			VALUES ($4, $1, current_date, $2, $3)`, svc.ID, i, fmt.Sprintf("W-%03d", i), branch); err != nil {
			t.Fatal(err)
		}
	}

	st, err := repo.WaitTimeStats(ctx, db, svc.ID, 2*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if st.SampleSize != 10 || st.WaitingNow != 2 {
		t.Fatalf("sample=%d waiting=%d, want 10 and 2", st.SampleSize, st.WaitingNow)
	}
	// percentile_cont: median = (50+60)/2 = 55; P90 at rank 0.9*(10-1)=8.1 → 90 + 0.1*(100-90) = 91.
	if st.MedianSeconds == nil || !near(*st.MedianSeconds, 55) {
		t.Errorf("median = %v, want 55", deref(st.MedianSeconds))
	}
	if st.P90Seconds == nil || !near(*st.P90Seconds, 91) {
		t.Errorf("p90 = %v, want 91", deref(st.P90Seconds))
	}

	st, err = repo.WaitTimeStats(ctx, db, empty.ID, 2*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if st.SampleSize != 0 || st.MedianSeconds != nil || st.P90Seconds != nil {
		t.Errorf("empty service: %+v, want zero sample and nil percentiles", st)
	}
}

func near(a, b float64) bool { return a-b < 0.01 && b-a < 0.01 }

func deref(f *float64) any {
	if f == nil {
		return nil
	}
	return *f
}
