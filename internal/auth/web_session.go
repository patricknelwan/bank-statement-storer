package auth

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"example.com/bca/internal/config"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const browserKind = "browser"
const webSessionLifetime = 30 * 24 * time.Hour

func safeReturnPath(raw string) string {
	if strings.ContainsAny(raw, "%\\?#") {
		return "/transactions"
	}
	switch raw {
	case "/transactions", "/imports", "/settings":
		return raw
	}
	parts := strings.Split(raw, "/")
	if len(parts) == 3 && parts[0] == "" && (parts[1] == "transactions" || parts[1] == "imports") {
		if id, err := uuid.Parse(parts[2]); err == nil && id.String() == parts[2] {
			return raw
		}
	}
	return "/transactions"
}

func (s *Service) webCookie(value string, expiry time.Time) *http.Cookie {
	secure := strings.HasPrefix(s.Config.PublicURL, "https://")
	name := "statement_session"
	if secure {
		name = "__Host-statement_session"
	}
	return &http.Cookie{Name: name, Value: value, Path: "/", Secure: secure, HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: int(webSessionLifetime.Seconds()), Expires: expiry}
}

func (s *Service) clearWebCookie(w http.ResponseWriter) {
	cookie := s.webCookie("", time.Unix(1, 0))
	cookie.MaxAge = -1
	http.SetCookie(w, cookie)
}

func (s *Service) issueWebSession(ctx context.Context, tx pgx.Tx, userID string, r *http.Request) (string, time.Time, error) {
	secret, err := Random()
	if err != nil {
		return "", time.Time{}, err
	}
	csrf, err := Random()
	if err != nil {
		return "", time.Time{}, err
	}
	expiry := time.Now().Add(webSessionLifetime)
	if _, err = tx.Exec(ctx, `INSERT INTO web_sessions(user_id,secret_hash,csrf_token,expires_at) VALUES($1,$2,$3,$4)`, userID, Hash(secret), csrf, expiry); err != nil {
		return "", time.Time{}, err
	}
	if old, err := r.Cookie(s.webCookie("", expiry).Name); err == nil {
		if _, err = tx.Exec(ctx, `UPDATE web_sessions SET revoked_at=now() WHERE secret_hash=$1 AND user_id=$2 AND revoked_at IS NULL`, Hash(old.Value), userID); err != nil {
			return "", time.Time{}, err
		}
	}
	return secret, expiry, nil
}

func authFailure(w http.ResponseWriter, r *http.Request, status int, code string) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"code": code, "request_id": middleware.GetReqID(r.Context())})
}

func oauthFailure(w http.ResponseWriter, r *http.Request, client string, status int, message string) {
	if client == "web" {
		reason := "authorization_failed"
		if message == "consent denied" {
			reason = "access_denied"
		}
		http.Redirect(w, r, "/sign-in?reason="+reason, http.StatusSeeOther)
		return
	}
	http.Error(w, message, status)
}

func (s *Service) webOwner(w http.ResponseWriter, r *http.Request, cookie *http.Cookie, next http.Handler) {
	if _, err := config.PublicOrigin(s.Config.PublicURL); err != nil {
		authFailure(w, r, 503, "unavailable")
		return
	}
	if !validChallenge(cookie.Value) {
		s.clearWebCookie(w)
		reject(w, r)
		return
	}
	var p Principal
	p.Kind = browserKind
	err := s.DB.QueryRow(r.Context(), `SELECT ws.id,ws.user_id,ws.csrf_token,ws.expires_at FROM web_sessions ws JOIN users u ON u.id=ws.user_id WHERE ws.secret_hash=$1 AND ws.revoked_at IS NULL AND ws.expires_at>now() AND u.google_sub=$2`, Hash(cookie.Value), s.Config.OwnerGoogleSub).Scan(&p.SessionID, &p.UserID, &p.CSRFToken, &p.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		s.clearWebCookie(w)
		reject(w, r)
		return
	}
	if err != nil {
		authFailure(w, r, 503, "unavailable")
		return
	}
	if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		authFailure(w, r, 403, "cross_site_request")
		return
	}
	switch r.Method {
	case http.MethodPost, http.MethodPatch, http.MethodDelete, http.MethodPut:
		expected, _ := config.PublicOrigin(s.Config.PublicURL)
		origin, err := url.Parse(r.Header.Get("Origin"))
		if err != nil || origin.String() != expected || subtle.ConstantTimeCompare([]byte(r.Header.Get("X-CSRF-Token")), []byte(p.CSRFToken)) != 1 {
			authFailure(w, r, 403, "csrf_failed")
			return
		}
	}
	next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, p)))
}

func (s *Service) BrowserSession(w http.ResponseWriter, r *http.Request) {
	p := FromContext(r.Context())
	if p.Kind != browserKind {
		reject(w, r)
		return
	}
	var id, email, timezone string
	if err := s.DB.QueryRow(r.Context(), `SELECT id,email,timezone FROM users WHERE id=$1`, p.UserID).Scan(&id, &email, &timezone); err != nil {
		authFailure(w, r, 503, "unavailable")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"user": map[string]string{"id": id, "email": email, "timezone": timezone}, "csrf_token": p.CSRFToken, "expires_at": p.ExpiresAt})
}

func (s *Service) ClearBrowserSessionCookie(w http.ResponseWriter, r *http.Request) {
	if FromContext(r.Context()).Kind == browserKind {
		s.clearWebCookie(w)
	}
}
