package realtime

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"regexp"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/qa/backend/internal/auth"
	"github.com/qa/backend/internal/queue"
)

// Application close codes (4000-4999 are reserved for applications by RFC 6455).
const (
	CloseUnauthorized = websocket.StatusCode(4001) // bad or expired token: refresh and reconnect
	CloseForbidden    = websocket.StatusCode(4003) // valid token, missing permission: do not retry
	CloseBadRequest   = websocket.StatusCode(4400) // malformed subscribe message or unknown branch
	CloseAuthTimeout  = websocket.StatusCode(4408) // no subscribe message in time
)

const (
	authTimeout  = 5 * time.Second
	writeTimeout = 10 * time.Second
	pingInterval = 25 * time.Second
	// A client further behind than this gets a fresh snapshot instead of a replay.
	maxReplay = 5000
)

var uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// Protocol
//
//	client → {"type":"subscribe","token":"<jwt>","branch_id":"<uuid>","last_event_id":123}
//	                                         (last_event_id omitted on first connect)
//	server → {"type":"snapshot","last_event_id":N,"board":{...}}        fresh state, or
//	server → {"type":"event",...} × k, then {"type":"replay_complete","last_event_id":N,"replayed":k}
//	server → {"type":"event","id":N,"event_type":"ticket.called","occurred_at":"...","data":{...}}  live
//
// The token travels in the first message rather than the URL so it does not end
// up in access logs or browser history.
type subscribeMsg struct {
	Type        string `json:"type"`
	Token       string `json:"token"`
	BranchID    string `json:"branch_id"`
	LastEventID *int64 `json:"last_event_id"`
}

type eventMsg struct {
	Type       string          `json:"type"`
	ID         int64           `json:"id"`
	EventType  string          `json:"event_type"`
	OccurredAt time.Time       `json:"occurred_at"`
	Data       json.RawMessage `json:"data"`
}

type snapshotMsg struct {
	Type        string      `json:"type"`
	LastEventID int64       `json:"last_event_id"`
	Board       queue.Board `json:"board"`
}

type replayCompleteMsg struct {
	Type        string `json:"type"`
	LastEventID int64  `json:"last_event_id"`
	Replayed    int    `json:"replayed"`
}

type Server struct {
	hub     *Hub
	repo    *queue.Repository
	tokens  *auth.TokenIssuer
	origins []string
}

// NewServer accepts cross-origin connections only from the given host patterns
// (e.g. "localhost:5173"); same-origin connections are always accepted.
func NewServer(hub *Hub, repo *queue.Repository, tokens *auth.TokenIssuer, allowedOrigins []string) *Server {
	return &Server{hub: hub, repo: repo, tokens: tokens, origins: allowedOrigins}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// http.Server read/write timeouts would otherwise persist on the hijacked
	// connection and kill long-lived sockets.
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Time{})
	_ = rc.SetWriteDeadline(time.Time{})

	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: s.origins})
	if err != nil {
		return // Accept has already written the HTTP error
	}
	defer c.CloseNow()
	c.SetReadLimit(8 << 10)

	if err := s.serve(r.Context(), c); err != nil && !isNormalClose(err) {
		log.Printf("realtime: %v", err)
	}
}

func (s *Server) serve(ctx context.Context, c *websocket.Conn) error {
	authCtx, cancel := context.WithTimeout(ctx, authTimeout)
	var msg subscribeMsg
	err := wsjson.Read(authCtx, c, &msg)
	cancel()
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return c.Close(CloseAuthTimeout, "subscribe message not received")
		}
		return c.Close(CloseBadRequest, "expected a JSON subscribe message")
	}
	if msg.Type != "subscribe" || !uuidRe.MatchString(msg.BranchID) {
		return c.Close(CloseBadRequest, "expected {type: subscribe, token, branch_id}")
	}
	claims, err := s.tokens.Parse(msg.Token)
	if err != nil {
		return c.Close(CloseUnauthorized, "invalid or expired token")
	}
	if !claims.Role.Can(auth.PermQueueRead) {
		return c.Close(CloseForbidden, "queue:read permission required")
	}
	if ok, err := s.repo.BranchExists(ctx, msg.BranchID); err != nil {
		return err
	} else if !ok {
		return c.Close(CloseBadRequest, "unknown branch")
	}

	// Subscribe before reading history so nothing committed in between is lost;
	// overlap between the two is removed by the cursor check below.
	sub, err := s.hub.Subscribe(ctx, msg.BranchID)
	if err != nil {
		return err
	}
	defer s.hub.Unsubscribe(sub)

	cursor, err := s.catchUp(ctx, c, msg)
	if err != nil {
		return err
	}

	// From here on the client only listens; CloseRead handles control frames and
	// cancels ctx when the peer goes away.
	ctx = c.CloseRead(ctx)
	expiry := time.NewTimer(time.Until(claims.ExpiresAt.Time))
	defer expiry.Stop()
	ping := time.NewTicker(pingInterval)
	defer ping.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-sub.Dropped:
			return c.Close(websocket.StatusTryAgainLater, "client too slow; reconnect with last_event_id")
		case <-expiry.C:
			return c.Close(CloseUnauthorized, "token expired")
		case <-ping.C:
			pctx, cancel := context.WithTimeout(ctx, writeTimeout)
			err := c.Ping(pctx)
			cancel()
			if err != nil {
				return nil
			}
		case ev := <-sub.C:
			if ev.ID <= cursor {
				continue // already sent during catch-up
			}
			if err := write(ctx, c, toEventMsg(ev)); err != nil {
				return err
			}
			cursor = ev.ID
		}
	}
}

// catchUp brings the client to the present and returns the last event id it has.
func (s *Server) catchUp(ctx context.Context, c *websocket.Conn, msg subscribeMsg) (int64, error) {
	if msg.LastEventID != nil {
		ok, err := s.repo.CanResumeFrom(ctx, msg.BranchID, *msg.LastEventID, maxReplay)
		if err != nil {
			return 0, err
		}
		if ok {
			cursor, replayed := *msg.LastEventID, 0
			for {
				events, err := s.repo.EventsAfter(ctx, msg.BranchID, cursor, fetchPageSize)
				if err != nil {
					return 0, err
				}
				for _, ev := range events {
					if err := write(ctx, c, toEventMsg(ev)); err != nil {
						return 0, err
					}
					cursor = ev.ID
					replayed++
				}
				if len(events) < fetchPageSize {
					break
				}
			}
			return cursor, write(ctx, c, replayCompleteMsg{Type: "replay_complete", LastEventID: cursor, Replayed: replayed})
		}
	}

	board, err := s.repo.Board(ctx, msg.BranchID)
	if err != nil {
		return 0, err
	}
	return board.LastEventID, write(ctx, c, snapshotMsg{Type: "snapshot", LastEventID: board.LastEventID, Board: board})
}

func toEventMsg(ev queue.Event) eventMsg {
	return eventMsg{Type: "event", ID: ev.ID, EventType: ev.Type, OccurredAt: ev.OccurredAt, Data: ev.Payload}
}

func write(ctx context.Context, c *websocket.Conn, v any) error {
	ctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	return wsjson.Write(ctx, c, v)
}

func isNormalClose(err error) bool {
	switch websocket.CloseStatus(err) {
	case websocket.StatusNormalClosure, websocket.StatusGoingAway:
		return true
	}
	return errors.Is(err, context.Canceled)
}
