package httpapi

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"example.com/bca/internal/auth"
	"example.com/bca/internal/config"
	"example.com/bca/internal/database"
	"example.com/bca/internal/gmail"
	"example.com/bca/internal/transaction"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"golang.org/x/time/rate"
)

type API struct {
	DB     *pgxpool.Pool
	Auth   *auth.Service
	Worker *gmail.Worker
	Config config.Config
}
type apiError struct {
	Code      string `json:"code"`
	RequestID string `json:"request_id"`
}

func fail(w http.ResponseWriter, r *http.Request, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(apiError{code, middleware.GetReqID(r.Context())})
}
func jsonOut(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func decode(r *http.Request, v any) error {
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		return errors.New("content type")
	}
	defer r.Body.Close()
	body, err := io.ReadAll(io.LimitReader(r.Body, (1<<20)+1))
	if err != nil || len(body) > 1<<20 {
		return errors.New("body too large")
	}
	d := json.NewDecoder(bytes.NewReader(body))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return err
	}
	var extra any
	if err := d.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON or body too large")
	}
	return nil
}
func (a API) Router() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID, middleware.Recoverer)
	r.Get("/health/live", func(w http.ResponseWriter, r *http.Request) { jsonOut(w, 200, map[string]string{"status": "live"}) })
	r.Get("/health/ready", func(w http.ResponseWriter, r *http.Request) {
		ready, err := database.New(a.DB).DatabaseReady(r.Context())
		if err != nil || !ready {
			fail(w, r, 503, "database_not_ready")
			return
		}
		jsonOut(w, 200, map[string]string{"status": "ready"})
	})
	registry, latency := metrics(a.DB)
	r.Handle("/metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))
	authRate := rate.NewLimiter(rate.Every(time.Second), 10)
	controlRate := rate.NewLimiter(rate.Every(10*time.Second), 2)
	r.With(throttle(authRate)).Get("/auth/google/start", a.Auth.Start)
	r.Get("/auth/google/callback", a.Auth.Callback)
	r.With(throttle(authRate)).Post("/api/v1/auth/exchange", a.exchange)
	r.With(throttle(authRate)).Post("/api/v1/auth/refresh", a.refresh)
	r.With(a.Auth.Worker).Post("/api/v1/webhooks/bca", a.ingest)
	r.Group(func(r chi.Router) {
		r.Use(a.Auth.Owner)
		r.Post("/api/v1/auth/logout", a.logout)
		r.Get("/api/v1/me", a.me)
		r.Get("/api/v1/integrations/gmail", a.integration)
		r.Delete("/api/v1/integrations/gmail", a.disconnect)
		r.With(throttle(controlRate)).Post("/api/v1/integrations/gmail/sync", a.sync)
		r.Get("/api/v1/integrations/gmail/sync/{id}", a.syncOperation)
		r.Get("/api/v1/imports", a.imports)
		r.Get("/api/v1/imports/{id}", a.importDetail)
		r.With(throttle(controlRate)).Get("/api/v1/imports/{id}/source", a.importSource)
		r.With(throttle(controlRate)).Get("/api/v1/imports/{id}/inspect", a.inspectImport)
		r.With(throttle(controlRate)).Post("/api/v1/imports/{id}/retry", a.retry)
		r.Get("/api/v1/transactions", a.transactions)
		r.Get("/api/v1/transactions/{id}", a.transactionDetail)
		r.Patch("/api/v1/transactions/{id}", a.patchTransaction)
		r.Get("/api/v1/owned-accounts", a.ownedAccounts)
		r.Post("/api/v1/owned-accounts", a.addOwnedAccount)
	})
	return promhttp.InstrumentHandlerDuration(latency, r)
}
func (a API) exchange(w http.ResponseWriter, r *http.Request) {
	var q struct {
		Code         string `json:"code"`
		CodeVerifier string `json:"code_verifier"`
	}
	if decode(r, &q) != nil || q.Code == "" {
		fail(w, r, 422, "invalid_code")
		return
	}
	pair, err := a.Auth.Exchange(r.Context(), q.Code, q.CodeVerifier)
	if err != nil {
		fail(w, r, 401, "invalid_code")
		return
	}
	jsonOut(w, 200, pair)
}
func (a API) refresh(w http.ResponseWriter, r *http.Request) {
	var q struct {
		RefreshToken string `json:"refresh_token"`
	}
	if decode(r, &q) != nil || q.RefreshToken == "" {
		fail(w, r, 422, "invalid_refresh_token")
		return
	}
	pair, err := a.Auth.Refresh(r.Context(), q.RefreshToken)
	if err != nil {
		fail(w, r, 401, "invalid_refresh_token")
		return
	}
	jsonOut(w, 200, pair)
}
func (a API) logout(w http.ResponseWriter, r *http.Request) {
	if a.Auth.Logout(r.Context(), auth.FromContext(r.Context())) != nil {
		fail(w, r, 503, "unavailable")
		return
	}
	w.WriteHeader(204)
}
func (a API) me(w http.ResponseWriter, r *http.Request) {
	var id, email, tz string
	err := a.DB.QueryRow(r.Context(), `SELECT id,email,timezone FROM users WHERE id=$1`, auth.FromContext(r.Context()).UserID).Scan(&id, &email, &tz)
	if err != nil {
		fail(w, r, 503, "unavailable")
		return
	}
	jsonOut(w, 200, map[string]string{"id": id, "email": email, "timezone": tz})
}
func (a API) integration(w http.ResponseWriter, r *http.Request) {
	var status, recovery string
	var last *time.Time
	err := a.DB.QueryRow(r.Context(), `SELECT i.status,s.recovery_status,s.last_success FROM gmail_integrations i JOIN gmail_sync_state s ON s.integration_id=i.id WHERE i.user_id=$1`, auth.FromContext(r.Context()).UserID).Scan(&status, &recovery, &last)
	if errors.Is(err, pgx.ErrNoRows) {
		jsonOut(w, 200, map[string]any{"status": "not_connected"})
		return
	}
	if err != nil {
		fail(w, r, 503, "unavailable")
		return
	}
	var operation any
	var opID, opStatus string
	opErr := a.DB.QueryRow(r.Context(), `SELECT id,status FROM gmail_sync_operations WHERE user_id=$1 ORDER BY created_at DESC LIMIT 1`, auth.FromContext(r.Context()).UserID).Scan(&opID, &opStatus)
	if opErr == nil {
		operation = map[string]string{"id": opID, "status": opStatus}
	} else if !errors.Is(opErr, pgx.ErrNoRows) {
		fail(w, r, 503, "unavailable")
		return
	}
	jsonOut(w, 200, map[string]any{"status": status, "recovery_status": recovery, "last_success": last, "last_operation": operation})
}
func (a API) disconnect(w http.ResponseWriter, r *http.Request) {
	tag, err := a.DB.Exec(r.Context(), `UPDATE gmail_integrations SET status='disconnected',encrypted_refresh_token=NULL WHERE user_id=$1`, auth.FromContext(r.Context()).UserID)
	if err != nil {
		fail(w, r, 503, "unavailable")
		return
	}
	if tag.RowsAffected() == 0 {
		fail(w, r, 404, "integration_not_found")
		return
	}
	w.WriteHeader(204)
}
func (a API) sync(w http.ResponseWriter, r *http.Request) {
	userID := auth.FromContext(r.Context()).UserID
	var integrationID string
	err := a.DB.QueryRow(r.Context(), `SELECT id FROM gmail_integrations WHERE user_id=$1 AND status='connected'`, userID).Scan(&integrationID)
	if err != nil {
		fail(w, r, 409, "integration_unavailable")
		return
	}
	var op string
	err = a.DB.QueryRow(r.Context(), `INSERT INTO gmail_sync_operations(user_id,integration_id,status) SELECT $1,$2,'queued' WHERE NOT EXISTS(SELECT 1 FROM gmail_sync_operations WHERE integration_id=$2 AND status IN ('queued','running')) RETURNING id`, userID, integrationID).Scan(&op)
	if err != nil {
		fail(w, r, 409, "sync_already_running")
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		_, _ = a.DB.Exec(ctx, `UPDATE gmail_sync_operations SET status='running' WHERE id=$1 AND status='queued'`, op)
		err := a.Worker.Discover(ctx, integrationID)
		status, reason := "completed", ""
		if errors.Is(err, gmail.ErrBusy) {
			status, reason = "failed", "sync_busy"
		} else if err != nil {
			status, reason = "failed", "discovery_failed"
		}
		finish, stop := context.WithTimeout(context.Background(), 2*time.Second)
		defer stop()
		_, _ = a.DB.Exec(finish, `UPDATE gmail_sync_operations SET status=$2,reason_code=NULLIF($3,''),finished_at=now() WHERE id=$1`, op, status, reason)
	}()
	jsonOut(w, 202, map[string]string{"operation_id": op, "status": "queued"})
}
func (a API) syncOperation(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var status string
	var reason *string
	var created time.Time
	var finished *time.Time
	err := a.DB.QueryRow(r.Context(), `SELECT status,reason_code,created_at,finished_at FROM gmail_sync_operations WHERE id=$1 AND user_id=$2`, id, auth.FromContext(r.Context()).UserID).Scan(&status, &reason, &created, &finished)
	if errors.Is(err, pgx.ErrNoRows) {
		fail(w, r, 404, "operation_not_found")
		return
	}
	if err != nil {
		fail(w, r, 503, "unavailable")
		return
	}
	jsonOut(w, 200, map[string]any{"id": id, "status": status, "reason_code": reason, "created_at": created, "finished_at": finished})
}
func (a API) ingest(w http.ResponseWriter, r *http.Request) {
	var event transaction.Event
	if decode(r, &event) != nil {
		fail(w, r, 422, "invalid_json")
		return
	}
	result, err := (transaction.Service{DB: a.DB}).Ingest(r.Context(), event)
	if err != nil {
		var typed transaction.Error
		if errors.As(err, &typed) {
			fail(w, r, typed.Status, typed.Code)
			return
		}
		fail(w, r, 503, "unavailable")
		return
	}
	status := 200
	if result.Created {
		status = 201
	}
	jsonOut(w, status, result)
}

