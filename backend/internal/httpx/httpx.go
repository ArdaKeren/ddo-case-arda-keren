// Package httpx holds small HTTP helpers shared by all handlers.
package httpx

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
)

const (
	MarkHeader = "X-DDO-Mark"
	MarkValue  = "bfe1f2"
)

// DDOMark stamps every response, including 404s and errors, so it must be
// registered before any other middleware that may short-circuit.
func DDOMark(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(MarkHeader, MarkValue)
		next.ServeHTTP(w, r)
	})
}

func JSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if v != nil {
		_ = json.NewEncoder(w).Encode(v)
	}
}

func Error(w http.ResponseWriter, status int, msg string) {
	JSON(w, status, map[string]string{"error": msg})
}

const maxBodyBytes = 1 << 20

// Decode reads a JSON body into dst, rejecting unknown fields and oversized bodies.
func Decode(w http.ResponseWriter, r *http.Request, dst any) error {
	body, err := ReadBody(w, r)
	if err != nil {
		return err
	}
	return DecodeBytes(body, dst)
}

// ReadBody returns the raw request body, capped at 1 MiB.
func ReadBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	return io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
}

// DecodeBytes applies the same strict rules as Decode to an already-read body.
func DecodeBytes(body []byte, dst any) error {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("body must contain a single JSON object")
	}
	return nil
}

// ClientIP returns the request's IP without port, or "" if it cannot be parsed.
// Pair with chi's RealIP middleware when running behind a trusted proxy.
func ClientIP(r *http.Request) string {
	host := r.RemoteAddr
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if net.ParseIP(host) == nil {
		return ""
	}
	return host
}
