package queue

import (
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/qa/backend/internal/audit"
	"github.com/qa/backend/internal/auth"
	"github.com/qa/backend/internal/httpx"
	"github.com/qa/backend/internal/idempotency"
)

// statsWindow is the look-back used for the wait estimate attached to new tickets.
const statsWindow = 2 * time.Hour

var (
	branchCodeRe  = regexp.MustCompile(`^[A-Z0-9-]{2,16}$`)
	serviceCodeRe = regexp.MustCompile(`^[A-Z]{1,3}$`)
	uuidRe        = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
)

type Handler struct {
	repo   *Repository
	tokens *auth.TokenIssuer
	audit  *audit.Logger
	now    func() time.Time
}

func NewHandler(repo *Repository, tokens *auth.TokenIssuer, auditLog *audit.Logger) *Handler {
	return &Handler{repo: repo, tokens: tokens, audit: auditLog, now: time.Now}
}

// TicketRoutes is mounted under /api/tickets.
func (h *Handler) TicketRoutes() chi.Router {
	r := chi.NewRouter()
	r.Use(auth.Authenticate(h.tokens))
	r.With(auth.RequirePermission(auth.PermTicketsCreate)).Post("/", h.createTicket)
	r.Get("/{id}", h.getTicket)
	return r
}

// AppointmentRoutes is mounted under /api/appointments.
func (h *Handler) AppointmentRoutes() chi.Router {
	r := chi.NewRouter()
	r.Use(auth.Authenticate(h.tokens))
	r.With(auth.RequirePermission(auth.PermAppointmentsCreate)).Post("/", h.createAppointment)
	r.Get("/{id}", h.getAppointment)
	r.Post("/{id}/check-in", h.checkIn)
	return r
}

// QueueRoutes is mounted under /api/queue.
func (h *Handler) QueueRoutes() chi.Router {
	r := chi.NewRouter()
	r.Use(auth.Authenticate(h.tokens))
	r.Get("/branches", h.listBranches)
	r.Get("/services", h.listServices)
	r.With(auth.RequirePermission(auth.PermQueueRead)).Get("/branches/{id}/counters", h.listCounters)
	r.With(auth.RequirePermission(auth.PermQueueRead)).Get("/branches/{id}/board", h.board)
	r.With(auth.RequirePermission(auth.PermQueueManage)).Post("/branches", h.createBranch)
	r.With(auth.RequirePermission(auth.PermQueueManage)).Post("/services", h.createService)
	r.With(auth.RequirePermission(auth.PermQueueManage)).Post("/counters", h.createCounter)
	r.With(auth.RequirePermission(auth.PermQueueCall)).Post("/call-next", h.callNext)
	r.With(auth.RequirePermission(auth.PermQueueCall)).Post("/tickets/{id}/finish", h.finish)
	r.With(auth.RequirePermission(auth.PermQueueRead)).Get("/services/{id}/wait-stats", h.waitStats)
	return r
}

// apiError is a client-facing failure returned from inside an idempotent
// operation. It is sent to the client but never stored for replay, because the
// transaction is rolled back and the client may fix the problem and retry.
type apiError struct {
	status int
	msg    string
}

func (e *apiError) Error() string { return e.msg }

func badRequest(msg string) error { return &apiError{http.StatusBadRequest, msg} }

// runIdempotent executes op inside a transaction guarded by the request's
// Idempotency-Key. The successful response is stored in the same transaction and
// replayed verbatim for retries. It returns true when op ran and committed now
// (as opposed to a replay or a failure), so callers can audit exactly once.
func (h *Handler) runIdempotent(w http.ResponseWriter, r *http.Request, endpoint string,
	op func(tx *sql.Tx, body []byte) (status int, resp any, err error)) bool {
	p, _ := auth.PrincipalFrom(r.Context())

	key := r.Header.Get(idempotency.Header)
	if !idempotency.ValidKey(key) {
		httpx.Error(w, http.StatusBadRequest, "Idempotency-Key header is required (1-255 printable ASCII characters)")
		return false
	}
	body, err := httpx.ReadBody(w, r)
	if err != nil {
		httpx.Error(w, http.StatusRequestEntityTooLarge, "request body too large")
		return false
	}
	reqHash := idempotency.RequestHash(r.Method, r.URL.Path, body)

	tx, err := h.repo.DB().BeginTx(r.Context(), nil)
	if err != nil {
		h.internal(w, err)
		return false
	}
	defer tx.Rollback()

	stored, err := idempotency.Claim(r.Context(), tx, p.UserID, endpoint, key, reqHash)
	switch {
	case errors.Is(err, idempotency.ErrKeyReused):
		httpx.Error(w, http.StatusUnprocessableEntity, err.Error())
		return false
	case errors.Is(err, idempotency.ErrInProgress):
		httpx.Error(w, http.StatusConflict, err.Error())
		return false
	case err != nil:
		h.internal(w, err)
		return false
	case stored != nil:
		w.Header().Set(idempotency.ReplayedHeader, "true")
		writeRaw(w, stored.StatusCode, stored.Body)
		return false
	}

	status, resp, err := op(tx, body)
	var ae *apiError
	if errors.As(err, &ae) {
		httpx.Error(w, ae.status, ae.msg)
		return false
	}
	if err != nil {
		h.internal(w, err)
		return false
	}
	raw, err := json.Marshal(resp)
	if err != nil {
		h.internal(w, err)
		return false
	}
	if err := idempotency.Save(r.Context(), tx, p.UserID, endpoint, key, status, raw); err != nil {
		h.internal(w, err)
		return false
	}
	if err := tx.Commit(); err != nil {
		h.internal(w, err)
		return false
	}
	writeRaw(w, status, raw)
	return true
}

