package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	DatabaseURL, HTTPAddr, PublicURL, InternalIngestURL                   string
	GoogleClientID, GoogleClientSecret, GoogleRedirectURI, OwnerGoogleSub string
	WorkerToken, JWTSecret, AccountHMACKey                                string
	EncryptionKey                                                         []byte
	PollInterval                                                          time.Duration
	Sender, Timezone                                                      string
	RetentionDays                                                         int
	TrustGmailAuthResults                                                 bool
}

func Load() (Config, error) {
	c := Config{
		DatabaseURL: os.Getenv("DATABASE_URL"), HTTPAddr: os.Getenv("HTTP_ADDR"),
		PublicURL: os.Getenv("PUBLIC_URL"), InternalIngestURL: os.Getenv("INTERNAL_INGEST_URL"),
		GoogleClientID: os.Getenv("GOOGLE_CLIENT_ID"), GoogleClientSecret: os.Getenv("GOOGLE_CLIENT_SECRET"),
		GoogleRedirectURI: os.Getenv("GOOGLE_REDIRECT_URI"), OwnerGoogleSub: os.Getenv("OWNER_GOOGLE_SUB"),
		WorkerToken: os.Getenv("WORKER_INGEST_TOKEN"), JWTSecret: os.Getenv("JWT_SIGNING_SECRET"),
		AccountHMACKey: os.Getenv("ACCOUNT_HMAC_KEY"), Sender: os.Getenv("BCA_SENDER"),
		Timezone: os.Getenv("IMPORT_TIMEZONE"),
	}
	for name, value := range map[string]string{
		"DATABASE_URL": c.DatabaseURL, "HTTP_ADDR": c.HTTPAddr, "PUBLIC_URL": c.PublicURL,
		"INTERNAL_INGEST_URL": c.InternalIngestURL, "GOOGLE_CLIENT_ID": c.GoogleClientID,
		"GOOGLE_CLIENT_SECRET": c.GoogleClientSecret, "GOOGLE_REDIRECT_URI": c.GoogleRedirectURI,
		"OWNER_GOOGLE_SUB": c.OwnerGoogleSub, "WORKER_INGEST_TOKEN": c.WorkerToken,
		"JWT_SIGNING_SECRET": c.JWTSecret, "ACCOUNT_HMAC_KEY": c.AccountHMACKey,
	} {
		if value == "" {
			return c, fmt.Errorf("%s is required", name)
		}
	}
	for name, raw := range map[string]string{"PUBLIC_URL": c.PublicURL, "INTERNAL_INGEST_URL": c.InternalIngestURL, "GOOGLE_REDIRECT_URI": c.GoogleRedirectURI} {
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" || (u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1" || u.Hostname() == "service"))) {
			return c, fmt.Errorf("%s must be an HTTPS URL or a local/private service HTTP URL", name)
		}
	}
	if c.WorkerToken == c.JWTSecret || c.WorkerToken == c.AccountHMACKey || c.JWTSecret == c.AccountHMACKey {
		return c, errors.New("security keys must be distinct")
	}
	if len(c.WorkerToken) < 32 || len(c.JWTSecret) < 32 || len(c.AccountHMACKey) < 32 {
		return c, errors.New("security keys must each contain at least 32 characters")
	}
	if c.Sender != "bca@bca.co.id" {
		return c, errors.New("BCA_SENDER must equal bca@bca.co.id")
	}
	if c.Timezone != "Asia/Jakarta" {
		return c, errors.New("IMPORT_TIMEZONE must equal Asia/Jakarta")
	}
	key, err := base64.StdEncoding.DecodeString(os.Getenv("TOKEN_ENCRYPTION_KEY"))
	if err != nil || len(key) != 32 {
		return c, errors.New("TOKEN_ENCRYPTION_KEY must be base64 encoded 32 bytes")
	}
	c.EncryptionKey = key
	c.PollInterval = time.Minute
	if raw := os.Getenv("POLL_INTERVAL"); raw != "" {
		c.PollInterval, err = time.ParseDuration(raw)
		if err != nil || c.PollInterval < 10*time.Second {
			return c, errors.New("POLL_INTERVAL must be at least 10s")
		}
	}
	switch os.Getenv("TRUST_GMAIL_AUTH_RESULTS") {
	case "true":
		c.TrustGmailAuthResults = true
	case "false", "":
		c.TrustGmailAuthResults = false
	default:
		return c, errors.New("TRUST_GMAIL_AUTH_RESULTS must be true or false")
	}
	c.RetentionDays = 30
	if raw := os.Getenv("SOURCE_RETENTION_DAYS"); raw != "" {
		c.RetentionDays, err = strconv.Atoi(raw)
		if err != nil || c.RetentionDays < 1 || c.RetentionDays > 365 {
			return c, errors.New("SOURCE_RETENTION_DAYS must be 1..365")
		}
	}
	if strings.TrimRight(c.GoogleRedirectURI, "/") != strings.TrimRight(c.PublicURL, "/")+"/auth/google/callback" {
		return c, errors.New("GOOGLE_REDIRECT_URI must match PUBLIC_URL/auth/google/callback")
	}
	return c, nil
}
