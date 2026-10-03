package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"example.com/bca/internal/auth"
	"example.com/bca/internal/config"
	"example.com/bca/internal/gmail"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestManualSyncOutcomeIsDurable(t *testing.T) {
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
	var userID, integrationID string
	sub := uuid.NewString()
	if err = db.QueryRow(ctx, "INSERT INTO users(google_sub,email) VALUES($1,'synthetic@example.invalid') RETURNING id", sub).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRow(ctx, "INSERT INTO gmail_integrations(user_id,mailbox_id,status) VALUES($1,$2,'connected') RETURNING id", userID, sub).Scan(&integrationID); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(ctx, "INSERT INTO gmail_sync_state(integration_id,coverage_start) VALUES($1,now()-interval '1 day')", integrationID); err != nil {
		t.Fatal(err)
	}
	code, err := auth.Random()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(ctx, "INSERT INTO auth_flows(kind,secret_hash,user_id,expires_at) VALUES('login_code',$1,$2,now()+interval '1 minute')", auth.Hash(code), userID); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{JWTSecret: "synthetic-jwt-key-with-at-least-32-chars"}
	identity := &auth.Service{DB: db, Config: cfg}
	pair, err := identity.Exchange(ctx, code, "")
	if err != nil {
		t.Fatal(err)
	}
	worker := gmail.NewWorker(db, identity, cfg)
	server := httptest.NewServer((API{DB: db, Auth: identity, Worker: worker, Config: cfg}).Router())
	defer server.Close()
	req, _ := http.NewRequest("POST", server.URL+"/api/v1/integrations/gmail/sync", nil)
	req.Header.Set("Authorization", "Bearer "+pair.AccessToken)
	response, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var scheduled struct {
		OperationID string `json:"operation_id"`
	}
	if err = json.NewDecoder(response.Body).Decode(&scheduled); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 202 || scheduled.OperationID == "" {
		t.Fatalf("status=%d operation=%s", response.StatusCode, scheduled.OperationID)
	}
	for i := 0; i < 30; i++ {
		request, _ := http.NewRequest("GET", server.URL+"/api/v1/integrations/gmail/sync/"+scheduled.OperationID, nil)
		request.Header.Set("Authorization", "Bearer "+pair.AccessToken)
		result, err := server.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		var detail struct {
			Status     string  `json:"status"`
			ReasonCode *string `json:"reason_code"`
		}
		if err = json.NewDecoder(result.Body).Decode(&detail); err != nil {
			t.Fatal(err)
		}
		result.Body.Close()
		if detail.Status == "failed" {
			if detail.ReasonCode == nil || *detail.ReasonCode != "discovery_failed" {
				t.Fatalf("%+v", detail)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("operation did not reach a durable outcome")
}