func writeRaw(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

type ticketResponse struct {
	Ticket        Ticket    `json:"ticket"`
	Position      int64     `json:"position"`
	EstimatedWait WaitStats `json:"estimated_wait"`
}

func (h *Handler) createTicket(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFrom(r.Context())
	var created Ticket
	ok := h.runIdempotent(w, r, "POST /api/tickets", func(tx *sql.Tx, body []byte) (int, any, error) {
		var in struct {
			ServiceID string `json:"service_id"`
		}
		if err := httpx.DecodeBytes(body, &in); err != nil || !uuidRe.MatchString(in.ServiceID) {
			return 0, nil, badRequest("service_id (uuid) is required")
		}
		t, err := h.repo.CreateTicket(r.Context(), tx, in.ServiceID, &p.UserID)
		if errors.Is(err, ErrServiceNotFound) {
			return 0, nil, &apiError{http.StatusNotFound, err.Error()}
		}
		if err != nil {
			return 0, nil, err
		}
		pos, err := h.repo.Position(r.Context(), tx, t)
		if err != nil {
			return 0, nil, err
		}
		stats, err := h.repo.WaitTimeStats(r.Context(), tx, t.ServiceID, statsWindow)
		if err != nil {
			return 0, nil, err
		}
		created = t
		return http.StatusCreated, ticketResponse{Ticket: t, Position: pos, EstimatedWait: stats}, nil
	})
	if ok {
		h.audit.Log(r, audit.Entry{ActorUserID: p.UserID, Action: "ticket.create", EntityType: "qa-tickets",
			EntityID: created.ID, Metadata: map[string]any{"display_code": created.DisplayCode}})
	}
}

func (h *Handler) getTicket(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFrom(r.Context())
	id := chi.URLParam(r, "id")
	if !uuidRe.MatchString(id) {
		httpx.Error(w, http.StatusNotFound, "ticket not found")
		return
	}
	t, err := h.repo.TicketByID(r.Context(), id)
	// Other users' tickets are reported as missing rather than forbidden so IDs cannot be probed.
	isOwner := err == nil && t.CustomerUserID != nil && *t.CustomerUserID == p.UserID
	if errors.Is(err, ErrNotFound) || (err == nil && !isOwner && !p.Role.Can(auth.PermQueueRead)) {
		httpx.Error(w, http.StatusNotFound, "ticket not found")
		return
	}
	if err != nil {
		h.internal(w, err)
		return
	}
	var pos int64
	if t.Status == StatusWaiting {
		if pos, err = h.repo.Position(r.Context(), h.repo.DB(), t); err != nil {
			h.internal(w, err)
			return
		}
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"ticket": t, "position": pos})
}

func (h *Handler) createAppointment(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFrom(r.Context())
	var created Appointment
	ok := h.runIdempotent(w, r, "POST /api/appointments", func(tx *sql.Tx, body []byte) (int, any, error) {
		var in struct {
			ServiceID   string    `json:"service_id"`
			ScheduledAt time.Time `json:"scheduled_at"`
		}
		if err := httpx.DecodeBytes(body, &in); err != nil || !uuidRe.MatchString(in.ServiceID) {
			return 0, nil, badRequest("service_id (uuid) and scheduled_at (RFC 3339) are required")
		}
		if !ValidSlot(in.ScheduledAt, h.now()) {
			return 0, nil, badRequest("scheduled_at must be a quarter-hour slot between 5 minutes and 90 days from now")
		}
		a, err := h.repo.CreateAppointment(r.Context(), tx, in.ServiceID, p.UserID, in.ScheduledAt)
		switch {
		case errors.Is(err, ErrServiceNotFound):
			return 0, nil, &apiError{http.StatusNotFound, err.Error()}
		case errors.Is(err, ErrSlotTaken):
			return 0, nil, &apiError{http.StatusConflict, err.Error()}
		case err != nil:
			return 0, nil, err
		}
		created = a
		return http.StatusCreated, a, nil
	})
	if ok {
		h.audit.Log(r, audit.Entry{ActorUserID: p.UserID, Action: "appointment.create", EntityType: "qa-appointments",
			EntityID: created.ID})
	}
}

