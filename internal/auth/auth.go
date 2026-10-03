package auth

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net/http"
	"strings"
	"time"

	"example.com/bca/internal/config"
	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

type Service struct {
	DB       *pgxpool.Pool
	Config   config.Config
	OAuth    oauth2.Config
	Verifier *oidc.IDTokenVerifier
}

type Principal struct{ UserID, SessionID string }
type ctxKey struct{}

func New(ctx context.Context, db *pgxpool.Pool, c config.Config) (*Service, error) {
	provider, err := oidc.NewProvider(ctx, "https://accounts.google.com")
	if err != nil {
		return nil, err
	}
	return &Service{DB: db, Config: c, OAuth: oauth2.Config{
		ClientID: c.GoogleClientID, ClientSecret: c.GoogleClientSecret,
		RedirectURL: c.GoogleRedirectURI, Endpoint: google.Endpoint,
		Scopes: []string{oidc.ScopeOpenID, "email", "https://www.googleapis.com/auth/gmail.readonly"},
	}, Verifier: provider.Verifier(&oidc.Config{ClientID: c.GoogleClientID})}, nil
}

func Random() (string, error) {
	var b [32]byte
	_, err := rand.Read(b[:])
	return base64.RawURLEncoding.EncodeToString(b[:]), err
}
func Hash(s string) []byte { h := sha256.Sum256([]byte(s)); return h[:] }

func (s *Service) Encrypt(plain string, context string) ([]byte, error) {
	block, err := aes.NewCipher(s.Config.EncryptionKey)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, []byte(plain), []byte(context)), nil
}
func (s *Service) Decrypt(ciphertext []byte, context string) (string, error) {
	block, err := aes.NewCipher(s.Config.EncryptionKey)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(ciphertext) < gcm.NonceSize() {
		return "", errors.New("invalid ciphertext")
	}
	plain, err := gcm.Open(nil, ciphertext[:gcm.NonceSize()], ciphertext[gcm.NonceSize():], []byte(context))
	return string(plain), err
}

func (s *Service) Start(w http.ResponseWriter, r *http.Request) {
	state, e1 := Random()
	nonce, e2 := Random()
	verifier := oauth2.GenerateVerifier()
	if e1 != nil || e2 != nil {
		http.Error(w, "unavailable", 503)
		return
	}
	_, err := s.DB.Exec(r.Context(), `INSERT INTO auth_flows(kind,secret_hash,nonce,pkce_verifier,expires_at) VALUES('oauth',$1,$2,$3,now()+interval '5 minutes')`, Hash(state), nonce, verifier)
	if err != nil {
		http.Error(w, "unavailable", 503)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "bca_oauth_state", Value: state, Path: "/auth/google/callback", HttpOnly: true, Secure: strings.HasPrefix(s.Config.PublicURL, "https://"), SameSite: http.SameSiteLaxMode, MaxAge: 300})
	http.Redirect(w, r, s.OAuth.AuthCodeURL(state, oauth2.AccessTypeOffline, oauth2.SetAuthURLParam("prompt", "consent"), oauth2.S256ChallengeOption(verifier), oauth2.SetAuthURLParam("nonce", nonce)), http.StatusFound)
}

