package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"os"
	"sync"
	"time"
)

type RunConfig struct {
	Duration         time.Duration
	TicketRate       float64 // per second, across all branches
	AppointmentRate  float64
	CallRate         float64
	RetryRatio       float64 // share of creates re-sent with the same Idempotency-Key
	Workers          int
	Branches         int
	Customers        int
	CustomerPassword string
	Seed             int64
}

type branchSetup struct {
	Branch
	Services []Service
	Counters []Counter
}

type runner struct {
	cfg       RunConfig
	admin     *Session
	customers []*Session
	branches  []branchSetup
	stats     *Stats

	rngMu sync.Mutex
	rng   *rand.Rand

	// The ticket each counter is currently serving, finished before it calls the next.
	servingMu sync.Mutex
	serving   map[string]string
}

func Run(ctx context.Context, api *API, admin *Session, cfg RunConfig) error {
	if err := requireAdmin(admin); err != nil {
		return err
	}
	branches, err := discover(admin, cfg.Branches)
	if err != nil {
		return err
	}
	r := &runner{
		cfg:      cfg,
		admin:    admin,
		branches: branches,
		stats:    NewStats(),
		rng:      rand.New(rand.NewSource(cfg.Seed)),
		serving:  map[string]string{},
	}
	for i := 1; i <= cfg.Customers; i++ {
		r.customers = append(r.customers, NewSession(api, customerEmail(i), cfg.CustomerPassword))
	}
	log.Printf("run: %d branches, %d customers, %s at %.0f tickets/s, %.0f appointments/s, %.0f calls/s",
		len(branches), len(r.customers), cfg.Duration, cfg.TicketRate, cfg.AppointmentRate, cfg.CallRate)

	ctx, cancel := context.WithTimeout(ctx, cfg.Duration)
	defer cancel()

	jobs := make(chan func(), cfg.Workers*2)
	var workers sync.WaitGroup
	for i := 0; i < cfg.Workers; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for job := range jobs {
				job()
			}
		}()
	}

	var producers sync.WaitGroup
	produce := func(rate float64, job func()) {
		if rate <= 0 {
			return
		}
		producers.Add(1)
		go func() {
			defer producers.Done()
			t := time.NewTicker(time.Duration(float64(time.Second) / rate))
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					select {
					case jobs <- job:
					default:
						r.stats.Dropped() // keep the offered rate honest instead of queueing unboundedly
					}
				}
			}
		}()
	}
	produce(cfg.TicketRate, r.createTicket)
	produce(cfg.AppointmentRate, r.createAppointment)
	produce(cfg.CallRate, r.callNext)

	start := time.Now()
	progress := time.NewTicker(10 * time.Second)
	defer progress.Stop()
loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		case <-progress.C:
			log.Print(r.stats.Progress(time.Since(start)))
		}
	}
	producers.Wait()
	close(jobs)
	workers.Wait()

	elapsed := time.Since(start)
	fmt.Println()
	r.stats.Report(os.Stdout, elapsed)
	if n := r.stats.Failures(); n > 0 {
		return fmt.Errorf("%d failures (5xx, network errors or idempotency mismatches)", n)
	}
	return nil
}

func discover(admin *Session, limit int) ([]branchSetup, error) {
	var bs struct {
		Branches []Branch `json:"branches"`
	}
	if err := admin.GetJSON("/api/queue/branches", &bs); err != nil {
		return nil, err
	}
	var svc struct {
		Services []Service `json:"services"`
	}
	if err := admin.GetJSON("/api/queue/services", &svc); err != nil {
		return nil, err
	}
	byBranch := map[string][]Service{}
	for _, s := range svc.Services {
		byBranch[s.BranchID] = append(byBranch[s.BranchID], s)
	}

	want := map[string]bool{}
	for i := 1; i <= limit; i++ {
		want[branchCode(i)] = true
	}
	var out []branchSetup
	for _, b := range bs.Branches {
		if !want[b.Code] {
			continue
		}
		var cs struct {
			Counters []Counter `json:"counters"`
		}
		if err := admin.GetJSON("/api/queue/branches/"+b.ID+"/counters", &cs); err != nil {
			return nil, err
		}
		if len(byBranch[b.ID]) == 0 || len(cs.Counters) == 0 {
			continue
		}
		out = append(out, branchSetup{Branch: b, Services: byBranch[b.ID], Counters: cs.Counters})
	}
	if len(out) == 0 {
		return nil, errors.New("no seeded branches found; run `simulator seed` first")
	}
	if len(out) < limit {
		log.Printf("run: only %d of %d branches are seeded", len(out), limit)
	}
	return out, nil
}

