package gmail

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	gmailapi "google.golang.org/api/gmail/v1"
	"google.golang.org/api/option"
)

func TestDiscoveryKeepsOtherMailPrivate(t *testing.T) {
	raw := os.Getenv("TEST_DATABASE_URL")
	if raw == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	db, err := pgxpool.New(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	sub := uuid.NewString()
	var userID, integrationID string
	if err = db.QueryRow(ctx, "INSERT INTO users(google_sub,email) VALUES($1,'test@example.invalid') RETURNING id", sub).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRow(ctx, "INSERT INTO gmail_integrations(user_id,mailbox_id,status) VALUES($1,$2,'connected') RETURNING id", userID, sub).Scan(&integrationID); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(ctx, "INSERT INTO gmail_sync_state(integration_id,coverage_start) VALUES($1,$2)", integrationID, time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	fullCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/messages"):
			if !strings.Contains(r.URL.Query().Get("q"), "from:bca@bca.co.id") || r.URL.Query().Get("fields") != "messages/id,nextPageToken" {
				t.Errorf("unsafe list: %s", r.URL.String())
			}
			fmt.Fprint(w, `{"messages":[{"id":"other"},{"id":"bca1"}]}`)
		case strings.HasSuffix(r.URL.Path, "/history"):
			fmt.Fprint(w, `{"history":[{"messagesAdded":[{"message":{"id":"bca2"}}]}],"historyId":"101"}`)
		case strings.Contains(r.URL.Path, "/messages/"):
			if r.URL.Query().Get("format") != "metadata" {
				fullCalls++
				t.Errorf("full message fetched: %s", r.URL.String())
			}
			from := "fake@example.com"
			if !strings.HasSuffix(r.URL.Path, "/other") {
				from = "BCA <bca@bca.co.id>"
			}
			fmt.Fprintf(w, `{"id":"test","payload":{"headers":[{"name":"From","value":%q}]}}`, from)
		default:
			t.Errorf("unexpected request: %s", r.URL.String())
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	api, err := gmailapi.NewService(ctx, option.WithEndpoint(server.URL+"/"), option.WithHTTPClient(server.Client()))
	if err != nil {
		t.Fatal(err)
	}
	worker := Worker{DB: db}
	generation, _, start, err := worker.lease(ctx, integrationID)
	if err != nil {
		t.Fatal(err)
	}
	if err = worker.scan(ctx, &Provider{API: api}, integrationID, generation, start); err != nil {
		t.Fatal(err)
	}
	terminal, err := worker.replay(ctx, &Provider{API: api}, integrationID, generation, "100")
	if err != nil || terminal != "101" {
		t.Fatalf("checkpoint %s %v", terminal, err)
	}
	var count int
	if err = db.QueryRow(ctx, "SELECT count(*) FROM bca_email_jobs WHERE integration_id=$1", integrationID).Scan(&count); err != nil || count != 2 || fullCalls != 0 {
		t.Fatalf("jobs=%d full=%d err=%v", count, fullCalls, err)
	}
}

func TestExpiredHistoryRecovery(t *testing.T) {
	raw := os.Getenv("TEST_DATABASE_URL")
	if raw == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	db, err := pgxpool.New(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	sub := uuid.NewString()
	var userID, integrationID string
	if err = db.QueryRow(ctx, "INSERT INTO users(google_sub,email) VALUES($1,'test@example.invalid') RETURNING id", sub).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRow(ctx, "INSERT INTO gmail_integrations(user_id,mailbox_id,status) VALUES($1,$2,'connected') RETURNING id", userID, sub).Scan(&integrationID); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(ctx, "INSERT INTO gmail_sync_state(integration_id,history_id,coverage_start) VALUES($1,'50',$2)", integrationID, time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/profile"):
			fmt.Fprint(w, `{"historyId":"100"}`)
		case strings.HasSuffix(r.URL.Path, "/history"):
			if r.URL.Query().Get("startHistoryId") == "50" {
				w.WriteHeader(404)
				fmt.Fprint(w, `{"error":{"code":404,"message":"history expired"}}`)
				return
			}
			fmt.Fprint(w, `{"history":[{"messagesAdded":[{"message":{"id":"replayed"}}]}],"historyId":"101"}`)
		case strings.HasSuffix(r.URL.Path, "/messages"):
			fmt.Fprint(w, `{"messages":[{"id":"scanned"}]}`)
		case strings.Contains(r.URL.Path, "/messages/"):
			if r.URL.Query().Get("format") != "metadata" {
				t.Error("body fetched during recovery")
			}
			fmt.Fprint(w, `{"payload":{"headers":[{"name":"From","value":"bca@bca.co.id"}]}}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	api, err := gmailapi.NewService(ctx, option.WithEndpoint(server.URL+"/"), option.WithHTTPClient(server.Client()))
	if err != nil {
		t.Fatal(err)
	}
	worker := Worker{DB: db, providerFactory: func(context.Context, string) (*Provider, error) { return &Provider{API: api}, nil }}
	if err = worker.Discover(ctx, integrationID); err != nil {
		t.Fatal(err)
	}
	var history, status string
	var count int
	if err = db.QueryRow(ctx, "SELECT history_id,recovery_status FROM gmail_sync_state WHERE integration_id=$1", integrationID).Scan(&history, &status); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRow(ctx, "SELECT count(*) FROM bca_email_jobs WHERE integration_id=$1", integrationID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if history != "101" || status != "complete" || count != 2 {
		t.Fatalf("history=%s status=%s jobs=%d", history, status, count)
	}

	if _, err = db.Exec(ctx, "UPDATE gmail_integrations SET status='disconnected' WHERE id=$1", integrationID); err != nil {
		t.Fatal(err)
	}
	if err = worker.Discover(ctx, integrationID); !errors.Is(err, ErrBusy) {
		t.Fatalf("disconnect allowed discovery: %v", err)
	}
	if err = worker.candidate(ctx, &Provider{API: api}, integrationID, "late"); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRow(ctx, "SELECT count(*) FROM bca_email_jobs WHERE integration_id=$1", integrationID).Scan(&count); err != nil || count != 2 {
		t.Fatalf("jobs after disconnect=%d err=%v", count, err)
	}
}