func (s *Service) Callback(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie("bca_oauth_state")
	http.SetCookie(w, &http.Cookie{Name: "bca_oauth_state", Path: "/auth/google/callback", MaxAge: -1, HttpOnly: true})
	if err != nil || r.URL.Query().Get("state") == "" || subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(r.URL.Query().Get("state"))) != 1 || r.URL.Query().Get("code") == "" {
		http.Error(w, "invalid authorization flow", 400)
		return
	}
	var nonce, verifier string
	err = s.DB.QueryRow(r.Context(), `UPDATE auth_flows SET consumed_at=now() WHERE kind='oauth' AND secret_hash=$1 AND consumed_at IS NULL AND expires_at>now() RETURNING nonce,pkce_verifier`, Hash(cookie.Value)).Scan(&nonce, &verifier)
	if err != nil {
		http.Error(w, "invalid authorization flow", 400)
		return
	}
	token, err := s.OAuth.Exchange(r.Context(), r.URL.Query().Get("code"), oauth2.VerifierOption(verifier))
	if err != nil {
		http.Error(w, "authorization failed", 401)
		return
	}
	rawID, ok := token.Extra("id_token").(string)
	if !ok {
		http.Error(w, "identity unavailable", 401)
		return
	}
	id, err := s.Verifier.Verify(r.Context(), rawID)
	if err != nil {
		http.Error(w, "identity invalid", 401)
		return
	}
	var claims struct {
		Nonce         string `json:"nonce"`
		Email         string `json:"email"`
		EmailVerified bool   `json:"email_verified"`
	}
	if err := id.Claims(&claims); err != nil || claims.Nonce != nonce || !claims.EmailVerified || id.Subject != s.Config.OwnerGoogleSub {
		http.Error(w, "owner identity rejected", 403)
		return
	}
	tx, err := s.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	var userID, integrationID string
	err = tx.QueryRow(r.Context(), `INSERT INTO users(google_sub,email) VALUES($1,$2) ON CONFLICT(google_sub) DO UPDATE SET email=EXCLUDED.email RETURNING id`, id.Subject, claims.Email).Scan(&userID)
	if err != nil {
		http.Error(w, "unavailable", 503)
		return
	}
	err = tx.QueryRow(r.Context(), `INSERT INTO gmail_integrations(user_id,mailbox_id,status) VALUES($1,$2,'connected') ON CONFLICT(user_id) DO UPDATE SET status='connected' RETURNING id`, userID, id.Subject).Scan(&integrationID)
	if err != nil {
		http.Error(w, "unavailable", 503)
		return
	}
	if token.RefreshToken != "" {
		encrypted, e := s.Encrypt(token.RefreshToken, integrationID)
		if e != nil {
			http.Error(w, "unavailable", 503)
			return
		}
		_, err = tx.Exec(r.Context(), `UPDATE gmail_integrations SET encrypted_refresh_token=$2 WHERE id=$1`, integrationID, encrypted)
		if err != nil {
			http.Error(w, "unavailable", 503)
			return
		}
	}
	var hasToken bool
	err = tx.QueryRow(r.Context(), `SELECT encrypted_refresh_token IS NOT NULL FROM gmail_integrations WHERE id=$1`, integrationID).Scan(&hasToken)
	if err != nil || !hasToken {
		http.Error(w, "offline access unavailable; reconnect with consent", 400)
		return
	}
	_, err = tx.Exec(r.Context(), `INSERT INTO gmail_sync_state(integration_id,coverage_start) VALUES($1,now()-interval '30 days') ON CONFLICT DO NOTHING`, integrationID)
	if err != nil {
		http.Error(w, "unavailable", 503)
		return
	}
	code, err := Random()
	if err != nil {
		http.Error(w, "unavailable", 503)
		return
	}
	_, err = tx.Exec(r.Context(), `INSERT INTO auth_flows(kind,secret_hash,user_id,expires_at) VALUES('login_code',$1,$2,now()+interval '60 seconds')`, Hash(code), userID)
	if err != nil || tx.Commit(r.Context()) != nil {
		http.Error(w, "unavailable", 503)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	fmt.Fprintf(w, "<!doctype html><html><body><h1>BCA connected</h1><p>Copy this one-time application code into Postman within 60 seconds:</p><code>%s</code></body></html>", html.EscapeString(code))
}

type TokenPair struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
}

func (s *Service) issue(ctx context.Context, userID, familyID string, expiry time.Time) (TokenPair, error) {
	refresh, err := Random()
	if err != nil {
		return TokenPair{}, err
	}
	var sessionID string
	err = s.DB.QueryRow(ctx, `INSERT INTO sessions(user_id,family_id,refresh_hash,expires_at) VALUES($1,$2,$3,$4) RETURNING id`, userID, familyID, Hash(refresh), expiry).Scan(&sessionID)
	if err != nil {
		return TokenPair{}, err
	}
	access, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"iss": "bca-backend", "aud": "bca-owner", "sub": userID, "sid": sessionID,
		"iat": time.Now().Unix(), "exp": time.Now().Add(15 * time.Minute).Unix(),
	}).SignedString([]byte(s.Config.JWTSecret))
	return TokenPair{access, refresh, 900}, err
}

