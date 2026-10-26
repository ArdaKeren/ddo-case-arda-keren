package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// checks is the requirement → test mapping printed by `make verify`.
// Edit IDs and titles here if the case document numbers them differently.
var checks = []struct {
	ID, Title string
	Run       func(*verifier, *assert) error
}{
	{"F-1", "Kimlik doğrulama (Argon2id + JWT, refresh rotasyonu)", (*verifier).checkAuth},
	{"F-2", "RBAC: rol bazlı yetkilendirme", (*verifier).checkRBAC},
	{"F-3", "Bilet oluşturma + Idempotency-Key", (*verifier).checkTickets},
	{"F-4", "Sıra çağırma motoru (FIFO, call-next, finish)", (*verifier).checkCallNext},
	{"F-5", "Bekleme süresi tahmini (Median / P90)", (*verifier).checkWaitStats},
	{"F-6", "Randevu (rezervasyon, çakışma, check-in)", (*verifier).checkAppointments},
	{"T-1", "X-DDO-Mark: bfe1f2 başlığı tüm yanıtlarda", (*verifier).checkMark},
	{"T-2", "/health ve gizli /_hz/bfe1 uçları", (*verifier).checkHealth},
	{"T-3", "WebSocket canlı akış + kopma sonrası recovery", (*verifier).checkWebSocket},
	{"T-4", "Race condition yok (SKIP LOCKED, eşzamanlı idempotency)", (*verifier).checkRace},
}

type verifier struct {
	api   *API
	admin *Session
	run   string // unique suffix so repeated runs never collide

	branch, service, raceService string
	counters                     []string
	customer, display, staff     *Session
	wsURL                        string
}

// assert counts assertions so the report can say how much each check covered.
type assert struct{ n int }

func (a *assert) status(what string, res Response, err error, want ...int) error {
	a.n++
	if err != nil {
		return fmt.Errorf("%s: %v", what, err)
	}
	for _, w := range want {
		if res.Status == w {
			return nil
		}
	}
	return fmt.Errorf("%s: HTTP %d, want %v: %s", what, res.Status, want, truncate(res.Body))
}

func (a *assert) that(ok bool, format string, args ...any) error {
	a.n++
	if !ok {
		return fmt.Errorf(format, args...)
	}
	return nil
}

func truncate(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 120 {
		return s[:120] + "…"
	}
	return s
}

type checkResult struct {
	ID, Title string
	Pass      bool
	Detail    string
	Took      time.Duration
}