type cursor struct {
	At time.Time `json:"at"`
	ID string    `json:"id"`
}

func page(r *http.Request) (int, cursor, error) {
	limit := 50
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 100 {
			return 0, cursor{}, errors.New("invalid limit")
		}
		limit = n
	}
	var c cursor
	if raw := r.URL.Query().Get("cursor"); raw != "" {
		b, err := base64.RawURLEncoding.DecodeString(raw)
		if err != nil || json.Unmarshal(b, &c) != nil || c.ID == "" || c.At.IsZero() {
			return 0, cursor{}, errors.New("invalid cursor")
		}
	}
	return limit, c, nil
}
func nextCursor(at time.Time, id string) string {
	b, _ := json.Marshal(cursor{at, id})
	return base64.RawURLEncoding.EncodeToString(b)
}
func (a API) imports(w http.ResponseWriter, r *http.Request) {
	limit, c, err := page(r)
	if err != nil {
		fail(w, r, 422, "invalid_page")
		return
	}
	state := r.URL.Query().Get("state")
	switch state {
	case "", "queued", "processing", "retry_wait", "completed", "ignored", "unsupported", "needs_review", "failed":
	default:
		fail(w, r, 422, "invalid_state")
		return
	}
	rows, err := a.DB.Query(r.Context(), `SELECT j.id,j.state,j.reason_code,j.attempts,j.created_at,ts.transaction_id
		FROM bca_email_jobs j JOIN gmail_integrations i ON i.id=j.integration_id
		LEFT JOIN transaction_sources ts ON ts.job_id=j.id
		WHERE i.user_id=$1 AND ($2='' OR j.state=$2) AND ($3::timestamptz IS NULL OR (j.created_at,j.id)<($3,$4::uuid))
		ORDER BY j.created_at DESC,j.id DESC LIMIT $5`, auth.FromContext(r.Context()).UserID, state, nullableTime(c.At), nullableID(c.ID), limit+1)
	if err != nil {
		fail(w, r, 503, "unavailable")
		return
	}
	defer rows.Close()
	type item struct {
		ID            string    `json:"id"`
		State         string    `json:"state"`
		ReasonCode    *string   `json:"reason_code"`
		Attempts      int       `json:"attempts"`
		CreatedAt     time.Time `json:"created_at"`
		TransactionID *string   `json:"transaction_id"`
	}
	items := []item{}
	next := ""
	for rows.Next() {
		var x item
		if rows.Scan(&x.ID, &x.State, &x.ReasonCode, &x.Attempts, &x.CreatedAt, &x.TransactionID) != nil {
			fail(w, r, 503, "unavailable")
			return
		}
		if len(items) == limit {
			next = nextCursor(items[len(items)-1].CreatedAt, items[len(items)-1].ID)
			break
		}
		items = append(items, x)
	}
	jsonOut(w, 200, map[string]any{"items": items, "next_cursor": next})
}
func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}
func nullableTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}
func nullableID(id string) any {
	if id == "" {
		return nil
	}
	return id
}
func (a API) importDetail(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var state string
	var reason, transactionID *string
	var attempts int
	var created, updated time.Time
	err := a.DB.QueryRow(r.Context(), `SELECT j.state,j.reason_code,j.attempts,j.created_at,j.updated_at,ts.transaction_id FROM bca_email_jobs j JOIN gmail_integrations i ON i.id=j.integration_id LEFT JOIN transaction_sources ts ON ts.job_id=j.id WHERE j.id=$1 AND i.user_id=$2`, id, auth.FromContext(r.Context()).UserID).Scan(&state, &reason, &attempts, &created, &updated, &transactionID)
	if errors.Is(err, pgx.ErrNoRows) {
		fail(w, r, 404, "import_not_found")
		return
	}
	if err != nil {
		fail(w, r, 503, "unavailable")
		return
	}
	jsonOut(w, 200, map[string]any{"id": id, "state": state, "reason_code": reason, "attempts": attempts, "created_at": created, "updated_at": updated, "transaction_id": transactionID})
}

