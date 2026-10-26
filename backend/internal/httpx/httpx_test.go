package httpx

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
)

func TestDDOMarkOnEveryResponse(t *testing.T) {
	r := chi.NewRouter()
	r.Use(DDOMark)
	r.Get("/ok", func(w http.ResponseWriter, r *http.Request) { JSON(w, http.StatusOK, nil) })
	r.Get("/boom", func(w http.ResponseWriter, r *http.Request) { Error(w, http.StatusInternalServerError, "x") })

	for _, path := range []string{"/ok", "/boom", "/missing"} {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if got := rec.Header().Get("X-DDO-Mark"); got != "bfe1f2" {
			t.Errorf("%s: X-DDO-Mark = %q", path, got)
		}
	}
}
