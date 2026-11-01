package ui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/shubhamstrength/hull/internal/store"
)

func TestGuard(t *testing.T) {
	t.Setenv("HULL_HOME", t.TempDir())
	st, _ := store.Open()
	h := (&Server{Store: st, Version: "test"}).Handler()
	do := func(method, host, path string, hdr bool) int {
		r := httptest.NewRequest(method, path, strings.NewReader(`{}`))
		r.Host = host
		if hdr {
			r.Header.Set("X-Hull", "1")
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	if c := do("GET", "127.0.0.1:7070", "/api/images", false); c != http.StatusOK {
		t.Errorf("local GET = %d", c)
	}
	if c := do("GET", "localhost:7070", "/", false); c != http.StatusOK {
		t.Errorf("dashboard page = %d", c)
	}
	if c := do("GET", "attacker.example:7070", "/api/images", false); c != http.StatusForbidden {
		t.Errorf("rebinding host = %d, want 403", c)
	}
	if c := do("POST", "127.0.0.1:7070", "/api/images/remove", false); c != http.StatusForbidden {
		t.Errorf("POST without header = %d, want 403", c)
	}
	if c := do("POST", "127.0.0.1:7070", "/api/images/remove", true); c != http.StatusBadRequest {
		t.Errorf("POST with header but no ref = %d, want 400", c)
	}
}
