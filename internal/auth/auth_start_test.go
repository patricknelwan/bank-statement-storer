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

func TestMobileStartRequiresConfiguredCallbackAndChallenge(t *testing.T) {
	challenge := appChallenge("0123456789012345678901234567890123456789012")
	for _, tc := range []struct {
		name, callback, challenge string
	}{
		{"missing callback", "", challenge},
		{"invalid challenge", "bcatracking://auth/callback", "invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := Service{Config: config.Config{PublicURL: "http://127.0.0.1:18080", MobileRedirectURI: tc.callback}}
			w := httptest.NewRecorder()
			s.Start(w, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:18080/auth/google/start?client=mobile&code_challenge="+tc.challenge, nil))
			if w.Code != http.StatusBadRequest || w.Header().Get("Set-Cookie") != "" {
				t.Fatalf("status=%d cookie=%q", w.Code, w.Header().Get("Set-Cookie"))
			}
		})
	}
}

func TestMobileStartCanonicalRedirectKeepsChallenge(t *testing.T) {
	challenge := appChallenge("0123456789012345678901234567890123456789012")
	s := Service{Config: config.Config{PublicURL: "http://127.0.0.1:18080", MobileRedirectURI: "bcatracking://auth/callback"}}
	w := httptest.NewRecorder()
	s.Start(w, httptest.NewRequest(http.MethodGet, "http://localhost:18080/auth/google/start?client=mobile&code_challenge="+challenge, nil))
	if w.Code != http.StatusFound || w.Header().Get("Location") != "http://127.0.0.1:18080/auth/google/start?client=mobile&code_challenge="+challenge {
		t.Fatalf("status=%d location=%q", w.Code, w.Header().Get("Location"))
	}
}

func TestAppChallenge(t *testing.T) {
	verifier := "0123456789012345678901234567890123456789012"
	challenge := appChallenge(verifier)
	if !validChallenge(challenge) || appChallenge("bad") != "" || challenge == appChallenge(verifier+"x") {
		t.Fatal("invalid app PKCE challenge handling")
	}
}
