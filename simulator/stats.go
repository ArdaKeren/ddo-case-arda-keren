package main

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"text/tabwriter"
	"time"
)

type opStats struct {
	byStatus  map[int]int
	transport int
	latencies []time.Duration
}

// Stats collects per-operation outcomes and latencies across workers.
type Stats struct {
	mu  sync.Mutex
	ops map[string]*opStats

	IdempotencyOK     int
	IdempotencyFailed int
	Backpressure      int
}

func NewStats() *Stats { return &Stats{ops: map[string]*opStats{}} }

func (s *Stats) op(name string) *opStats {
	o, ok := s.ops[name]
	if !ok {
		o = &opStats{byStatus: map[int]int{}}
		s.ops[name] = o
	}
	return o
}

func (s *Stats) Record(name string, res Response, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o := s.op(name)
	if err != nil {
		o.transport++
		return
	}
	o.byStatus[res.Status]++
	o.latencies = append(o.latencies, res.Latency)
}

func (s *Stats) Idempotency(ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ok {
		s.IdempotencyOK++
	} else {
		s.IdempotencyFailed++
	}
}

func (s *Stats) Dropped() {
	s.mu.Lock()
	s.Backpressure++
	s.mu.Unlock()
}

// Failures counts outcomes that indicate a bug or outage rather than expected
// business responses (e.g. 409 slot taken, 204 empty queue).
func (s *Stats) Failures() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.IdempotencyFailed
	for _, o := range s.ops {
		n += o.transport
		for code, c := range o.byStatus {
			if code >= 500 {
				n += c
			}
		}
	}
	return n
}

func (s *Stats) Progress(elapsed time.Duration) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	names := s.names()
	parts := make([]string, 0, len(names))
	for _, n := range names {
		o := s.ops[n]
		total := o.transport
		for _, c := range o.byStatus {
			total += c
		}
		parts = append(parts, fmt.Sprintf("%s=%d", n, total))
	}
	return fmt.Sprintf("[%5.0fs] %s", elapsed.Seconds(), strings.Join(parts, " "))
}

func (s *Stats) Report(w io.Writer, elapsed time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(tw, "op\ttotal\trate/s\tstatus codes\tnet err\tp50\tp90\tp99\t")
	for _, n := range s.names() {
		o := s.ops[n]
		total := o.transport
		codes := make([]int, 0, len(o.byStatus))
		for code, c := range o.byStatus {
			total += c
			codes = append(codes, code)
		}
		sort.Ints(codes)
		cs := make([]string, len(codes))
		for i, code := range codes {
			cs[i] = fmt.Sprintf("%d:%d", code, o.byStatus[code])
		}
		p := percentiles(o.latencies, 0.5, 0.9, 0.99)
		fmt.Fprintf(tw, "%s\t%d\t%.1f\t%s\t%d\t%s\t%s\t%s\t\n", n, total, float64(total)/elapsed.Seconds(),
			strings.Join(cs, " "), o.transport, ms(p[0]), ms(p[1]), ms(p[2]))
	}
	tw.Flush()
	fmt.Fprintf(w, "\nidempotency replays verified: %d ok, %d mismatched\n", s.IdempotencyOK, s.IdempotencyFailed)
	if s.Backpressure > 0 {
		fmt.Fprintf(w, "jobs skipped because all workers were busy: %d (raise -workers or lower the rates)\n", s.Backpressure)
	}
}

func (s *Stats) names() []string {
	names := make([]string, 0, len(s.ops))
	for n := range s.ops {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func percentiles(d []time.Duration, qs ...float64) []time.Duration {
	out := make([]time.Duration, len(qs))
	if len(d) == 0 {
		return out
	}
	sorted := append([]time.Duration(nil), d...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	for i, q := range qs {
		out[i] = sorted[int(q*float64(len(sorted)-1))]
	}
	return out
}

func ms(d time.Duration) string {
	if d == 0 {
		return "-"
	}
	return fmt.Sprintf("%.1fms", float64(d.Microseconds())/1000)
}