func (s *Service) Exchange(ctx context.Context, code string) (TokenPair, error) {
	var userID string
	err := s.DB.QueryRow(ctx, `UPDATE auth_flows SET consumed_at=now() WHERE kind='login_code' AND secret_hash=$1 AND consumed_at IS NULL AND expires_at>now() RETURNING user_id`, Hash(code)).Scan(&userID)
	if err != nil {
		return TokenPair{}, errors.New("invalid code")
	}
	family := uuid.NewString()
	return s.issue(ctx, userID, family, time.Now().Add(30*24*time.Hour))
}

func (s *Service) Refresh(ctx context.Context, raw string) (TokenPair, error) {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return TokenPair{}, err
	}
	defer tx.Rollback(ctx)
	var id, userID, family string
	var expiry time.Time
	var consumed, revoked *time.Time
	err = tx.QueryRow(ctx, `SELECT id,user_id,family_id,expires_at,consumed_at,revoked_at FROM sessions WHERE refresh_hash=$1 FOR UPDATE`, Hash(raw)).Scan(&id, &userID, &family, &expiry, &consumed, &revoked)
	if err != nil || revoked != nil || time.Now().After(expiry) {
		return TokenPair{}, errors.New("invalid refresh token")
	}
	if consumed != nil {
		_, _ = tx.Exec(ctx, `UPDATE sessions SET revoked_at=now() WHERE family_id=$1 AND revoked_at IS NULL`, family)
		_ = tx.Commit(ctx)
		return TokenPair{}, errors.New("refresh token reused")
	}
	_, err = tx.Exec(ctx, `UPDATE sessions SET consumed_at=now() WHERE id=$1`, id)
	if err != nil {
		return TokenPair{}, err
	}
	refresh, err := Random()
	if err != nil {
		return TokenPair{}, err
	}
	var newID string
	err = tx.QueryRow(ctx, `INSERT INTO sessions(user_id,family_id,refresh_hash,expires_at) VALUES($1,$2,$3,$4) RETURNING id`, userID, family, Hash(refresh), expiry).Scan(&newID)
	if err != nil {
		return TokenPair{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return TokenPair{}, err
	}
	access, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{"iss": "bca-backend", "aud": "bca-owner", "sub": userID, "sid": newID, "iat": time.Now().Unix(), "exp": time.Now().Add(15 * time.Minute).Unix()}).SignedString([]byte(s.Config.JWTSecret))
	return TokenPair{access, refresh, 900}, err
}

func (s *Service) Logout(ctx context.Context, p Principal) error {
	_, err := s.DB.Exec(ctx, `UPDATE sessions SET revoked_at=now() WHERE family_id=(SELECT family_id FROM sessions WHERE id=$1 AND user_id=$2)`, p.SessionID, p.UserID)
	return err
}

func (s *Service) Owner(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if raw == "" || raw == r.Header.Get("Authorization") {
			reject(w, r)
			return
		}
		claims := jwt.MapClaims{}
		token, err := jwt.NewParser(jwt.WithValidMethods([]string{"HS256"}), jwt.WithIssuer("bca-backend"), jwt.WithAudience("bca-owner"), jwt.WithExpirationRequired()).ParseWithClaims(raw, claims, func(t *jwt.Token) (any, error) { return []byte(s.Config.JWTSecret), nil })
		if err != nil || !token.Valid {
			reject(w, r)
			return
		}
		userID, ok1 := claims["sub"].(string)
		sessionID, ok2 := claims["sid"].(string)
		if !ok1 || !ok2 {
			reject(w, r)
			return
		}
		var active bool
		err = s.DB.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM sessions WHERE id=$1 AND user_id=$2 AND consumed_at IS NULL AND revoked_at IS NULL AND expires_at>now())`, sessionID, userID).Scan(&active)
		if err != nil || !active {
			reject(w, r)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, Principal{userID, sessionID})))
	})
}
func FromContext(ctx context.Context) Principal { p, _ := ctx.Value(ctxKey{}).(Principal); return p }

func (s *Service) Worker(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if len(raw) != len(s.Config.WorkerToken) || subtle.ConstantTimeCompare([]byte(raw), []byte(s.Config.WorkerToken)) != 1 {
			reject(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func reject(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(401)
	_ = json.NewEncoder(w).Encode(map[string]string{"code": "unauthorized", "request_id": middleware.GetReqID(r.Context())})
}
