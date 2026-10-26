// Package realtime streams branch queue events to WebSocket clients and lets
// reconnecting clients recover the events they missed.
package realtime

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/qa/backend/internal/queue"
)

const (
	notifyChannel  = "qa_queue_events"
	fetchPageSize  = 500
	subscriberBuf  = 1024
	pollInterval   = 2 * time.Second
	reconnectDelay = 2 * time.Second
)

// Hub tails "qa-queue_events" for every branch that has at least one subscriber
// and fans new events out to them.
//
// It reads from the table rather than trusting NOTIFY payloads, so it works with
// several backend instances and cannot lose events: NOTIFY only wakes it up, and
// a periodic poll covers notifications missed while the listener reconnects.
type Hub struct {
	repo *queue.Repository
	dsn  string

	mu       sync.Mutex
	branches map[string]*branchFeed

	// fetchMu serializes fetches so each branch cursor advances in one place.
	fetchMu sync.Mutex
}

type branchFeed struct {
	cursor int64
	subs   map[*Subscriber]struct{}
}

// Subscriber receives a branch's events on C. If it falls more than
// subscriberBuf events behind, Dropped is closed and it gets nothing more; the
// client is expected to reconnect and recover from its last event id.
type Subscriber struct {
	BranchID string
	C        chan queue.Event
	Dropped  chan struct{}
	dropOnce sync.Once
}

func (s *Subscriber) drop() { s.dropOnce.Do(func() { close(s.Dropped) }) }

func NewHub(repo *queue.Repository, dsn string) *Hub {
	return &Hub{repo: repo, dsn: dsn, branches: map[string]*branchFeed{}}
}

// Subscribe starts delivering branchID's events that are committed from now on.
// Callers that need older events must read them from the repository themselves
// and de-duplicate by event id, since the two sources may overlap.
func (h *Hub) Subscribe(ctx context.Context, branchID string) (*Subscriber, error) {
	sub := &Subscriber{BranchID: branchID, C: make(chan queue.Event, subscriberBuf), Dropped: make(chan struct{})}

	h.mu.Lock()
	feed, ok := h.branches[branchID]
	if ok {
		feed.subs[sub] = struct{}{}
		h.mu.Unlock()
		return sub, nil
	}
	h.mu.Unlock()

	latest, err := h.repo.LatestEventID(ctx, branchID)
	if err != nil {
		return nil, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if feed, ok = h.branches[branchID]; !ok { // another subscriber may have won the race
		feed = &branchFeed{cursor: latest, subs: map[*Subscriber]struct{}{}}
		h.branches[branchID] = feed
	}
	feed.subs[sub] = struct{}{}
	return sub, nil
}

func (h *Hub) Unsubscribe(sub *Subscriber) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if feed, ok := h.branches[sub.BranchID]; ok {
		delete(feed.subs, sub)
		if len(feed.subs) == 0 {
			delete(h.branches, sub.BranchID)
		}
	}
}

// Run blocks until ctx is cancelled.
func (h *Hub) Run(ctx context.Context) {
	go h.listen(ctx)
	t := time.NewTicker(pollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			h.fetchAll(ctx)
		}
	}
}

func (h *Hub) listen(ctx context.Context) {
	for ctx.Err() == nil {
		if err := h.listenOnce(ctx); err != nil && ctx.Err() == nil {
			log.Printf("realtime: listener: %v; reconnecting in %s", err, reconnectDelay)
			select {
			case <-ctx.Done():
			case <-time.After(reconnectDelay):
			}
		}
	}
}

func (h *Hub) listenOnce(ctx context.Context) error {
	conn, err := pgx.Connect(ctx, h.dsn)
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	if _, err := conn.Exec(ctx, "LISTEN "+notifyChannel); err != nil {
		return err
	}
	h.fetchAll(ctx) // catch up on anything committed while we were not listening
	for {
		n, err := conn.WaitForNotification(ctx)
		if err != nil {
			return err
		}
		h.fetch(ctx, n.Payload)
	}
}

func (h *Hub) fetchAll(ctx context.Context) {
	h.mu.Lock()
	ids := make([]string, 0, len(h.branches))
	for id := range h.branches {
		ids = append(ids, id)
	}
	h.mu.Unlock()
	for _, id := range ids {
		h.fetch(ctx, id)
	}
}

func (h *Hub) fetch(ctx context.Context, branchID string) {
	h.fetchMu.Lock()
	defer h.fetchMu.Unlock()

	for {
		h.mu.Lock()
		feed, ok := h.branches[branchID]
		var cursor int64
		if ok {
			cursor = feed.cursor
		}
		h.mu.Unlock()
		if !ok {
			return
		}

		events, err := h.repo.EventsAfter(ctx, branchID, cursor, fetchPageSize)
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("realtime: fetch %s: %v", branchID, err)
			}
			return
		}
		if len(events) == 0 {
			return
		}

		h.mu.Lock()
		// The feed may have been removed and recreated while we were querying.
		if current, ok := h.branches[branchID]; ok && current == feed {
			for _, ev := range events {
				for sub := range feed.subs {
					select {
					case sub.C <- ev:
					default:
						sub.drop()
						delete(feed.subs, sub)
					}
				}
			}
			feed.cursor = events[len(events)-1].ID
		}
		h.mu.Unlock()

		if len(events) < fetchPageSize {
			return
		}
	}
}