// Verify runs every check against a live deployment and prints a PASS/FAIL table.
func Verify(api *API, admin *Session, apiURL string) error {
	v := &verifier{api: api, admin: admin, run: strings.ToUpper(newUUID()[:6])}
	v.wsURL = "ws" + strings.TrimPrefix(apiURL, "http") + "/ws"

	fmt.Printf("Verifying %s …\n\n", apiURL)
	setupErr := v.setup()
	if setupErr != nil {
		fmt.Printf("setup failed: %v\n\n", setupErr)
	}

	var results []checkResult
	for _, c := range checks {
		start := time.Now()
		a := &assert{}
		var err error
		if setupErr != nil {
			err = errors.New("skipped: setup failed")
		} else {
			err = c.Run(v, a)
		}
		r := checkResult{ID: c.ID, Title: c.Title, Pass: err == nil, Took: time.Since(start)}
		if err != nil {
			r.Detail = err.Error()
		} else {
			r.Detail = fmt.Sprintf("%d assertions", a.n)
		}
		results = append(results, r)
	}

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tGereksinim\tSonuç\tSüre\tAyrıntı")
	fmt.Fprintln(tw, "--\t----------\t-----\t----\t-------")
	passed := 0
	for _, r := range results {
		status := "FAIL"
		if r.Pass {
			status = "PASS"
			passed++
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", r.ID, r.Title, status, r.Took.Round(time.Millisecond), r.Detail)
	}
	tw.Flush()
	fmt.Printf("\n%d/%d PASS\n", passed, len(results))
	if passed != len(results) {
		return fmt.Errorf("%d check(s) failed", len(results)-passed)
	}
	return nil
}

func (v *verifier) setup() error {
	if err := requireAdmin(v.admin); err != nil {
		return err
	}
	var b, s, rs struct {
		ID string `json:"id"`
	}
	if err := v.adminCreate("/api/queue/branches", map[string]string{"code": "VF-" + v.run, "name": "Verify " + v.run}, &b); err != nil {
		return err
	}
	v.branch = b.ID
	if err := v.adminCreate("/api/queue/services", map[string]string{"branch_id": b.ID, "code": "A", "name": "Verify"}, &s); err != nil {
		return err
	}
	if err := v.adminCreate("/api/queue/services", map[string]string{"branch_id": b.ID, "code": "R", "name": "Race"}, &rs); err != nil {
		return err
	}
	v.service, v.raceService = s.ID, rs.ID
	for i := 1; i <= 2; i++ {
		var c struct {
			ID string `json:"id"`
		}
		if err := v.adminCreate("/api/queue/counters", map[string]string{"branch_id": b.ID, "name": fmt.Sprintf("Gişe %d", i)}, &c); err != nil {
			return err
		}
		v.counters = append(v.counters, c.ID)
	}
	var err error
	if v.customer, err = v.newUser("user"); err != nil {
		return err
	}
	if v.display, err = v.newUser("display"); err != nil {
		return err
	}
	v.staff, err = v.newUser("staff")
	return err
}

func (v *verifier) adminCreate(path string, body any, out any) error {
	res, err := v.admin.Do(http.MethodPost, path, mustJSON(body), nil)
	if err != nil {
		return err
	}
	if res.Status != http.StatusCreated {
		return fmt.Errorf("POST %s: %w", path, res.Err())
	}
	return res.Decode(out)
}

const verifyPassword = "Verify-Pass-2026!"

func (v *verifier) email(role string) string {
	return fmt.Sprintf("verify.%s.%s.%s@ddo-case.invalid", strings.ToLower(v.run), role, strings.ToLower(newUUID()[:4]))
}

func (v *verifier) newUser(role string) (*Session, error) {
	email := v.email(role)
	if _, err := register(v.api, email, verifyPassword, "Verify "+role); err != nil {
		return nil, err
	}
	if role != "user" {
		id, err := findUserID(v.admin, email)
		if err != nil {
			return nil, err
		}
		res, err := v.admin.Do(http.MethodPatch, "/api/admin/users/"+id+"/role", mustJSON(map[string]string{"role": role}), nil)
		if err != nil {
			return nil, err
		}
		if res.Status != http.StatusOK {
			return nil, fmt.Errorf("set role %s: %w", role, res.Err())
		}
	}
	return NewSession(v.api, email, verifyPassword), nil
}

func (v *verifier) newTicket(s *Session, serviceID string) (Response, error) {
	return s.Do(http.MethodPost, "/api/tickets", mustJSON(map[string]string{"service_id": serviceID}),
		map[string]string{"Idempotency-Key": newUUID()})
}

func (v *verifier) callNext(s *Session, serviceID, counterID string) (Response, error) {
	return s.Do(http.MethodPost, "/api/queue/call-next",
		mustJSON(map[string]string{"service_id": serviceID, "counter_id": counterID}), nil)
}

// --- F-1 -------------------------------------------------------------------

func (v *verifier) checkAuth(a *assert) error {
	email := v.email("auth")
	reg := mustJSON(map[string]string{"email": email, "password": verifyPassword, "full_name": "Verify Auth"})
	res, err := v.api.Do(http.MethodPost, "/api/auth/register", "", reg, nil)
	if e := a.status("register", res, err, 201); e != nil {
		return e
	}
	res, err = v.api.Do(http.MethodPost, "/api/auth/register", "", reg, nil)
	if e := a.status("duplicate register", res, err, 409); e != nil {
		return e
	}
	res, err = v.api.Do(http.MethodPost, "/api/auth/register", "",
		mustJSON(map[string]string{"email": v.email("weak"), "password": "short", "full_name": "x"}), nil)
	if e := a.status("weak password rejected", res, err, 400); e != nil {
		return e
	}
	res, err = v.api.Do(http.MethodPost, "/api/auth/login", "", mustJSON(map[string]string{"email": email, "password": "wrong-password-1"}), nil)
	if e := a.status("wrong password", res, err, 401); e != nil {
		return e
	}
	res, err = v.api.Do(http.MethodPost, "/api/auth/login", "", mustJSON(map[string]string{"email": email, "password": verifyPassword}), nil)
	if e := a.status("login", res, err, 200); e != nil {
		return e
	}
	var tk struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	_ = res.Decode(&tk)
	parts := strings.Split(tk.AccessToken, ".")
	var hdr struct {
		Alg string `json:"alg"`
	}
	if len(parts) == 3 {
		raw, _ := base64.RawURLEncoding.DecodeString(parts[0])
		_ = json.Unmarshal(raw, &hdr)
	}
	if e := a.that(hdr.Alg == "HS256", "access token is not an HS256 JWT (alg=%q)", hdr.Alg); e != nil {
		return e
	}
	res, err = v.api.Do(http.MethodGet, "/api/auth/me", tk.AccessToken, nil, nil)
	if e := a.status("me", res, err, 200); e != nil {
		return e
	}
	res, err = v.api.Do(http.MethodGet, "/api/auth/me", "", nil, nil)
	if e := a.status("me without token", res, err, 401); e != nil {
		return e
	}
	res, err = v.api.Do(http.MethodPost, "/api/auth/refresh", "", mustJSON(map[string]string{"refresh_token": tk.RefreshToken}), nil)
	if e := a.status("refresh", res, err, 200); e != nil {
		return e
	}
	var tk2 struct {
		RefreshToken string `json:"refresh_token"`
	}
	_ = res.Decode(&tk2)
	if e := a.that(tk2.RefreshToken != "" && tk2.RefreshToken != tk.RefreshToken, "refresh token was not rotated"); e != nil {
		return e
	}
	res, err = v.api.Do(http.MethodPost, "/api/auth/refresh", "", mustJSON(map[string]string{"refresh_token": tk.RefreshToken}), nil)
	if e := a.status("replayed refresh token rejected", res, err, 401); e != nil {
		return e
	}
	res, err = v.api.Do(http.MethodPost, "/api/auth/logout", "", mustJSON(map[string]string{"refresh_token": tk2.RefreshToken}), nil)
	return a.status("logout", res, err, 204)
}

// --- F-2 -------------------------------------------------------------------

func (v *verifier) checkRBAC(a *assert) error {
	call := mustJSON(map[string]string{"service_id": v.service, "counter_id": v.counters[0]})
	board := "/api/queue/branches/" + v.branch + "/board"
	cases := []struct {
		name   string
		s      *Session
		method string
		path   string
		body   []byte
		want   int
	}{
		{"user → GET /api/admin/users", v.customer, "GET", "/api/admin/users", nil, 403},
		{"admin → GET /api/admin/users", v.admin, "GET", "/api/admin/users", nil, 200},
		{"user → board", v.customer, "GET", board, nil, 403},
		{"display → board", v.display, "GET", board, nil, 200},
		{"display → call-next", v.display, "POST", "/api/queue/call-next", call, 403},
		{"user → call-next", v.customer, "POST", "/api/queue/call-next", call, 403},
		{"staff → create branch", v.staff, "POST", "/api/queue/branches", mustJSON(map[string]string{"code": "NO-" + v.run, "name": "x"}), 403},
		{"user → audit logs", v.customer, "GET", "/api/admin/audit-logs", nil, 403},
		{"admin → audit logs", v.admin, "GET", "/api/admin/audit-logs", nil, 200},
	}
	for _, c := range cases {
		res, err := c.s.Do(c.method, c.path, c.body, nil)
		if e := a.status(c.name, res, err, c.want); e != nil {
			return e
		}
	}
	return nil
}

// --- F-3 -------------------------------------------------------------------

func (v *verifier) checkTickets(a *assert) error {
	body := mustJSON(map[string]string{"service_id": v.service})
	key := map[string]string{"Idempotency-Key": newUUID()}
	first, err := v.customer.Do(http.MethodPost, "/api/tickets", body, key)
	if e := a.status("create ticket", first, err, 201); e != nil {
		return e
	}
	var tr struct {
		Ticket struct {
			DisplayCode string `json:"display_code"`
			Status      string `json:"status"`
		} `json:"ticket"`
		EstimatedWait *json.RawMessage `json:"estimated_wait"`
	}
	_ = first.Decode(&tr)
	if e := a.that(strings.HasPrefix(tr.Ticket.DisplayCode, "A-") && tr.Ticket.Status == "waiting" && tr.EstimatedWait != nil,
		"unexpected ticket body: %s", truncate(first.Body)); e != nil {
		return e
	}
	again, err := v.customer.Do(http.MethodPost, "/api/tickets", body, key)
	if e := a.status("replay same key", again, err, 201); e != nil {
		return e
	}
	if e := a.that(again.Replayed && bytes.Equal(again.Body, first.Body), "replay not byte-identical or missing Idempotent-Replayed"); e != nil {
		return e
	}
	res, err := v.customer.Do(http.MethodPost, "/api/tickets", mustJSON(map[string]string{"service_id": v.raceService}), key)
	if e := a.status("same key, different body", res, err, 422); e != nil {
		return e
	}
	res, err = v.customer.Do(http.MethodPost, "/api/tickets", body, nil)
	if e := a.status("missing Idempotency-Key", res, err, 400); e != nil {
		return e
	}
	res, err = v.customer.Do(http.MethodPost, "/api/tickets", mustJSON(map[string]string{"service_id": "00000000-0000-0000-0000-000000000000"}),
		map[string]string{"Idempotency-Key": newUUID()})
	return a.status("unknown service", res, err, 404)
}

// --- F-4 -------------------------------------------------------------------

func (v *verifier) checkCallNext(a *assert) error {
	// Drain whatever F-3 left so FIFO can be checked from a known start.
	for {
		res, err := v.callNext(v.staff, v.service, v.counters[0])
		if err != nil || res.Status != 200 {
			break
		}
	}
	var ids [2]string
	for i := range ids {
		res, err := v.newTicket(v.customer, v.service)
		if e := a.status("create ticket", res, err, 201); e != nil {
			return e
		}
		var tr struct {
			Ticket struct{ ID string } `json:"ticket"`
		}
		_ = res.Decode(&tr)
		ids[i] = tr.Ticket.ID
	}
	for i, want := range ids {
		res, err := v.callNext(v.staff, v.service, v.counters[i])
		if e := a.status("call-next", res, err, 200); e != nil {
			return e
		}
		var t struct {
			ID        string `json:"id"`
			Status    string `json:"status"`
			CounterID string `json:"counter_id"`
		}
		_ = res.Decode(&t)
		if e := a.that(t.ID == want && t.Status == "called" && t.CounterID == v.counters[i],
			"call %d returned %s (status %s) — FIFO or counter assignment broken", i+1, t.ID, t.Status); e != nil {
			return e
		}
	}
	res, err := v.callNext(v.staff, v.service, v.counters[0])
	if e := a.status("call-next on empty queue", res, err, 204); e != nil {
		return e
	}
	finish := func(outcome string) (Response, error) {
		return v.staff.Do(http.MethodPost, "/api/queue/tickets/"+ids[0]+"/finish", mustJSON(map[string]string{"outcome": outcome}), nil)
	}
	res, err = finish("completed")
	if e := a.status("finish", res, err, 200); e != nil {
		return e
	}
	res, err = finish("completed")
	if e := a.status("finish twice", res, err, 409); e != nil {
		return e
	}
	res, err = finish("vanished")
	return a.status("invalid outcome", res, err, 400)
}

// --- F-5 -------------------------------------------------------------------

func (v *verifier) checkWaitStats(a *assert) error {
	path := "/api/queue/services/" + v.service + "/wait-stats?window=1h"
	res, err := v.staff.Do(http.MethodGet, path, nil, nil)
	if e := a.status("wait-stats", res, err, 200); e != nil {
		return e
	}
	var st struct {
		SampleSize    int      `json:"sample_size"`
		MedianSeconds *float64 `json:"median_seconds"`
		P90Seconds    *float64 `json:"p90_seconds"`
	}
	_ = res.Decode(&st)
	// F-3 and F-4 called at least two tickets of this service.
	if e := a.that(st.SampleSize >= 2 && st.MedianSeconds != nil && st.P90Seconds != nil && *st.P90Seconds >= *st.MedianSeconds,
		"unexpected stats: %s", truncate(res.Body)); e != nil {
		return e
	}
	res, err = v.staff.Do(http.MethodGet, "/api/queue/services/"+v.service+"/wait-stats?window=nope", nil, nil)
	if e := a.status("invalid window", res, err, 400); e != nil {
		return e
	}
	res, err = v.customer.Do(http.MethodGet, path, nil, nil)
	return a.status("customer cannot read stats", res, err, 403)
}

// --- F-6 -------------------------------------------------------------------

func (v *verifier) checkAppointments(a *assert) error {
	// The next quarter-hour slot at least 5 minutes ahead, so check-in is already open.
	slot := time.Now().Add(6 * time.Minute).Truncate(15 * time.Minute).Add(15 * time.Minute)
	book := func(s *Session, at time.Time, key string) (Response, error) {
		return s.Do(http.MethodPost, "/api/appointments",
			mustJSON(map[string]any{"service_id": v.service, "scheduled_at": at.UTC().Format(time.RFC3339)}),
			map[string]string{"Idempotency-Key": key})
	}
	key := newUUID()
	res, err := book(v.customer, slot, key)
	if e := a.status("book", res, err, 201); e != nil {
		return e
	}
	var ap struct {
		ID string `json:"id"`
	}
	_ = res.Decode(&ap)
	again, err := book(v.customer, slot, key)
	if e := a.status("book replay", again, err, 201); e != nil {
		return e
	}
	if e := a.that(again.Replayed && bytes.Equal(again.Body, res.Body), "booking replay not identical"); e != nil {
		return e
	}
	res, err = book(v.customer, slot, newUUID())
	if e := a.status("double booking", res, err, 409); e != nil {
		return e
	}
	res, err = book(v.customer, slot.Add(7*time.Minute), newUUID())
	if e := a.status("non-quarter-hour slot", res, err, 400); e != nil {
		return e
	}
	res, err = v.display.Do(http.MethodGet, "/api/appointments/"+ap.ID, nil, nil)
	if e := a.status("other user's appointment hidden", res, err, 404); e != nil {
		return e
	}
	res, err = v.customer.Do(http.MethodPost, "/api/appointments/"+ap.ID+"/check-in", nil, nil)
	if e := a.status("check-in", res, err, 200); e != nil {
		return e
	}
	var ci struct {
		Appointment struct{ Status string } `json:"appointment"`
		Ticket      struct{ Status string } `json:"ticket"`
	}
	_ = res.Decode(&ci)
	if e := a.that(ci.Appointment.Status == "checked_in" && ci.Ticket.Status == "waiting", "check-in did not issue a waiting ticket"); e != nil {
		return e
	}
	res, err = v.customer.Do(http.MethodPost, "/api/appointments/"+ap.ID+"/check-in", nil, nil)
	return a.status("second check-in", res, err, 409)
}

// --- T-1 -------------------------------------------------------------------

func (v *verifier) checkMark(a *assert) error {
	tok, err := v.customer.Token()
	if err != nil {
		return err
	}
	probes := []struct {
		name, method, path, token, body string
	}{
		{"200 /health", "GET", "/health", "", ""},
		{"200 /api/auth/me", "GET", "/api/auth/me", tok, ""},
		{"401", "GET", "/api/auth/me", "", ""},
		{"403", "GET", "/api/admin/users", tok, ""},
		{"404", "GET", "/no-such-endpoint", "", ""},
		{"405", "DELETE", "/health", "", ""},
		{"400", "POST", "/api/auth/login", "", "{broken"},
	}
	for _, p := range probes {
		req, _ := http.NewRequest(p.method, v.api.base+p.path, strings.NewReader(p.body))
		if p.token != "" {
			req.Header.Set("Authorization", "Bearer "+p.token)
		}
		res, err := v.api.http.Do(req)
		if err != nil {
			return fmt.Errorf("%s: %v", p.name, err)
		}
		_, _ = io.Copy(io.Discard, res.Body)
		res.Body.Close()
		if e := a.that(res.Header.Get("X-DDO-Mark") == "bfe1f2", "%s: X-DDO-Mark = %q", p.name, res.Header.Get("X-DDO-Mark")); e != nil {
			return e
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, res, err := websocket.Dial(ctx, v.wsURL, nil)
	if err != nil {
		return fmt.Errorf("websocket dial: %v", err)
	}
	defer c.CloseNow()
	return a.that(res.Header.Get("X-DDO-Mark") == "bfe1f2", "101 upgrade: X-DDO-Mark = %q", res.Header.Get("X-DDO-Mark"))
}

// --- T-2 -------------------------------------------------------------------

func (v *verifier) checkHealth(a *assert) error {
	res, err := v.api.Do(http.MethodGet, "/health", "", nil, nil)
	if e := a.status("/health", res, err, 200); e != nil {
		return e
	}
	if e := a.that(strings.Contains(string(res.Body), `"ok"`), "/health body: %s", truncate(res.Body)); e != nil {
		return e
	}
	res, err = v.api.Do(http.MethodGet, "/_hz/bfe1", "", nil, nil)
	if e := a.status("/_hz/bfe1", res, err, 200); e != nil {
		return e
	}
	var d struct {
		Status string `json:"status"`
		DB     struct {
			Reachable     bool  `json:"reachable"`
			SchemaVersion int64 `json:"schema_version"`
		} `json:"db"`
	}
	_ = res.Decode(&d)
	return a.that(d.Status == "ok" && d.DB.Reachable && d.DB.SchemaVersion >= 7,
		"/_hz/bfe1 = %s", truncate(res.Body))
}

// --- T-3 -------------------------------------------------------------------

type wsMsg struct {
	Type        string `json:"type"`
	ID          int64  `json:"id"`
	LastEventID int64  `json:"last_event_id"`
	Replayed    int    `json:"replayed"`
}

func (v *verifier) wsConnect(token string, last *int64) (*websocket.Conn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, v.wsURL, nil)
	if err != nil {
		return nil, err
	}
	sub := map[string]any{"type": "subscribe", "token": token, "branch_id": v.branch}
	if last != nil {
		sub["last_event_id"] = *last
	}
	return c, wsjson.Write(ctx, c, sub)
}

func wsRead(c *websocket.Conn) (wsMsg, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var m wsMsg
	err := wsjson.Read(ctx, c, &m)
	return m, err
}

func (v *verifier) checkWebSocket(a *assert) error {
	tok, err := v.display.Token()
	if err != nil {
		return err
	}
	c, err := v.wsConnect(tok, nil)
	if err != nil {
		return fmt.Errorf("connect: %v", err)
	}
	m, err := wsRead(c)
	if e := a.that(err == nil && m.Type == "snapshot", "first message %q (%v), want snapshot", m.Type, err); e != nil {
		c.CloseNow()
		return e
	}
	if res, err := v.newTicket(v.customer, v.service); a.status("create ticket", res, err, 201) != nil {
		c.CloseNow()
		return fmt.Errorf("create ticket failed")
	}
	m, err = wsRead(c)
	if e := a.that(err == nil && m.Type == "event" && m.ID > 0, "live event not received (%v)", err); e != nil {
		c.CloseNow()
		return e
	}
	last := m.ID
	c.Close(websocket.StatusNormalClosure, "simulated outage")

	const missed = 3
	for i := 0; i < missed; i++ {
		if res, err := v.newTicket(v.customer, v.service); a.status("create ticket while offline", res, err, 201) != nil {
			return fmt.Errorf("create ticket failed")
		}
	}
	c2, err := v.wsConnect(tok, &last)
	if err != nil {
		return fmt.Errorf("reconnect: %v", err)
	}
	defer c2.CloseNow()
	prev := last
	for i := 0; i < missed; i++ {
		m, err := wsRead(c2)
		if e := a.that(err == nil && m.Type == "event" && m.ID > prev, "replayed event %d: %+v (%v)", i+1, m, err); e != nil {
			return e
		}
		prev = m.ID
	}
	m, err = wsRead(c2)
	if e := a.that(err == nil && m.Type == "replay_complete" && m.Replayed == missed && m.LastEventID == prev,
		"expected replay_complete of %d, got %+v (%v)", missed, m, err); e != nil {
		return e
	}

	bad, err := v.wsConnect("not-a-token", nil)
	if err != nil {
		return err
	}
	defer bad.CloseNow()
	_, err = wsRead(bad)
	return a.that(websocket.CloseStatus(err) == 4001, "invalid token: close status %d, want 4001", websocket.CloseStatus(err))
}

// --- T-4 -------------------------------------------------------------------

func (v *verifier) checkRace(a *assert) error {
	const tickets, callers = 30, 60
	for i := 0; i < tickets; i++ {
		if res, err := v.newTicket(v.customer, v.raceService); a.status("create ticket", res, err, 201) != nil {
			return fmt.Errorf("create ticket %d failed: %v", i, err)
		}
	}
	if _, err := v.staff.Token(); err != nil {
		return err
	}

	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		seen  = map[string]int{}
		empty int
		fails []string
		start = make(chan struct{})
	)
	for i := 0; i < callers; i++ {
		counter := v.counters[i%len(v.counters)]
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			res, err := v.callNext(v.staff, v.raceService, counter)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err != nil:
				fails = append(fails, err.Error())
			case res.Status == 204:
				empty++
			case res.Status == 200:
				var t struct {
					ID string `json:"id"`
				}
				_ = res.Decode(&t)
				seen[t.ID]++
			default:
				fails = append(fails, fmt.Sprintf("HTTP %d", res.Status))
			}
		}()
	}
	close(start)
	wg.Wait()
	if e := a.that(len(fails) == 0, "call-next errors: %v", fails); e != nil {
		return e
	}
	dupes := 0
	for _, n := range seen {
		if n > 1 {
			dupes++
		}
	}
	if e := a.that(len(seen) == tickets && dupes == 0 && empty == callers-tickets,
		"%d callers: %d distinct tickets (want %d), %d double-assigned, %d empty", callers, len(seen), tickets, dupes, empty); e != nil {
		return e
	}

	// Same Idempotency-Key fired concurrently must create exactly one ticket.
	key := newUUID()
	body := mustJSON(map[string]string{"service_id": v.raceService})
	bodies := make([][]byte, 20)
	var wg2 sync.WaitGroup
	start2 := make(chan struct{})
	for i := range bodies {
		wg2.Add(1)
		go func(i int) {
			defer wg2.Done()
			<-start2
			if res, err := v.customer.Do(http.MethodPost, "/api/tickets", body, map[string]string{"Idempotency-Key": key}); err == nil && res.Status == 201 {
				bodies[i] = res.Body
			}
		}(i)
	}
	close(start2)
	wg2.Wait()
	for i, b := range bodies {
		if e := a.that(b != nil && bytes.Equal(b, bodies[0]), "concurrent same-key request %d got a different or failed response", i); e != nil {
			return e
		}
	}
	res, err := v.display.Do(http.MethodGet, "/api/queue/branches/"+v.branch+"/board", nil, nil)
	if e := a.status("board", res, err, 200); e != nil {
		return e
	}
	var board struct {
		Tickets []struct {
			ServiceID string `json:"service_id"`
			Status    string `json:"status"`
		} `json:"tickets"`
	}
	_ = res.Decode(&board)
	waiting := 0
	for _, t := range board.Tickets {
		if t.ServiceID == v.raceService && t.Status == "waiting" {
			waiting++
		}
	}
	return a.that(waiting == 1, "20 concurrent same-key requests created %d tickets, want 1", waiting)
}
