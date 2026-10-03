package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"strings"
	"time"

	"example.com/bca/internal/auth"
	"example.com/bca/internal/parser/bca"
	"example.com/bca/internal/transaction"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func seed() error {
	raw := os.Getenv("DATABASE_URL")
	parsed, err := url.Parse(raw)
	if err != nil || !strings.HasSuffix(parsed.Path, "_test") || os.Getenv("BCA_TEST_DATABASE") != "1" {
		return errors.New("seed requires BCA_TEST_DATABASE=1 and a database name ending in _test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	db, err := pgxpool.New(ctx, raw)
	if err != nil {
		return err
	}
	defer db.Close()
	code, err := auth.Random()
	if err != nil {
		return err
	}
	var userID, integrationID string
	err = db.QueryRow(ctx, `INSERT INTO users(google_sub,email) VALUES('synthetic-test-owner','synthetic@example.invalid')
  ON CONFLICT(google_sub) DO UPDATE SET email=EXCLUDED.email RETURNING id`).Scan(&userID)
	if err != nil {
		return err
	}
	err = db.QueryRow(ctx, `INSERT INTO gmail_integrations(user_id,mailbox_id,status) VALUES($1,'synthetic-test-mailbox','connected')
  ON CONFLICT(user_id) DO UPDATE SET status='connected' RETURNING id`, userID).Scan(&integrationID)
	if err != nil {
		return err
	}
	_, err = db.Exec(ctx, `INSERT INTO gmail_sync_state(integration_id,coverage_start) VALUES($1,now()-interval '30 days') ON CONFLICT DO NOTHING`, integrationID)
	if err != nil {
		return err
	}
	_, err = db.Exec(ctx, `INSERT INTO auth_flows(kind,secret_hash,user_id,expires_at) VALUES('login_code',$1,$2,now()+interval '60 seconds')`, auth.Hash(code), userID)
	if err != nil {
		return err
	}
	ids := map[string]string{}
	events := map[string]string{}
	for _, kind := range []string{"bca_transfer", "interbank_transfer"} {
		messageID := "synthetic-" + uuid.NewString()
		var id string
		err = db.QueryRow(ctx, `INSERT INTO bca_email_jobs(integration_id,message_id,state) VALUES($1,$2,'processing') RETURNING id`, integrationID, messageID).Scan(&id)
		if err != nil {
			return err
		}
		receipt := bca.Receipt{ParserVersion: "bca-account-v1", BankReference: "SYN" + strings.ReplaceAll(uuid.NewString(), "-", "")[:16], ReceiptKind: kind, Status: "successful", Currency: "IDR", Amount: "90000.00", SourceAccountAlias: "2180xxxx19", BeneficiaryBank: "BCA", BeneficiaryAccountMasked: "******7890", TransactionDateLocal: "2026-10-02T11:20:40", Timezone: "Asia/Jakarta"}
		if kind == "interbank_transfer" {
			fee := "2500.00"
			receipt.Fee = &fee
			receipt.Amount = "3300000.00"
			receipt.ParserVersion = "bca-interbank-v1"
			receipt.BeneficiaryBank = "BANK MANDIRI"
			receipt.BeneficiaryAccountMasked = "******9066"
		}
		event := transaction.Event{SourceJobID: id, SourceMessageID: messageID, Receipt: receipt}
		payload, _ := json.Marshal(event)
		_, err = db.Exec(ctx, `UPDATE bca_email_jobs SET delivery_payload=$2,parser_version=$3 WHERE id=$1`, id, payload, receipt.ParserVersion)
		if err != nil {
			return err
		}
		ids[kind] = id
		events[kind] = string(payload)
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]string{"login_code": code, "account_job_id": ids["bca_transfer"], "interbank_job_id": ids["interbank_transfer"], "account_event": events["bca_transfer"], "interbank_event": events["interbank_transfer"], "expires_at": time.Now().Add(time.Minute).Format(time.RFC3339)})
}