// appointmentFor loads an appointment the caller may act on: their own, or any
// if they are staff. Others' appointments are reported as missing.
func (h *Handler) appointmentFor(w http.ResponseWriter, r *http.Request) (Appointment, bool) {
	p, _ := auth.PrincipalFrom(r.Context())
	id := chi.URLParam(r, "id")
	if !uuidRe.MatchString(id) {
		httpx.Error(w, http.StatusNotFound, "appointment not found")
		return Appointment{}, false
	}
	a, err := h.repo.AppointmentByID(r.Context(), id)
	isOwner := err == nil && a.CustomerUserID != nil && *a.CustomerUserID == p.UserID
	if errors.Is(err, ErrNotFound) || (err == nil && !isOwner && !p.Role.Can(auth.PermQueueCall)) {
		httpx.Error(w, http.StatusNotFound, "appointment not found")
		return Appointment{}, false
	}
	if err != nil {
		h.internal(w, err)
		return Appointment{}, false
	}
	return a, true
}

func (h *Handler) getAppointment(w http.ResponseWriter, r *http.Request) {
	if a, ok := h.appointmentFor(w, r); ok {
		httpx.JSON(w, http.StatusOK, a)
	}
}

func (h *Handler) checkIn(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFrom(r.Context())
	a, ok := h.appointmentFor(w, r)
	if !ok {
		return
	}
	a, t, err := h.repo.CheckIn(r.Context(), a.ID, h.now())
	switch {
	case errors.Is(err, ErrNotCheckInnable), errors.Is(err, ErrCheckInWindow):
		httpx.Error(w, http.StatusConflict, err.Error())
		return
	case errors.Is(err, ErrServiceNotFound):
		httpx.Error(w, http.StatusConflict, "the appointment's service is no longer active")
		return
	case err != nil:
		h.internal(w, err)
		return
	}
	h.audit.Log(r, audit.Entry{ActorUserID: p.UserID, Action: "appointment.check_in", EntityType: "qa-appointments",
		EntityID: a.ID, Metadata: map[string]any{"ticket_id": t.ID}})
	httpx.JSON(w, http.StatusOK, map[string]any{"appointment": a, "ticket": t})
}

func (h *Handler) callNext(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFrom(r.Context())
	var in struct {
		ServiceID string `json:"service_id"`
		CounterID string `json:"counter_id"`
	}
	if err := httpx.Decode(w, r, &in); err != nil || !uuidRe.MatchString(in.ServiceID) || !uuidRe.MatchString(in.CounterID) {
		httpx.Error(w, http.StatusBadRequest, "service_id and counter_id (uuid) are required")
		return
	}
	t, err := h.repo.CallNext(r.Context(), in.ServiceID, in.CounterID, p.UserID)
	switch {
	case errors.Is(err, ErrQueueEmpty):
		w.WriteHeader(http.StatusNoContent)
		return
	case errors.Is(err, ErrCounterNotFound):
		httpx.Error(w, http.StatusNotFound, err.Error())
		return
	case err != nil:
		h.internal(w, err)
		return
	}
	h.audit.Log(r, audit.Entry{ActorUserID: p.UserID, Action: "queue.call_next", EntityType: "qa-tickets",
		EntityID: t.ID, Metadata: map[string]any{"display_code": t.DisplayCode, "counter_id": in.CounterID}})
	httpx.JSON(w, http.StatusOK, t)
}

func (h *Handler) finish(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFrom(r.Context())
	var in struct {
		Outcome string `json:"outcome"`
	}
	if err := httpx.Decode(w, r, &in); err != nil || (in.Outcome != StatusCompleted && in.Outcome != StatusNoShow) {
		httpx.Error(w, http.StatusBadRequest, "outcome must be completed or no_show")
		return
	}
	id := chi.URLParam(r, "id")
	if !uuidRe.MatchString(id) {
		httpx.Error(w, http.StatusNotFound, "ticket not found")
		return
	}
	t, err := h.repo.Finish(r.Context(), id, in.Outcome)
	switch {
	case errors.Is(err, ErrNotFound):
		httpx.Error(w, http.StatusNotFound, "ticket not found")
		return
	case errors.Is(err, ErrInvalidTransition):
		httpx.Error(w, http.StatusConflict, err.Error())
		return
	case err != nil:
		h.internal(w, err)
		return
	}
	h.audit.Log(r, audit.Entry{ActorUserID: p.UserID, Action: "queue.finish", EntityType: "qa-tickets",
		EntityID: t.ID, Metadata: map[string]any{"outcome": in.Outcome}})
	httpx.JSON(w, http.StatusOK, t)
}

