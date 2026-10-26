package server

// End-to-end integration tests: the real router, middleware chain and background
// workers against PostgreSQL in a Testcontainers container (see internal/testutil).

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/pressly/goose/v3"

	"github.com/qa/backend/internal/config"
	"github.com/qa/backend/internal/testutil"
	"github.com/qa/backend/migrations"
)

func TestMain(m *testing.M) { testutil.Main(m) }

const adminPassword = "SeedDalga-IT-2026!"

type env struct {
	t     *testing.T
	db    *sql.DB
	base  string
	admin string // admin access token
}

func setup(t *testing.T) *env {
	t.Helper()
	db := testutil.DB(t)
	cfg := config.Config{
		DatabaseURL:    testutil.DSN(t),
		JWTSecret:      []byte("integration-secret-integration-secret"),
		JWTIssuer:      "qa-it",
		AccessTTL:      15 * time.Minute,
		RefreshTTL:     24 * time.Hour,
		QueueTimezone:  "Europe/Istanbul",
		EventRetention: 48 * time.Hour,
		AdminEmail:     config.DefaultSeedAdminEmail,
		AdminPassword:  adminPassword,
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	app, err := New(ctx, cfg, db)
	if err != nil {
		t.Fatal(err)
	}
	go app.Run(ctx)
	srv := httptest.NewServer(app.Handler)
	t.Cleanup(srv.Close)

	e := &env{t: t, db: db, base: srv.URL}
	e.admin = e.login(config.DefaultSeedAdminEmail, adminPassword).AccessToken
	return e
}

type resp struct {
	Status int
	Header http.Header
	Body   []byte
}

func (r resp) json(t *testing.T, v any) {
	t.Helper()
	if err := json.Unmarshal(r.Body, v); err != nil {
		t.Fatalf("decode %s: %v", r.Body, err)
	}
}

func (e *env) do(method, path, token string, body any, headers map[string]string) resp {
	e.t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		rd = strings.NewReader(b)
	default:
		raw, _ := json.Marshal(b)
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, e.base+path, rd)
	if err != nil {
		e.t.Fatal(err)
	}
	if rd != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	return resp{Status: res.StatusCode, Header: res.Header, Body: raw}
}

func (e *env) expect(r resp, status int) resp {
	e.t.Helper()
	if r.Status != status {
		e.t.Fatalf("status %d, want %d: %s", r.Status, status, r.Body)
	}
	return r
}

type tokens struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	User         struct {
		ID   string `json:"id"`
		Role string `json:"role"`
	} `json:"user"`
}

