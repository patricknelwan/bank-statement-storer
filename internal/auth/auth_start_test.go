package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"example.com/bca/internal/config"
)

func TestStartRedirectsOtherHostBeforeSettingStateCookie(t *testing.T) {
	s := Service{Config: config.Config{PublicURL: "http://127.0.0.1:18080"}}
	w := httptest.NewRecorder()
	s.Start(w, httptest.NewRequest(http.MethodGet, "http://localhost:18080/auth/google/start", nil))
	if w.Code != http.StatusFound {
		t.Fatalf("status = %d", w.Code)
	}
	if got := w.Header().Get("Location"); got != "http://127.0.0.1:18080/auth/google/start" {
		t.Fatalf("location = %q", got)
	}
	if w.Header().Get("Set-Cookie") != "" {
		t.Fatal("state cookie set for noncanonical host")
	}
}