func (h *Handler) waitStats(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if !uuidRe.MatchString(id) {
		httpx.Error(w, http.StatusNotFound, "service not found")
		return
	}
	window := statsWindow
	if v := r.URL.Query().Get("window"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 || d > 30*24*time.Hour {
			httpx.Error(w, http.StatusBadRequest, "window must be a duration between 1s and 720h, e.g. 2h")
			return
		}
		window = d
	}
	st, err := h.repo.WaitTimeStats(r.Context(), h.repo.DB(), id, window)
	if err != nil {
		h.internal(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, st)
}

func (h *Handler) board(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if !uuidRe.MatchString(id) {
		httpx.Error(w, http.StatusNotFound, "branch not found")
		return
	}
	b, err := h.repo.Board(r.Context(), id)
	if err != nil {
		h.internal(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, b)
}

func (h *Handler) listBranches(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFrom(r.Context())
	branches, err := h.repo.ListBranches(r.Context(), !p.Role.Can(auth.PermQueueManage))
	if err != nil {
		h.internal(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"branches": branches})
}

func (h *Handler) listServices(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFrom(r.Context())
	branchID := r.URL.Query().Get("branch_id")
	if branchID != "" && !uuidRe.MatchString(branchID) {
		httpx.Error(w, http.StatusBadRequest, "branch_id must be a uuid")
		return
	}
	services, err := h.repo.ListServices(r.Context(), branchID, !p.Role.Can(auth.PermQueueManage))
	if err != nil {
		h.internal(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"services": services})
}

func (h *Handler) listCounters(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if !uuidRe.MatchString(id) {
		httpx.Error(w, http.StatusNotFound, "branch not found")
		return
	}
	counters, err := h.repo.ListCounters(r.Context(), id)
	if err != nil {
		h.internal(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"counters": counters})
}

func (h *Handler) createBranch(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Code string `json:"code"`
		Name string `json:"name"`
	}
	if err := httpx.Decode(w, r, &in); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	in.Code, in.Name = strings.ToUpper(strings.TrimSpace(in.Code)), strings.TrimSpace(in.Name)
	if !branchCodeRe.MatchString(in.Code) || in.Name == "" {
		httpx.Error(w, http.StatusBadRequest, "code must be 2-16 chars of A-Z, 0-9 or '-' and name is required")
		return
	}
	b, err := h.repo.CreateBranch(r.Context(), in.Code, in.Name)
	if errors.Is(err, ErrDuplicate) {
		httpx.Error(w, http.StatusConflict, "branch code already exists")
		return
	}
	if err != nil {
		h.internal(w, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, b)
}

func (h *Handler) createService(w http.ResponseWriter, r *http.Request) {
	var in struct {
		BranchID string `json:"branch_id"`
		Code     string `json:"code"`
		Name     string `json:"name"`
	}
	if err := httpx.Decode(w, r, &in); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	in.Code, in.Name = strings.ToUpper(strings.TrimSpace(in.Code)), strings.TrimSpace(in.Name)
	if !uuidRe.MatchString(in.BranchID) || !serviceCodeRe.MatchString(in.Code) || in.Name == "" {
		httpx.Error(w, http.StatusBadRequest, "branch_id (uuid), code (1-3 letters A-Z) and name are required")
		return
	}
	s, err := h.repo.CreateService(r.Context(), in.BranchID, in.Code, in.Name)
	switch {
	case errors.Is(err, ErrBranchNotFound):
		httpx.Error(w, http.StatusNotFound, err.Error())
		return
	case errors.Is(err, ErrDuplicate):
		httpx.Error(w, http.StatusConflict, "service code already exists in this branch")
		return
	case err != nil:
		h.internal(w, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, s)
}

func (h *Handler) createCounter(w http.ResponseWriter, r *http.Request) {
	var in struct {
		BranchID string `json:"branch_id"`
		Name     string `json:"name"`
	}
	if err := httpx.Decode(w, r, &in); err != nil || !uuidRe.MatchString(in.BranchID) || strings.TrimSpace(in.Name) == "" {
		httpx.Error(w, http.StatusBadRequest, "branch_id (uuid) and name are required")
		return
	}
	c, err := h.repo.CreateCounter(r.Context(), in.BranchID, strings.TrimSpace(in.Name))
	switch {
	case errors.Is(err, ErrBranchNotFound):
		httpx.Error(w, http.StatusNotFound, err.Error())
		return
	case errors.Is(err, ErrDuplicate):
		httpx.Error(w, http.StatusConflict, "counter name already exists in this branch")
		return
	case err != nil:
		h.internal(w, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, c)
}

func (h *Handler) internal(w http.ResponseWriter, err error) {
	log.Printf("queue: %v", err)
	httpx.Error(w, http.StatusInternalServerError, "internal error")
}