func (e *env) login(email, password string) tokens {
	e.t.Helper()
	var tk tokens
	e.expect(e.do("POST", "/api/auth/login", "", map[string]string{"email": email, "password": password}, nil), 200).json(e.t, &tk)
	return tk
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// user registers a fresh account, assigns role via the admin API and logs in.
func (e *env) user(role string) tokens {
	e.t.Helper()
	email := fmt.Sprintf("it.%s.%s@ddo-case.invalid", role, randHex(5))
	const pw = "Integration-Pass-1"
	var u struct {
		ID string `json:"id"`
	}
	e.expect(e.do("POST", "/api/auth/register", "", map[string]string{"email": email, "password": pw, "full_name": "IT " + role}, nil), 201).json(e.t, &u)
	if role != "user" {
		e.expect(e.do("PATCH", "/api/admin/users/"+u.ID+"/role", e.admin, map[string]string{"role": role}, nil), 200)
	}
	return e.login(email, pw)
}

type branchFixture struct{ Branch, Service, Counter string }

func (e *env) branch() branchFixture {
	e.t.Helper()
	var b, s, c struct {
		ID string `json:"id"`
	}
	e.expect(e.do("POST", "/api/queue/branches", e.admin, map[string]string{"code": "IT-" + strings.ToUpper(randHex(3)), "name": "IT Şube"}, nil), 201).json(e.t, &b)
	e.expect(e.do("POST", "/api/queue/services", e.admin, map[string]string{"branch_id": b.ID, "code": "A", "name": "Genel"}, nil), 201).json(e.t, &s)
	e.expect(e.do("POST", "/api/queue/counters", e.admin, map[string]string{"branch_id": b.ID, "name": "Gişe 1"}, nil), 201).json(e.t, &c)
	return branchFixture{b.ID, s.ID, c.ID}
}

func (e *env) ticket(token, serviceID string) string {
	e.t.Helper()
	var r struct {
		Ticket struct {
			ID string `json:"id"`
		} `json:"ticket"`
	}
	e.expect(e.do("POST", "/api/tickets", token, map[string]string{"service_id": serviceID},
		map[string]string{"Idempotency-Key": randHex(16)}), 201).json(e.t, &r)
	return r.Ticket.ID
}

// 1
func TestRegisterLoginMeUsesArgon2id(t *testing.T) {
	e := setup(t)
	u := e.user("user")
	if u.AccessToken == "" || u.RefreshToken == "" || u.User.Role != "user" {
		t.Fatalf("unexpected login response %+v", u)
	}
	var me struct {
		User        struct{ Role string } `json:"user"`
		Permissions []string              `json:"permissions"`
	}
	e.expect(e.do("GET", "/api/auth/me", u.AccessToken, nil, nil), 200).json(t, &me)
	if me.User.Role != "user" || len(me.Permissions) == 0 {
		t.Fatalf("me = %+v", me)
	}
	var hash string
	if err := e.db.QueryRow(`SELECT password_hash FROM "qa-ev-users" WHERE id = $1`, u.User.ID).Scan(&hash); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(hash, "$argon2id$v=19$") {
		t.Fatalf("password stored as %q, want an argon2id PHC string", hash[:20])
	}
	e.expect(e.do("POST", "/api/auth/login", "", map[string]string{"email": "nobody@ddo-case.invalid", "password": "whatever-123"}, nil), 401)
}

// 2
func TestRefreshRotationAndReuseDetection(t *testing.T) {
	e := setup(t)
	u := e.user("user")
	var r2 tokens
	e.expect(e.do("POST", "/api/auth/refresh", "", map[string]string{"refresh_token": u.RefreshToken}, nil), 200).json(t, &r2)
	if r2.RefreshToken == u.RefreshToken {
		t.Fatal("refresh token was not rotated")
	}
	// Replaying the old token is treated as theft: rejected, and the new one dies too.
	e.expect(e.do("POST", "/api/auth/refresh", "", map[string]string{"refresh_token": u.RefreshToken}, nil), 401)
	e.expect(e.do("POST", "/api/auth/refresh", "", map[string]string{"refresh_token": r2.RefreshToken}, nil), 401)

	var n int
	_ = e.db.QueryRow(`SELECT count(*) FROM "qa-audit_logs" WHERE action = 'auth.refresh_reuse_detected' AND actor_user_id = $1`, u.User.ID).Scan(&n)
	// Both attempts present a revoked token, so both are recorded as reuse.
	if n != 2 {
		t.Fatalf("reuse audit entries = %d, want 2", n)
	}
}

// 3
func TestRBACAcrossRoles(t *testing.T) {
	e := setup(t)
	b := e.branch()
	user, display, staff := e.user("user"), e.user("display"), e.user("staff")

	cases := []struct {
		name, method, path, token string
		body                      any
		want                      int
	}{
		{"anonymous → admin", "GET", "/api/admin/users", "", nil, 401},
		{"user → admin", "GET", "/api/admin/users", user.AccessToken, nil, 403},
		{"admin → admin", "GET", "/api/admin/users", e.admin, nil, 200},
		{"user → board", "GET", "/api/queue/branches/" + b.Branch + "/board", user.AccessToken, nil, 403},
		{"display → board", "GET", "/api/queue/branches/" + b.Branch + "/board", display.AccessToken, nil, 200},
		{"display → call-next", "POST", "/api/queue/call-next", display.AccessToken, map[string]string{"service_id": b.Service, "counter_id": b.Counter}, 403},
		{"staff → call-next (empty)", "POST", "/api/queue/call-next", staff.AccessToken, map[string]string{"service_id": b.Service, "counter_id": b.Counter}, 204},
		{"staff → create branch", "POST", "/api/queue/branches", staff.AccessToken, map[string]string{"code": "XX-1", "name": "x"}, 403},
		{"user → audit", "GET", "/api/admin/audit-logs", user.AccessToken, nil, 403},
	}
	for _, c := range cases {
		if r := e.do(c.method, c.path, c.token, c.body, nil); r.Status != c.want {
			t.Errorf("%s: status %d, want %d (%s)", c.name, r.Status, c.want, r.Body)
		}
	}
}

// 4
func TestDDOMarkHeaderOnEveryResponse(t *testing.T) {
	e := setup(t)
	check := func(name string, h http.Header) {
		if got := h.Get("X-DDO-Mark"); got != "bfe1f2" {
			t.Errorf("%s: X-DDO-Mark = %q, want bfe1f2", name, got)
		}
	}
	check("200 /health", e.do("GET", "/health", "", nil, nil).Header)
	check("404", e.do("GET", "/does-not-exist", "", nil, nil).Header)
	check("401", e.do("GET", "/api/auth/me", "", nil, nil).Header)
	check("400", e.do("POST", "/api/auth/login", "", "{not json", nil).Header)
	check("405", e.do("DELETE", "/health", "", nil, nil).Header)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, res, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(e.base, "http")+"/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	check("101 websocket upgrade", res.Header)
}

// 5
func TestHealthAndHiddenDiagnostics(t *testing.T) {
	e := setup(t)
	var h map[string]any
	e.expect(e.do("GET", "/health", "", nil, nil), 200).json(t, &h)
	if h["status"] != "ok" {
		t.Fatalf("/health = %v", h)
	}
	var d struct {
		Status string `json:"status"`
		DB     struct {
			Reachable     bool  `json:"reachable"`
			SchemaVersion int64 `json:"schema_version"`
		} `json:"db"`
	}
	e.expect(e.do("GET", "/_hz/bfe1", "", nil, nil), 200).json(t, &d)
	if d.Status != "ok" || !d.DB.Reachable || d.DB.SchemaVersion != 7 {
		t.Fatalf("/_hz/bfe1 = %+v, want ok, reachable, schema 7", d)
	}
	if strings.Contains(string(e.do("GET", "/_hz/bfe1", "", nil, nil).Body), "secret") {
		t.Fatal("diagnostics endpoint leaks something named secret")
	}
}

// 6
func TestAuditLogIsAppendOnly(t *testing.T) {
	e := setup(t)
	u := e.user("user")
	var id int64
	if err := e.db.QueryRow(`SELECT id FROM "qa-audit_logs" WHERE action = 'auth.login' AND actor_user_id = $1`, u.User.ID).Scan(&id); err != nil {
		t.Fatalf("login was not audited: %v", err)
	}
	for _, stmt := range []string{
		`UPDATE "qa-audit_logs" SET action = 'tampered' WHERE id = $1`,
		`DELETE FROM "qa-audit_logs" WHERE id = $1`,
	} {
		if _, err := e.db.Exec(stmt, id); err == nil || !strings.Contains(err.Error(), "append-only") {
			t.Errorf("%q: err = %v, want append-only rejection", stmt, err)
		}
	}
	if _, err := e.db.Exec(`TRUNCATE "qa-audit_logs"`); err == nil || !strings.Contains(err.Error(), "append-only") {
		t.Errorf("TRUNCATE: err = %v, want append-only rejection", err)
	}
	var logs struct {
		AuditLogs []struct{ ID int64 } `json:"audit_logs"`
	}
	e.expect(e.do("GET", "/api/admin/audit-logs?limit=100", e.admin, nil, nil), 200).json(t, &logs)
	if len(logs.AuditLogs) == 0 {
		t.Fatal("admin audit-log listing is empty")
	}
}

// 7
func TestTicketIdempotencyOverHTTP(t *testing.T) {
	e := setup(t)
	b := e.branch()
	u := e.user("user")
	key := map[string]string{"Idempotency-Key": "it-" + randHex(8)}
	body := map[string]string{"service_id": b.Service}

	first := e.expect(e.do("POST", "/api/tickets", u.AccessToken, body, key), 201)
	again := e.expect(e.do("POST", "/api/tickets", u.AccessToken, body, key), 201)
	if !bytes.Equal(first.Body, again.Body) || again.Header.Get("Idempotent-Replayed") != "true" {
		t.Fatalf("replay differs or is not marked:\n%s\n%s", first.Body, again.Body)
	}
	other := e.branch()
	e.expect(e.do("POST", "/api/tickets", u.AccessToken, map[string]string{"service_id": other.Service}, key), 422)
	e.expect(e.do("POST", "/api/tickets", u.AccessToken, body, nil), 400)

	var n int
	_ = e.db.QueryRow(`SELECT count(*) FROM "qa-tickets" WHERE service_id = $1`, b.Service).Scan(&n)
	if n != 1 {
		t.Fatalf("tickets = %d, want 1", n)
	}
}

// 8
func TestCallNextAndFinishLifecycle(t *testing.T) {
	e := setup(t)
	b := e.branch()
	u := e.user("user")
	first := e.ticket(u.AccessToken, b.Service)
	e.ticket(u.AccessToken, b.Service)

	call := map[string]string{"service_id": b.Service, "counter_id": b.Counter}
	type called struct {
		ID          string `json:"id"`
		DisplayCode string `json:"display_code"`
	}
	var c1, c2 called
	e.expect(e.do("POST", "/api/queue/call-next", e.admin, call, nil), 200).json(t, &c1)
	e.expect(e.do("POST", "/api/queue/call-next", e.admin, call, nil), 200).json(t, &c2)
	if c1.ID != first || !strings.HasSuffix(c1.DisplayCode, "-001") || !strings.HasSuffix(c2.DisplayCode, "-002") {
		t.Fatalf("FIFO violated: first call %s (%s), second %s", c1.DisplayCode, c1.ID, c2.DisplayCode)
	}
	e.expect(e.do("POST", "/api/queue/call-next", e.admin, call, nil), 204)

	e.expect(e.do("POST", "/api/queue/tickets/"+first+"/finish", e.admin, map[string]string{"outcome": "completed"}, nil), 200)
	e.expect(e.do("POST", "/api/queue/tickets/"+first+"/finish", e.admin, map[string]string{"outcome": "completed"}, nil), 409)
	e.expect(e.do("POST", "/api/queue/tickets/"+first+"/finish", e.admin, map[string]string{"outcome": "lost"}, nil), 400)

	other := e.branch()
	e.expect(e.do("POST", "/api/queue/call-next", e.admin, map[string]string{"service_id": b.Service, "counter_id": other.Counter}, nil), 404)
}

// 9
func TestConcurrentCallNextOverHTTPNeverDoubleAssigns(t *testing.T) {
	e := setup(t)
	b := e.branch()
	u := e.user("user")
	const tickets, callers = 40, 80
	for i := 0; i < tickets; i++ {
		e.ticket(u.AccessToken, b.Service)
	}

	call, _ := json.Marshal(map[string]string{"service_id": b.Service, "counter_id": b.Counter})
	var (
		wg     sync.WaitGroup
		mu     sync.Mutex
		ids    = map[string]int{}
		empty  int
		start  = make(chan struct{})
		client = &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: callers}}
	)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			req, _ := http.NewRequest("POST", e.base+"/api/queue/call-next", bytes.NewReader(call))
			req.Header.Set("Authorization", "Bearer "+e.admin)
			req.Header.Set("Content-Type", "application/json")
			res, err := client.Do(req)
			if err != nil {
				t.Error(err)
				return
			}
			defer res.Body.Close()
			var tk struct {
				ID string `json:"id"`
			}
			_ = json.NewDecoder(res.Body).Decode(&tk)
			mu.Lock()
			defer mu.Unlock()
			switch res.StatusCode {
			case 200:
				ids[tk.ID]++
			case 204:
				empty++
			default:
				t.Errorf("unexpected status %d", res.StatusCode)
			}
		}()
	}
	close(start)
	wg.Wait()

	if len(ids) != tickets || empty != callers-tickets {
		t.Fatalf("distinct called = %d (want %d), empty = %d (want %d)", len(ids), tickets, empty, callers-tickets)
	}
	for id, n := range ids {
		if n != 1 {
			t.Errorf("ticket %s called %d times", id, n)
		}
	}
}

