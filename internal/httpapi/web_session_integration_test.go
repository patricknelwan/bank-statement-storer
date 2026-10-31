package httpapi

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"example.com/bca/internal/auth"
	"example.com/bca/internal/config"
	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/oauth2"
)

type webFixture struct {
	db      *pgxpool.Pool
	service *auth.Service
	router  http.Handler
	sub     string
}

func newWebFixture(t *testing.T, foreignOwner bool) webFixture {
	t.Helper()
	raw := os.Getenv("TEST_DATABASE_URL")
	if raw == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	db, err := pgxpool.New(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	sub := uuid.NewString()
	var service *auth.Service
	google := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/authorize" {
			state := r.URL.Query().Get("state")
			http.Redirect(w, r, service.Config.PublicURL+"/auth/google/callback?"+url.Values{"state": {state}, "code": {state}}.Encode(), http.StatusFound)
			return
		}
		if err := r.ParseForm(); err != nil {
			t.Error(err)
			w.WriteHeader(500)
			return
		}
		var nonce string
		if err := db.QueryRow(r.Context(), `SELECT nonce FROM auth_flows WHERE secret_hash=$1`, auth.Hash(r.Form.Get("code"))).Scan(&nonce); err != nil {
			t.Error(err)
			w.WriteHeader(500)
			return
		}
		tokenSub := sub
		if foreignOwner {
			tokenSub = "another-owner"
		}
		idToken, err := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{"iss": "https://issuer.example", "aud": "synthetic-client", "sub": tokenSub, "nonce": nonce, "email": "synthetic@example.invalid", "email_verified": true, "iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix()}).SignedString(key)
		if err != nil {
			t.Error(err)
			w.WriteHeader(500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "synthetic-access", "refresh_token": "synthetic-refresh", "token_type": "Bearer", "expires_in": 3600, "id_token": idToken})
	}))
	t.Cleanup(google.Close)
	service = &auth.Service{DB: db, Config: config.Config{PublicURL: "https://statement.test", OwnerGoogleSub: sub, JWTSecret: strings.Repeat("j", 32), WorkerToken: strings.Repeat("w", 32), EncryptionKey: bytes.Repeat([]byte{3}, 32)}, OAuth: oauth2.Config{ClientID: "synthetic-client", ClientSecret: "synthetic-secret", RedirectURL: "https://statement.test/auth/google/callback", Endpoint: oauth2.Endpoint{AuthURL: google.URL + "/authorize", TokenURL: google.URL + "/token"}}, Verifier: oidc.NewVerifier("https://issuer.example", &oidc.StaticKeySet{PublicKeys: []crypto.PublicKey{&key.PublicKey}}, &oidc.Config{ClientID: "synthetic-client"})}
	return webFixture{db, service, (API{DB: db, Auth: service, Config: config.Config{AccountHMACKey: strings.Repeat("a", 32)}}).Router(), sub}
}

func (f webFixture) request(method, path string, cookie *http.Cookie, csrf, origin string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "https://statement.test"+path, nil)
	if cookie != nil {
		r.AddCookie(cookie)
	}
	if csrf != "" {
		r.Header.Set("X-CSRF-Token", csrf)
	}
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	w := httptest.NewRecorder()
	f.router.ServeHTTP(w, r)
	return w
}

func (f webFixture) start(t *testing.T, returnTo string) (*http.Cookie, string) {
	t.Helper()
	w := f.request("GET", "/auth/google/start?client=web&return_to="+url.QueryEscape(returnTo), nil, "", "")
	if w.Code != 302 {
		t.Fatalf("web start: status=%d body=%s", w.Code, w.Body.String())
	}
	location, err := url.Parse(w.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("state cookies=%d", len(cookies))
	}
	return cookies[0], location.Query().Get("state")
}