func (a API) importSource(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var integrationID, messageID string
	err := a.DB.QueryRow(r.Context(), `SELECT j.integration_id,j.message_id FROM bca_email_jobs j JOIN gmail_integrations i ON i.id=j.integration_id WHERE j.id=$1 AND i.user_id=$2`, id, auth.FromContext(r.Context()).UserID).Scan(&integrationID, &messageID)
	if errors.Is(err, pgx.ErrNoRows) {
		fail(w, r, 404, "import_not_found")
		return
	}
	if err != nil {
		fail(w, r, 503, "unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	source, err := a.Worker.FindSource(ctx, integrationID, messageID)
	if err != nil {
		fail(w, r, 503, "source_unavailable")
		return
	}
	jsonOut(w, 200, map[string]string{"id": id, "subject": source.Subject, "date": source.Date, "gmail_search": source.GmailSearch})
}

func (a API) inspectImport(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var integrationID, messageID, state string
	err := a.DB.QueryRow(r.Context(), `SELECT j.integration_id,j.message_id,j.state FROM bca_email_jobs j JOIN gmail_integrations i ON i.id=j.integration_id WHERE j.id=$1 AND i.user_id=$2`, id, auth.FromContext(r.Context()).UserID).Scan(&integrationID, &messageID, &state)
	if errors.Is(err, pgx.ErrNoRows) {
		fail(w, r, 404, "import_not_found")
		return
	}
	if err != nil {
		fail(w, r, 503, "unavailable")
		return
	}
	if state != "unsupported" {
		fail(w, r, 409, "import_not_unsupported")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	diagnostic, err := a.Worker.Inspect(ctx, integrationID, messageID)
	if err != nil {
		fail(w, r, 503, "inspection_unavailable")
		return
	}
	jsonOut(w, 200, map[string]any{"id": id, "job_state": state, "diagnostic": diagnostic})
}
func (a API) retry(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	tag, err := a.DB.Exec(r.Context(), `UPDATE bca_email_jobs j SET state='queued',attempts=0,next_attempt_at=now(),lease_until=NULL,reason_code=NULL,updated_at=now()
		FROM gmail_integrations i WHERE j.integration_id=i.id AND j.id=$1 AND i.user_id=$2 AND i.status='connected' AND j.state IN ('failed','needs_review','unsupported')`, id, auth.FromContext(r.Context()).UserID)
	if err != nil {
		fail(w, r, 503, "unavailable")
		return
	}
	if tag.RowsAffected() == 0 {
		fail(w, r, 409, "import_not_retryable")
		return
	}
	jsonOut(w, 202, map[string]string{"id": id, "state": "queued"})
}

type transactionDTO struct {
	ID                       string    `json:"id"`
	BankReference            string    `json:"bank_reference"`
	ReceiptKind              string    `json:"receipt_kind"`
	Classification           string    `json:"classification"`
	OccurredAt               time.Time `json:"occurred_at"`
	Amount                   string    `json:"amount"`
	Fee                      *string   `json:"fee"`
	Currency                 string    `json:"currency"`
	SourceAccountAlias       string    `json:"source_account_alias"`
	BeneficiaryBank          string    `json:"beneficiary_bank"`
	BeneficiaryAccountMasked string    `json:"beneficiary_account_masked"`
	PaymentTo                string    `json:"payment_to,omitempty"`
	Note                     string    `json:"note"`
	Version                  int       `json:"version"`
	CreatedAt                time.Time `json:"created_at"`
}

func scanTransaction(row pgx.Row) (transactionDTO, error) {
	var t transactionDTO
	var amount int64
	var fee *int64
	err := row.Scan(&t.ID, &t.BankReference, &t.ReceiptKind, &t.Classification, &t.OccurredAt, &amount, &fee, &t.Currency, &t.SourceAccountAlias, &t.BeneficiaryBank, &t.BeneficiaryAccountMasked, &t.PaymentTo, &t.Note, &t.Version, &t.CreatedAt)
	if err != nil {
		return t, err
	}
	t.Amount = fmt.Sprintf("%d.%02d", amount/100, amount%100)
	if fee != nil {
		v := fmt.Sprintf("%d.%02d", *fee/100, *fee%100)
		t.Fee = &v
	}
	return t, nil
}

const transactionColumns = `t.id,t.bank_reference,t.receipt_kind,t.classification,t.occurred_at,t.amount_minor,t.fee_minor,t.currency,t.source_account_alias,coalesce(t.beneficiary_bank,''),t.beneficiary_account_masked,t.payment_to,t.note,t.version,t.created_at`

func (a API) transactions(w http.ResponseWriter, r *http.Request) {
	limit, c, err := page(r)
	if err != nil {
		fail(w, r, 422, "invalid_page")
		return
	}
	classification := r.URL.Query().Get("classification")
	if classification != "" && classification != "unclassified" && classification != "expense" && classification != "internal_transfer" {
		fail(w, r, 422, "invalid_filter")
		return
	}
	from, to := r.URL.Query().Get("from"), r.URL.Query().Get("to")
	for _, v := range []string{from, to} {
		if v != "" {
			if _, err := time.Parse("2006-01-02", v); err != nil {
				fail(w, r, 422, "invalid_filter")
				return
			}
		}
	}
	rows, err := a.DB.Query(r.Context(), `SELECT `+transactionColumns+` FROM transactions t WHERE t.user_id=$1
		AND ($2='' OR t.classification=$2) AND ($3::date IS NULL OR t.occurred_at >= ($3::date::timestamp AT TIME ZONE 'Asia/Jakarta'))
		AND ($4::date IS NULL OR t.occurred_at < (($4::date+1)::timestamp AT TIME ZONE 'Asia/Jakarta'))
		AND ($5='' OR t.beneficiary_bank=$5) AND ($6='' OR t.source_account_alias=$6)
		AND ($7::timestamptz IS NULL OR (t.created_at,t.id)<($7,$8::uuid))
		ORDER BY t.created_at DESC,t.id DESC LIMIT $9`,
		auth.FromContext(r.Context()).UserID, classification, nullableString(from), nullableString(to), r.URL.Query().Get("bank"), r.URL.Query().Get("account"), nullableTime(c.At), nullableID(c.ID), limit+1)
	if err != nil {
		fail(w, r, 503, "unavailable")
		return
	}
	defer rows.Close()
	items := []transactionDTO{}
	next := ""
	for rows.Next() {
		x, err := scanTransaction(rows)
		if err != nil {
			fail(w, r, 503, "unavailable")
			return
		}
		if len(items) == limit {
			next = nextCursor(items[len(items)-1].CreatedAt, items[len(items)-1].ID)
			break
		}
		items = append(items, x)
	}
	jsonOut(w, 200, map[string]any{"items": items, "next_cursor": next})
}
func (a API) transactionDetail(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	x, err := scanTransaction(a.DB.QueryRow(r.Context(), `SELECT `+transactionColumns+` FROM transactions t WHERE t.id=$1 AND t.user_id=$2`, id, auth.FromContext(r.Context()).UserID))
	if errors.Is(err, pgx.ErrNoRows) {
		fail(w, r, 404, "transaction_not_found")
		return
	}
	if err != nil {
		fail(w, r, 503, "unavailable")
		return
	}
	jsonOut(w, 200, x)
}
func (a API) patchTransaction(w http.ResponseWriter, r *http.Request) {
	var q struct {
		Classification  string `json:"classification"`
		Note            string `json:"note"`
		ExpectedVersion int    `json:"expected_version"`
	}
	if decode(r, &q) != nil || q.ExpectedVersion < 1 || (q.Classification != "unclassified" && q.Classification != "expense" && q.Classification != "internal_transfer") || len(q.Note) > 2000 {
		fail(w, r, 422, "invalid_update")
		return
	}
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	user := auth.FromContext(r.Context()).UserID
	tx, err := a.DB.Begin(r.Context())
	if err != nil {
		fail(w, r, 503, "unavailable")
		return
	}
	defer tx.Rollback(r.Context())
	var beforeClass, beforeNote string
	var version int
	err = tx.QueryRow(r.Context(), `SELECT classification,note,version FROM transactions WHERE id=$1 AND user_id=$2 FOR UPDATE`, id, user).Scan(&beforeClass, &beforeNote, &version)
	if errors.Is(err, pgx.ErrNoRows) {
		fail(w, r, 404, "transaction_not_found")
		return
	}
	if err != nil {
		fail(w, r, 503, "unavailable")
		return
	}
	if version != q.ExpectedVersion {
		fail(w, r, 409, "stale_version")
		return
	}
	_, err = tx.Exec(r.Context(), `UPDATE transactions SET classification=$2,note=$3,version=version+1 WHERE id=$1`, id, q.Classification, q.Note)
	if err == nil {
		_, err = tx.Exec(r.Context(), `INSERT INTO transaction_audit(user_id,transaction_id,action,before_values,after_values) VALUES($1,$2,'owner_update',$3,$4)`, user, id, map[string]any{"classification": beforeClass, "note": beforeNote}, map[string]any{"classification": q.Classification, "note": q.Note})
	}
	if err != nil || tx.Commit(r.Context()) != nil {
		fail(w, r, 503, "unavailable")
		return
	}
	a.transactionDetail(w, r)
}
func (a API) ownedAccounts(w http.ResponseWriter, r *http.Request) {
	rows, err := a.DB.Query(r.Context(), `SELECT id,bank,masked_identifier,label FROM owned_accounts WHERE user_id=$1 ORDER BY created_at,id`, auth.FromContext(r.Context()).UserID)
	if err != nil {
		fail(w, r, 503, "unavailable")
		return
	}
	defer rows.Close()
	items := []map[string]string{}
	for rows.Next() {
		var id, bank, masked, label string
		if rows.Scan(&id, &bank, &masked, &label) != nil {
			fail(w, r, 503, "unavailable")
			return
		}
		items = append(items, map[string]string{"id": id, "bank": bank, "masked_identifier": masked, "label": label})
	}
	jsonOut(w, 200, map[string]any{"items": items})
}
func (a API) addOwnedAccount(w http.ResponseWriter, r *http.Request) {
	var q struct {
		Bank          string `json:"bank"`
		AccountNumber string `json:"account_number"`
		Label         string `json:"label"`
	}
	if decode(r, &q) != nil || q.Bank == "" || len(q.Bank) > 80 || len(q.Label) > 80 || len(q.AccountNumber) < 5 || len(q.AccountNumber) > 40 {
		fail(w, r, 422, "invalid_account")
		return
	}
	for _, c := range q.AccountNumber {
		if c < '0' || c > '9' {
			fail(w, r, 422, "invalid_account")
			return
		}
	}
	q.Bank = strings.ToUpper(strings.TrimSpace(q.Bank))
	h := hmac.New(sha256.New, []byte(a.Config.AccountHMACKey))
	h.Write([]byte(q.Bank + "\x00" + q.AccountNumber))
	masked := strings.Repeat("*", len(q.AccountNumber)-4) + q.AccountNumber[len(q.AccountNumber)-4:]
	var id string
	err := a.DB.QueryRow(r.Context(), `INSERT INTO owned_accounts(user_id,bank,match_token,masked_identifier,label) VALUES($1,$2,$3,$4,$5) ON CONFLICT(user_id,bank,match_token) DO UPDATE SET label=EXCLUDED.label RETURNING id`, auth.FromContext(r.Context()).UserID, q.Bank, h.Sum(nil), masked, q.Label).Scan(&id)
	if err != nil {
		fail(w, r, 503, "unavailable")
		return
	}
	jsonOut(w, 201, map[string]string{"id": id, "bank": q.Bank, "masked_identifier": masked, "label": q.Label})
}

// ponytail: one process-wide limiter bounds abuse; use bounded per-client limiters if public abuse becomes measurable.
func throttle(l *rate.Limiter) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !l.Allow() {
				fail(w, r, 429, "rate_limited")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func pathID(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := chi.URLParam(r, "id")
	if _, err := uuid.Parse(id); err != nil {
		fail(w, r, 422, "invalid_id")
		return "", false
	}
	return id, true
}