// 10
func TestWaitTimeStatsOverHTTP(t *testing.T) {
	e := setup(t)
	b := e.branch()
	u := e.user("user")
	for i := 0; i < 5; i++ {
		e.ticket(u.AccessToken, b.Service)
	}
	for i := 0; i < 5; i++ {
		e.expect(e.do("POST", "/api/queue/call-next", e.admin, map[string]string{"service_id": b.Service, "counter_id": b.Counter}, nil), 200)
	}
	var st struct {
		SampleSize    int      `json:"sample_size"`
		MedianSeconds *float64 `json:"median_seconds"`
		P90Seconds    *float64 `json:"p90_seconds"`
		WaitingNow    int      `json:"waiting_now"`
	}
	e.expect(e.do("GET", "/api/queue/services/"+b.Service+"/wait-stats?window=1h", e.admin, nil, nil), 200).json(t, &st)
	if st.SampleSize != 5 || st.MedianSeconds == nil || st.P90Seconds == nil || *st.P90Seconds < *st.MedianSeconds || st.WaitingNow != 0 {
		t.Fatalf("stats = %+v", st)
	}
	e.expect(e.do("GET", "/api/queue/services/"+b.Service+"/wait-stats?window=forever", e.admin, nil, nil), 400)
}

// 11
func TestAppointmentBookingAndCheckIn(t *testing.T) {
	e := setup(t)
	b := e.branch()
	u, stranger := e.user("user"), e.user("user")
	// The next quarter-hour slot at least 5 minutes away, so check-in is already open.
	slot := time.Now().Add(6 * time.Minute).Truncate(15 * time.Minute).Add(15 * time.Minute)
	book := func(at time.Time) resp {
		return e.do("POST", "/api/appointments", u.AccessToken,
			map[string]any{"service_id": b.Service, "scheduled_at": at.UTC().Format(time.RFC3339)},
			map[string]string{"Idempotency-Key": randHex(16)})
	}
	var a struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	e.expect(book(slot), 201).json(t, &a)
	e.expect(book(slot), 409)                    // double booking
	e.expect(book(slot.Add(7*time.Minute)), 400) // not a quarter hour

	e.expect(e.do("GET", "/api/appointments/"+a.ID, stranger.AccessToken, nil, nil), 404)
	var ci struct {
		Appointment struct{ Status string }     `json:"appointment"`
		Ticket      struct{ ID, Status string } `json:"ticket"`
	}
	e.expect(e.do("POST", "/api/appointments/"+a.ID+"/check-in", u.AccessToken, nil, nil), 200).json(t, &ci)
	if ci.Appointment.Status != "checked_in" || ci.Ticket.Status != "waiting" {
		t.Fatalf("check-in = %+v", ci)
	}
	e.expect(e.do("POST", "/api/appointments/"+a.ID+"/check-in", u.AccessToken, nil, nil), 409)
}

