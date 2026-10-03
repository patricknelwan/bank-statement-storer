package transaction

import (
	"context"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"time"

	"example.com/bca/internal/parser/bca"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Event struct {
	SourceJobID     string `json:"source_job_id"`
	SourceMessageID string `json:"source_message_id"`
	bca.Receipt
	BeneficiaryMatchToken string `json:"beneficiary_match_token,omitempty"`
}
type Result struct {
	ID      string `json:"id"`
	Created bool   `json:"created"`
}
type Error struct {
	Code   string
	Status int
}

func (e Error) Error() string { return e.Code }

var reference = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9/_-]{3,79}$`)

func Validate(e Event) (int64, *int64, time.Time, error) {
	if _, err := uuid.Parse(e.SourceJobID); err != nil {
		return 0, nil, time.Time{}, Error{"invalid_event", 422}
	}
	if e.SourceMessageID == "" || len(e.SourceMessageID) > 255 || !reference.MatchString(e.BankReference) ||
		(e.ReceiptKind != "bca_transfer" && e.ReceiptKind != "interbank_transfer") ||
		e.Status != "successful" || e.Currency != "IDR" || e.Timezone != "Asia/Jakarta" ||
		e.SourceAccountAlias == "" || len(e.SourceAccountAlias) > 80 || !strings.Contains(e.BeneficiaryAccountMasked, "*") || e.BeneficiaryAccount != "" ||
		(e.ReceiptKind == "bca_transfer" && e.ParserVersion != "bca-account-v1") || (e.ReceiptKind == "interbank_transfer" && e.ParserVersion != "bca-interbank-v1") {
		return 0, nil, time.Time{}, Error{"invalid_event", 422}
	}
	amount, err := bca.Money(e.Amount)
	if err != nil || amount <= 0 || fmt.Sprintf("%d.%02d", amount/100, amount%100) != e.Amount {
		return 0, nil, time.Time{}, Error{"invalid_amount", 422}
	}
	var fee *int64
	if e.Fee != nil {
		n, err := bca.Money(*e.Fee)
		if err != nil || fmt.Sprintf("%d.%02d", n/100, n%100) != *e.Fee {
			return 0, nil, time.Time{}, Error{"invalid_fee", 422}
		}
		fee = &n
	}
	local, err := time.Parse("2006-01-02T15:04:05", e.TransactionDateLocal)
	if err != nil {
		return 0, nil, time.Time{}, Error{"invalid_date", 422}
	}
	loc, _ := time.LoadLocation("Asia/Jakarta")
	occurred := time.Date(local.Year(), local.Month(), local.Day(), local.Hour(), local.Minute(), local.Second(), 0, loc).UTC()
	if e.BeneficiaryMatchToken != "" {
		b, err := hex.DecodeString(e.BeneficiaryMatchToken)
		if err != nil || len(b) != 32 {
			return 0, nil, time.Time{}, Error{"invalid_match_token", 422}
		}
	}
	return amount, fee, occurred, nil
}

type Service struct{ DB *pgxpool.Pool }

func (s Service) Ingest(ctx context.Context, e Event) (Result, error) {
	amount, fee, occurred, err := Validate(e)
	if err != nil {
		return Result{}, err
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return Result{}, err
	}
	defer tx.Rollback(ctx)
	var userID, integrationID, messageID, state, status string
	var persisted []byte
	err = tx.QueryRow(ctx, `SELECT i.user_id,j.integration_id,j.message_id,j.state,i.status,j.delivery_payload
		FROM bca_email_jobs j JOIN gmail_integrations i ON i.id=j.integration_id WHERE j.id=$1 FOR UPDATE OF j`, e.SourceJobID).Scan(&userID, &integrationID, &messageID, &state, &status, &persisted)
	if errors.Is(err, pgx.ErrNoRows) {
		return Result{}, Error{"unknown_source_job", 422}
	}
	if err != nil {
		return Result{}, err
	}
	if status != "connected" {
		return Result{}, Error{"integration_disconnected", 409}
	}
	if messageID != e.SourceMessageID {
		return Result{}, Error{"source_mismatch", 409}
	}
	if persisted == nil {
		return Result{}, Error{"payload_not_persisted", 409}
	}
	var saved Event
	if json.Unmarshal(persisted, &saved) != nil || !reflect.DeepEqual(saved, e) {
		return Result{}, Error{"source_mismatch", 409}
	}
	var linkedID string
	err = tx.QueryRow(ctx, `SELECT transaction_id FROM transaction_sources WHERE job_id=$1`, e.SourceJobID).Scan(&linkedID)
	if err == nil {
		if err = tx.Commit(ctx); err != nil {
			return Result{}, err
		}
		return Result{linkedID, false}, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Result{}, err
	}
	if state != "processing" && state != "retry_wait" {
		return Result{}, Error{"invalid_job_state", 409}
	}
	classification := "unclassified"
	var match []byte
	if e.BeneficiaryMatchToken != "" {
		match, _ = hex.DecodeString(e.BeneficiaryMatchToken)
		var owned bool
		err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM owned_accounts WHERE user_id=$1 AND bank=$2 AND match_token=$3)`, userID, strings.ToUpper(e.BeneficiaryBank), match).Scan(&owned)
		if err != nil {
			return Result{}, err
		}
		if owned {
			classification = "internal_transfer"
		}
	}
	var id string
	err = tx.QueryRow(ctx, `INSERT INTO transactions(user_id,integration_id,bank_reference,receipt_kind,classification,occurred_at,source_local_time,source_timezone,amount_minor,fee_minor,currency,source_account_alias,beneficiary_bank,beneficiary_account_masked,beneficiary_match_token,parser_version)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,'IDR',$11,$12,$13,$14,$15)
		ON CONFLICT(user_id,bank_reference,source_account_alias,receipt_kind) DO NOTHING RETURNING id`,
		userID, integrationID, e.BankReference, e.ReceiptKind, classification, occurred, e.TransactionDateLocal, e.Timezone, amount, fee, e.SourceAccountAlias, e.BeneficiaryBank, e.BeneficiaryAccountMasked, match, e.ParserVersion).Scan(&id)
	created := err == nil
	if !created {
		if !errors.Is(err, pgx.ErrNoRows) {
			return Result{}, err
		}
		var oldAmount int64
		var oldFee *int64
		var oldOccurred time.Time
		var oldMasked, oldBank string
		var oldMatch []byte
		err = tx.QueryRow(ctx, `SELECT id,amount_minor,fee_minor,occurred_at,beneficiary_account_masked,coalesce(beneficiary_bank,''),beneficiary_match_token FROM transactions WHERE user_id=$1 AND bank_reference=$2 AND source_account_alias=$3 AND receipt_kind=$4`, userID, e.BankReference, e.SourceAccountAlias, e.ReceiptKind).Scan(&id, &oldAmount, &oldFee, &oldOccurred, &oldMasked, &oldBank, &oldMatch)
		if err != nil {
			return Result{}, err
		}
		if oldAmount != amount || !equalFee(oldFee, fee) || !oldOccurred.Equal(occurred) || oldMasked != e.BeneficiaryAccountMasked || oldBank != e.BeneficiaryBank || subtle.ConstantTimeCompare(oldMatch, match) != 1 {
			_, _ = tx.Exec(ctx, `UPDATE bca_email_jobs SET state='needs_review',reason_code='conflicting_reference',updated_at=now() WHERE id=$1`, e.SourceJobID)
			if err = tx.Commit(ctx); err != nil {
				return Result{}, err
			}
			return Result{}, Error{"conflicting_reference", 409}
		}
	}
	_, err = tx.Exec(ctx, `INSERT INTO transaction_sources(transaction_id,job_id) VALUES($1,$2)`, id, e.SourceJobID)
	if err != nil {
		return Result{}, err
	}
	_, err = tx.Exec(ctx, `UPDATE bca_email_jobs SET state='completed',lease_until=NULL,reason_code=NULL,updated_at=now() WHERE id=$1`, e.SourceJobID)
	if err != nil {
		return Result{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return Result{}, err
	}
	return Result{id, created}, nil
}
func equalFee(a, b *int64) bool { return (a == nil && b == nil) || (a != nil && b != nil && *a == *b) }
