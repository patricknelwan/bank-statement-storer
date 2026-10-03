package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"example.com/bca/internal/auth"
	"example.com/bca/internal/config"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestMobileImportAndJakartaDateContract(t *testing.T) {
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
	var userID, integrationID, jobID, transactionID string
	if err := db.QueryRow(ctx, `INSERT INTO users(google_sub,email) VALUES($1,'synthetic@example.invalid') RETURNING id`, sub).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, `INSERT INTO gmail_integrations(user_id,mailbox_id,status) VALUES($1,$2,'connected') RETURNING id`, userID, sub).Scan(&integrationID); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, `INSERT INTO bca_email_jobs(integration_id,message_id,state) VALUES($1,$2,'completed') RETURNING id`, integrationID, uuid.NewString()).Scan(&jobID); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, `INSERT INTO transactions(user_id,integration_id,bank_reference,receipt_kind,classification,occurred_at,source_local_time,source_timezone,amount_minor,currency,source_account_alias,beneficiary_account_masked,parser_version)
		VALUES($1,$2,$3,'bca_transfer','unclassified','2026-10-02 17:30:00+00','2026-10-03T00:30:00','Asia/Jakarta',10000,'IDR','****1234','****5678','bca-account-v1') RETURNING id`, userID, integrationID, uuid.NewString()).Scan(&transactionID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO transaction_sources(transaction_id,job_id) VALUES($1,$2)`, transactionID, jobID); err != nil {
		t.Fatal(err)
	}
	code, err := auth.Random()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO auth_flows(kind,secret_hash,user_id,expires_at) VALUES('login_code',$1,$2,now()+interval '1 minute')`, auth.Hash(code), userID); err != nil {
		t.Fatal(err)
	}
	identity := &auth.Service{DB: db, Config: config.Config{JWTSecret: "synthetic-jwt-key-with-at-least-32-chars"}}
	pair, err := identity.Exchange(ctx, code, "")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer((API{DB: db, Auth: identity}).Router())
	defer server.Close()
	get := func(path string, target any) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, server.URL+path, nil)
		req.Header.Set("Authorization", "Bearer "+pair.AccessToken)
		resp, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("%s: status %d", path, resp.StatusCode)
		}
		if err := json.NewDecoder(resp.Body).Decode(target); err != nil {
			t.Fatal(err)
		}
	}
	var jobs struct {
		Items []map[string]any `json:"items"`
	}
	get("/api/v1/imports?state=completed", &jobs)
	found := false
	for _, job := range jobs.Items {
		if job["id"] == jobID {
			found = job["state"] == "completed" && job["transaction_id"] == transactionID && job["ID"] == nil && job["State"] == nil
		}
	}
	if !found {
		t.Fatal("import list did not expose the normalized keys and linked transaction")
	}
	var detail map[string]any
	get("/api/v1/imports/"+jobID, &detail)
	if detail["transaction_id"] != transactionID {
		t.Fatal("import detail did not expose linked transaction")
	}
	var page struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	get("/api/v1/transactions?from=2026-10-03&to=2026-10-03", &page)
	found = false
	for _, item := range page.Items {
		found = found || item.ID == transactionID
	}
	if !found {
		t.Fatal("Jakarta Oct 3 transaction missing from Oct 3 filter")
	}
	get("/api/v1/transactions?to=2026-10-02", &page)
	for _, item := range page.Items {
		if item.ID == transactionID {
			t.Fatal("Jakarta Oct 3 transaction included in Oct 2 filter")
		}
	}
}