// 12
func TestWebSocketRecoveryThroughFullRouter(t *testing.T) {
	e := setup(t)
	b := e.branch()
	u, display := e.user("user"), e.user("display")
	wsURL := "ws" + strings.TrimPrefix(e.base, "http") + "/ws"

	type msg struct {
		Type        string `json:"type"`
		ID          int64  `json:"id"`
		LastEventID int64  `json:"last_event_id"`
		Replayed    int    `json:"replayed"`
	}
	connect := func(last *int64) *websocket.Conn {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		c, _, err := websocket.Dial(ctx, wsURL, nil)
		if err != nil {
			t.Fatal(err)
		}
		sub := map[string]any{"type": "subscribe", "token": display.AccessToken, "branch_id": b.Branch}
		if last != nil {
			sub["last_event_id"] = *last
		}
		if err := wsjson.Write(ctx, c, sub); err != nil {
			t.Fatal(err)
		}
		return c
	}
	read := func(c *websocket.Conn) msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		var m msg
		if err := wsjson.Read(ctx, c, &m); err != nil {
			t.Fatalf("read: %v", err)
		}
		return m
	}

	c := connect(nil)
	if m := read(c); m.Type != "snapshot" {
		t.Fatalf("first message %q, want snapshot", m.Type)
	}
	e.ticket(u.AccessToken, b.Service)
	e.ticket(u.AccessToken, b.Service)
	read(c)
	last := read(c).ID
	c.Close(websocket.StatusNormalClosure, "offline")

	for i := 0; i < 3; i++ {
		e.ticket(u.AccessToken, b.Service)
	}
	c2 := connect(&last)
	defer c2.CloseNow()
	prev := last
	for i := 0; i < 3; i++ {
		m := read(c2)
		if m.Type != "event" || m.ID <= prev {
			t.Fatalf("replay %d: %+v (prev %d)", i, m, prev)
		}
		prev = m.ID
	}
	if m := read(c2); m.Type != "replay_complete" || m.Replayed != 3 || m.LastEventID != prev {
		t.Fatalf("got %+v, want replay_complete of 3 ending at %d", m, prev)
	}
}