func (r *runner) intn(n int) int {
	r.rngMu.Lock()
	defer r.rngMu.Unlock()
	return r.rng.Intn(n)
}

func (r *runner) chance(p float64) bool {
	r.rngMu.Lock()
	defer r.rngMu.Unlock()
	return r.rng.Float64() < p
}

func (r *runner) pick() (branchSetup, Service, *Session) {
	b := r.branches[r.intn(len(r.branches))]
	return b, b.Services[r.intn(len(b.Services))], r.customers[r.intn(len(r.customers))]
}

// postIdempotent sends body with a fresh Idempotency-Key and, for a share of
// requests, sends the identical request again to check the replay contract:
// same status, byte-identical body, and the Idempotent-Replayed header.
func (r *runner) postIdempotent(op string, s *Session, path string, body []byte) {
	headers := map[string]string{"Idempotency-Key": newUUID()}
	first, err := s.Do(http.MethodPost, path, body, headers)
	r.stats.Record(op, first, err)
	if err != nil || first.Status >= 300 || !r.chance(r.cfg.RetryRatio) {
		return
	}
	again, err := s.Do(http.MethodPost, path, body, headers)
	r.stats.Record(op+".retry", again, err)
	if err != nil {
		return
	}
	ok := again.Status == first.Status && again.Replayed && bytes.Equal(again.Body, first.Body)
	r.stats.Idempotency(ok)
	if !ok {
		log.Printf("idempotency mismatch on %s: first %d, retry %d replayed=%v", path, first.Status, again.Status, again.Replayed)
	}
}

func (r *runner) createTicket() {
	_, svc, cust := r.pick()
	r.postIdempotent("ticket", cust, "/api/tickets", mustJSON(map[string]string{"service_id": svc.ID}))
}

func (r *runner) createAppointment() {
	_, svc, cust := r.pick()
	// A random quarter-hour slot between 30 minutes and 14 days from now.
	first := time.Now().Add(30 * time.Minute).Truncate(15 * time.Minute).Add(15 * time.Minute)
	slot := first.Add(time.Duration(r.intn(14*24*4)) * 15 * time.Minute)
	r.postIdempotent("appointment", cust, "/api/appointments", mustJSON(map[string]any{
		"service_id": svc.ID, "scheduled_at": slot.UTC().Format(time.RFC3339),
	}))
}

func (r *runner) callNext() {
	b, svc, _ := r.pick()
	counter := b.Counters[r.intn(len(b.Counters))]

	r.servingMu.Lock()
	prev := r.serving[counter.ID]
	delete(r.serving, counter.ID)
	r.servingMu.Unlock()
	if prev != "" {
		outcome := "completed"
		if r.chance(0.1) {
			outcome = "no_show"
		}
		res, err := r.admin.Do(http.MethodPost, "/api/queue/tickets/"+prev+"/finish",
			mustJSON(map[string]string{"outcome": outcome}), nil)
		r.stats.Record("finish", res, err)
	}

	res, err := r.admin.Do(http.MethodPost, "/api/queue/call-next",
		mustJSON(map[string]string{"service_id": svc.ID, "counter_id": counter.ID}), nil)
	r.stats.Record("call_next", res, err)
	if err == nil && res.Status == http.StatusOK {
		var t struct {
			ID string `json:"id"`
		}
		if res.Decode(&t) == nil {
			r.servingMu.Lock()
			r.serving[counter.ID] = t.ID
			r.servingMu.Unlock()
		}
	}
}