func TestWebOAuthCookieSession(t *testing.T) {
	f := newWebFixture(t, false)
	stateCookie, state := f.start(t, "/transactions")
	var mode, destination string
	if err := f.db.QueryRow(context.Background(), `SELECT client_type,return_path FROM auth_flows WHERE secret_hash=$1`, auth.Hash(state)).Scan(&mode, &destination); err != nil || mode != "web" || destination != "/transactions" {
		t.Fatalf("persisted mode/return: %q %q %v", mode, destination, err)
	}
	callback := f.request("GET", "/auth/google/callback?state="+state+"&code="+state, stateCookie, "", "")
	if callback.Code != 303 || callback.Header().Get("Location") != "/transactions" {
		t.Fatalf("web callback: %d %s", callback.Code, callback.Body.String())
	}
	var cookie *http.Cookie
	for _, c := range callback.Result().Cookies() {
		if c.Name == "__Host-statement_session" {
			cookie = c
		}
	}
	if cookie == nil || !cookie.HttpOnly || !cookie.Secure || cookie.Path != "/" || cookie.Domain != "" || cookie.SameSite != http.SameSiteLaxMode || cookie.MaxAge != 30*24*60*60 {
		t.Fatalf("invalid session cookie: %+v", cookie)
	}
	if strings.Contains(callback.Body.String(), cookie.Value) {
		t.Fatal("credential exposed in callback body")
	}
	var id string
	var expiry time.Time
	if err := f.db.QueryRow(context.Background(), `SELECT id,expires_at FROM web_sessions WHERE secret_hash=$1`, auth.Hash(cookie.Value)).Scan(&id, &expiry); err != nil || time.Until(expiry) < 29*24*time.Hour || time.Until(expiry) > 30*24*time.Hour {
		t.Fatalf("hashed session/absolute expiry: %v", err)
	}
	var codes int
	if err := f.db.QueryRow(context.Background(), `SELECT count(*) FROM auth_flows WHERE kind='login_code' AND user_id=(SELECT user_id FROM web_sessions WHERE id=$1)`, id).Scan(&codes); err != nil || codes != 0 {
		t.Fatal("web flow emitted exchangeable code")
	}
	bootstrap := f.request("GET", "/api/v1/auth/session", cookie, "", "")
	var session struct {
		User      struct{ ID, Email, Timezone string }
		CSRF      string    `json:"csrf_token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if bootstrap.Code != 200 || json.Unmarshal(bootstrap.Body.Bytes(), &session) != nil || session.CSRF == "" || session.User.Email != "synthetic@example.invalid" || session.ExpiresAt.IsZero() {
		t.Fatalf("bootstrap: %d %s", bootstrap.Code, bootstrap.Body.String())
	}
	if bootstrap.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("session response cached")
	}
	if got := f.request("GET", "/api/v1/me", cookie, "", ""); got.Code != 200 {
		t.Fatalf("cookie owner read: %d", got.Code)
	}
	for _, tc := range []struct{ csrf, origin string }{{"", "https://statement.test"}, {session.CSRF, ""}, {session.CSRF, "https://evil.test"}, {"wrong", "https://statement.test"}} {
		if got := f.request("DELETE", "/api/v1/integrations/gmail", cookie, tc.csrf, tc.origin); got.Code != 403 {
			t.Fatalf("CSRF allowed: status %d", got.Code)
		}
	}
	ambiguous := httptest.NewRequest("GET", "https://statement.test/api/v1/me", nil)
	ambiguous.AddCookie(cookie)
	ambiguous.Header.Set("Authorization", "Bearer invalid")
	aw := httptest.NewRecorder()
	f.router.ServeHTTP(aw, ambiguous)
	if aw.Code != 400 {
		t.Fatalf("ambiguous credentials status=%d", aw.Code)
	}
	crossSite := httptest.NewRequest("GET", "https://statement.test/api/v1/me", nil)
	crossSite.AddCookie(cookie)
	crossSite.Header.Set("Sec-Fetch-Site", "cross-site")
	cw := httptest.NewRecorder()
	f.router.ServeHTTP(cw, crossSite)
	if cw.Code != 403 {
		t.Fatalf("cross-site read status=%d", cw.Code)
	}
	logout := f.request("POST", "/api/v1/auth/logout", cookie, session.CSRF, "https://statement.test")
	if logout.Code != 204 {
		t.Fatalf("logout status=%d body=%s", logout.Code, logout.Body.String())
	}
	if got := f.request("GET", "/api/v1/me", cookie, "", ""); got.Code != 401 {
		t.Fatalf("revoked cookie accepted: %d", got.Code)
	}
	replay := f.request("GET", "/auth/google/callback?state="+state+"&code="+state, stateCookie, "", "")
	if replay.Code == 303 && replay.Header().Get("Location") == "/transactions" {
		t.Fatal("OAuth replay accepted")
	}
}

func TestWebOAuthRejectsHostileReturnsAndExpiredFlows(t *testing.T) {
	f := newWebFixture(t, false)
	for _, returnTo := range []string{"https://evil.test", "//evil.test", "http:evil.test/transactions/00000000-0000-4000-8000-000000000001", "/transactions?code=secret", "/transactions/../settings", "/%2f%2fevil.test", "/settings#fragment", "/transactions/not-a-uuid"} {
		_, state := f.start(t, returnTo)
		var got string
		if err := f.db.QueryRow(context.Background(), `SELECT return_path FROM auth_flows WHERE secret_hash=$1`, auth.Hash(state)).Scan(&got); err != nil || got != "/transactions" {
			t.Fatalf("unsafe return %q => %q, %v", returnTo, got, err)
		}
	}
	cookie, state := f.start(t, "/settings")
	if _, err := f.db.Exec(context.Background(), `UPDATE auth_flows SET expires_at=now()-interval '1 second' WHERE secret_hash=$1`, auth.Hash(state)); err != nil {
		t.Fatal(err)
	}
	if got := f.request("GET", "/auth/google/callback?state="+state+"&code="+state, cookie, "", ""); got.Code == 303 && got.Header().Get("Location") == "/settings" {
		t.Fatal("expired flow accepted")
	}
}

func TestWebOwnerRejectionAndDatabaseOutage(t *testing.T) {
	f := newWebFixture(t, true)
	cookie, state := f.start(t, "/transactions")
	got := f.request("GET", "/auth/google/callback?state="+state+"&code="+state, cookie, "", "")
	if got.Code == 303 && got.Header().Get("Location") == "/transactions" {
		t.Fatal("foreign Google owner accepted")
	}
	for _, c := range got.Result().Cookies() {
		if c.Name == "__Host-statement_session" && c.MaxAge > 0 {
			t.Fatal("session issued to foreign owner")
		}
	}
	f.db.Close()
	got = f.request("GET", "/api/v1/auth/session", &http.Cookie{Name: "__Host-statement_session", Value: strings.Repeat("a", 42) + "A"}, "", "")
	if got.Code != 503 {
		t.Fatalf("database outage disguised as logout: %d", got.Code)
	}
}

func (f webFixture) login(t *testing.T, previous *http.Cookie) *http.Cookie {
	t.Helper()
	stateCookie, state := f.start(t, "/transactions")
	r := httptest.NewRequest("GET", "https://statement.test/auth/google/callback?state="+state+"&code="+state, nil)
	r.AddCookie(stateCookie)
	if previous != nil {
		r.AddCookie(previous)
	}
	w := httptest.NewRecorder()
	f.router.ServeHTTP(w, r)
	if w.Code != 303 || w.Header().Get("Location") != "/transactions" {
		t.Fatalf("login failed: %d %s", w.Code, w.Body.String())
	}
	for _, cookie := range w.Result().Cookies() {
		if cookie.Name == "__Host-statement_session" {
			return cookie
		}
	}
	t.Fatal("missing browser cookie")
	return nil
}

func TestWebExpiryReconnectAndCredentialSeparation(t *testing.T) {
	f := newWebFixture(t, false)
	old := f.login(t, nil)
	// Denied reconnect must not revoke the existing app session.
	stateCookie, state := f.start(t, "/settings")
	denied := f.request("GET", "/auth/google/callback?state="+state+"&error=access_denied", stateCookie, "", "")
	if denied.Code != 303 || denied.Header().Get("Location") != "/sign-in?reason=access_denied" {
		t.Fatalf("consent denial status=%d", denied.Code)
	}
	if got := f.request("GET", "/api/v1/me", old, "", ""); got.Code != 200 {
		t.Fatal("denied reconnect revoked the old session")
	}
	current := f.login(t, old)
	if current.Value == old.Value {
		t.Fatal("reconnect reused the session secret")
	}
	if got := f.request("GET", "/api/v1/me", old, "", ""); got.Code != 401 {
		t.Fatal("successful reconnect did not revoke old browser credential")
	}
	if got := f.request("GET", "/api/v1/webhooks/bca", current, "", ""); got.Code == 200 {
		t.Fatal("worker route became an owner endpoint")
	}
	if got := f.request("POST", "/api/v1/webhooks/bca", current, "", ""); got.Code != 401 {
		t.Fatalf("worker accepted owner cookie: %d", got.Code)
	}
	var userID string
	if err := f.db.QueryRow(context.Background(), `SELECT user_id FROM web_sessions WHERE secret_hash=$1`, auth.Hash(current.Value)).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	code, _ := auth.Random()
	if _, err := f.db.Exec(context.Background(), `INSERT INTO auth_flows(kind,secret_hash,user_id,expires_at) VALUES('login_code',$1,$2,now()+interval '1 minute')`, auth.Hash(code), userID); err != nil {
		t.Fatal(err)
	}
	pair, err := f.service.Exchange(context.Background(), code, "")
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "https://statement.test/api/v1/auth/session", nil)
	r.Header.Set("Authorization", "Bearer "+pair.AccessToken)
	w := httptest.NewRecorder()
	f.router.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal("browser bootstrap accepted bearer fallback")
	}
	if _, err := f.db.Exec(context.Background(), `UPDATE web_sessions SET expires_at=now()-interval '1 second' WHERE secret_hash=$1`, auth.Hash(current.Value)); err != nil {
		t.Fatal(err)
	}
	expired := f.request("GET", "/api/v1/me", current, "", "")
	if expired.Code != 401 || expired.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("expired credential status=%d", expired.Code)
	}
	foundClear := false
	for _, c := range expired.Result().Cookies() {
		if c.Name == current.Name && c.MaxAge < 0 {
			foundClear = true
		}
	}
	if !foundClear {
		t.Fatal("expired cookie was not cleared")
	}
}

func TestWebNonceMismatchAndFailedTransactionIssueNoCookie(t *testing.T) {
	f := newWebFixture(t, false)
	cookie, state := f.start(t, "/transactions")
	_, otherState := f.start(t, "/settings")
	badNonce := f.request("GET", "/auth/google/callback?state="+state+"&code="+otherState, cookie, "", "")
	if badNonce.Header().Get("Location") != "/sign-in?reason=authorization_failed" {
		t.Fatal("nonce mismatch accepted")
	}
	f.service.Config.EncryptionKey = nil
	cookie, state = f.start(t, "/transactions")
	failed := f.request("GET", "/auth/google/callback?state="+state+"&code="+state, cookie, "", "")
	for _, c := range failed.Result().Cookies() {
		if c.Name == "__Host-statement_session" && c.MaxAge > 0 {
			t.Fatal("failed transaction issued session credential")
		}
	}
	var owners int
	if err := f.db.QueryRow(context.Background(), `SELECT count(*) FROM users WHERE google_sub=$1`, f.sub).Scan(&owners); err != nil || owners != 0 {
		t.Fatal("failed OAuth/Gmail transaction committed owner data")
	}
}