// 13
func TestMigrationsRoundTripDownAndUp(t *testing.T) {
	db, err := sql.Open("pgx", testutil.NewDatabase(t))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	goose.SetLogger(goose.NopLogger())

	countQA := func() int {
		var n int
		_ = db.QueryRow(`SELECT count(*) FROM information_schema.tables
			WHERE table_schema = 'public' AND table_name LIKE 'qa-%' AND table_name <> 'qa-goose_db_version'`).Scan(&n)
		return n
	}
	if err := migrations.Up(db); err != nil {
		t.Fatalf("up: %v", err)
	}
	up := countQA()
	if err := goose.DownTo(db, ".", 0); err != nil {
		t.Fatalf("down: %v", err)
	}
	if n := countQA(); n != 0 {
		t.Fatalf("%d qa- tables left after migrating down to 0", n)
	}
	if err := migrations.Up(db); err != nil {
		t.Fatalf("up again: %v", err)
	}
	if n := countQA(); n != up {
		t.Fatalf("tables after re-up = %d, want %d", n, up)
	}
}

// 14
func TestSchemaNamingConvention(t *testing.T) {
	e := setup(t)
	rows, err := e.db.Query(`SELECT table_name FROM information_schema.tables
		WHERE table_schema = 'public' AND table_type = 'BASE TABLE' ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	seen := map[string]bool{}
	for rows.Next() {
		var name string
		_ = rows.Scan(&name)
		seen[name] = true
		if !strings.HasPrefix(name, "qa-") {
			t.Errorf("table %q lacks the qa- prefix", name)
		}
	}
	for _, required := range []string{"qa-ev-users", "qa-audit_logs"} {
		if !seen[required] {
			t.Errorf("required table %q missing", required)
		}
	}
}
