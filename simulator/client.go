package main

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// API is a thin JSON client for the backend.
type API struct {
	base string
	http *http.Client
}

func NewAPI(base string, maxConns int) *API {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.MaxIdleConns = maxConns
	tr.MaxIdleConnsPerHost = maxConns
	return &API{base: base, http: &http.Client{Timeout: 20 * time.Second, Transport: tr}}
}

type Response struct {
	Status   int
	Body     []byte
	Replayed bool
	Latency  time.Duration
}

func (r Response) Decode(v any) error { return json.Unmarshal(r.Body, v) }

func (r Response) Err() error {
	var e struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(r.Body, &e)
	if e.Error == "" {
		e.Error = string(r.Body)
	}
	return fmt.Errorf("HTTP %d: %s", r.Status, e.Error)
}

func (a *API) Do(method, path, token string, body []byte, headers map[string]string) (Response, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, a.base+path, rd)
	if err != nil {
		return Response{}, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	start := time.Now()
	res, err := a.http.Do(req)
	if err != nil {
		return Response{}, err
	}
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		return Response{}, err
	}
	return Response{
		Status:   res.StatusCode,
		Body:     b,
		Replayed: res.Header.Get("Idempotent-Replayed") == "true",
		Latency:  time.Since(start),
	}, nil
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// Session logs in lazily and logs in again when the access token is rejected.
// Re-login (rather than refresh-token rotation) keeps concurrent workers that
// share a session from tripping the server's refresh-token reuse detection.
type Session struct {
	api      *API
	Email    string
	password string

	mu     sync.Mutex
	token  string
	expiry time.Time
}

func NewSession(api *API, email, password string) *Session {
	return &Session{api: api, Email: email, password: password}
}

var ErrLogin = errors.New("login failed")

func (s *Session) Token() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.token != "" && time.Until(s.expiry) > 30*time.Second {
		return s.token, nil
	}
	res, err := s.api.Do(http.MethodPost, "/api/auth/login", "",
		mustJSON(map[string]string{"email": s.Email, "password": s.password}), nil)
	if err != nil {
		return "", err
	}
	if res.Status != http.StatusOK {
		return "", fmt.Errorf("%w for %s: %v", ErrLogin, s.Email, res.Err())
	}
	var tr struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if err := res.Decode(&tr); err != nil {
		return "", err
	}
	s.token, s.expiry = tr.AccessToken, time.Now().Add(time.Duration(tr.ExpiresIn)*time.Second)
	return s.token, nil
}

func (s *Session) invalidate(token string) {
	s.mu.Lock()
	if s.token == token {
		s.token = ""
	}
	s.mu.Unlock()
}

// Do sends an authenticated request, retrying once with a fresh login on 401.
func (s *Session) Do(method, path string, body []byte, headers map[string]string) (Response, error) {
	for attempt := 0; ; attempt++ {
		tok, err := s.Token()
		if err != nil {
			return Response{}, err
		}
		res, err := s.api.Do(method, path, tok, body, headers)
		if err == nil && res.Status == http.StatusUnauthorized && attempt == 0 {
			s.invalidate(tok)
			continue
		}
		return res, err
	}
}

func (s *Session) GetJSON(path string, v any) error {
	res, err := s.Do(http.MethodGet, path, nil, nil)
	if err != nil {
		return err
	}
	if res.Status != http.StatusOK {
		return res.Err()
	}
	return res.Decode(v)
}

// newUUID returns a random RFC 4122 version 4 UUID for Idempotency-Key headers.
func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
