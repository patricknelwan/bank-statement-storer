package gmail

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"example.com/bca/internal/auth"
	"example.com/bca/internal/config"
	"example.com/bca/internal/parser/bca"
	"example.com/bca/internal/transaction"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/oauth2"
	"google.golang.org/api/googleapi"
)

var ErrBusy = errors.New("discovery lease busy")

type Worker struct {
	DB              *pgxpool.Pool
	Auth            *auth.Service
	Config          config.Config
	HTTP            *http.Client
	providerFactory func(context.Context, string) (*Provider, error)
}

func NewWorker(db *pgxpool.Pool, a *auth.Service, c config.Config) *Worker {
	return &Worker{DB: db, Auth: a, Config: c, HTTP: &http.Client{Timeout: 20 * time.Second}}
}
func (w *Worker) provider(ctx context.Context, integrationID string) (*Provider, error) {
	if w.providerFactory != nil {
		return w.providerFactory(ctx, integrationID)
	}
	return w.client(ctx, integrationID)
}
func (w *Worker) client(ctx context.Context, integrationID string) (*Provider, error) {
	var encrypted []byte
	err := w.DB.QueryRow(ctx, `SELECT encrypted_refresh_token FROM gmail_integrations WHERE id=$1 AND status='connected'`, integrationID).Scan(&encrypted)
	if err != nil {
		return nil, err
	}
	refresh, err := w.Auth.Decrypt(encrypted, integrationID)
	if err != nil {
		return nil, err
	}
	ts := w.Auth.OAuth.TokenSource(ctx, &oauth2.Token{RefreshToken: refresh})
	token, err := ts.Token()
	if err != nil {
		var retrieve *oauth2.RetrieveError
		if errors.As(err, &retrieve) && retrieve.Response != nil && retrieve.Response.StatusCode == 400 {
			_, _ = w.DB.Exec(ctx, `UPDATE gmail_integrations SET status='reconnect_required' WHERE id=$1`, integrationID)
		}
		return nil, err
	}
	if token.RefreshToken != "" && token.RefreshToken != refresh {
		ciphertext, err := w.Auth.Encrypt(token.RefreshToken, integrationID)
		if err != nil {
			return nil, err
		}
		_, err = w.DB.Exec(ctx, `UPDATE gmail_integrations SET encrypted_refresh_token=$2 WHERE id=$1`, integrationID, ciphertext)
		if err != nil {
			return nil, err
		}
	}
	client := oauth2.NewClient(ctx, oauth2.StaticTokenSource(token))
	client.Timeout = 20 * time.Second
	return NewProvider(ctx, client)
}
func (w *Worker) Run(ctx context.Context) {
	w.cycle(ctx)
	for ctx.Err() == nil {
		wait := w.Config.PollInterval + time.Duration(rand.Int63n(int64(w.Config.PollInterval/10)+1))
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			w.cycle(ctx)
		}
	}
}
func (w *Worker) cycle(ctx context.Context) {
	rows, err := w.DB.Query(ctx, `SELECT id FROM gmail_integrations WHERE status='connected' AND encrypted_refresh_token IS NOT NULL`)
	if err != nil {
		slog.Error("integration list failed", "error", err)
		return
	}
	var ids []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	for _, id := range ids {
		runCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		err := w.Discover(runCtx, id)
		cancel()
		if err != nil && !errors.Is(err, ErrBusy) {
			slog.Warn("discovery failed", "integration_id", id, "error", safeError(err))
		}
	}
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				done, err := w.ProcessOne(ctx)
				if err != nil {
					slog.Warn("job processing failed", "error", safeError(err))
					return
				}
				if !done {
					return
				}
			}
		}()
	}
	wg.Wait()
	_, _ = w.DB.Exec(ctx, `UPDATE bca_email_jobs SET encrypted_source=NULL WHERE encrypted_source IS NOT NULL AND created_at<now()-($1::int * interval '1 day')`, w.Config.RetentionDays)
	_, _ = w.DB.Exec(ctx, `UPDATE gmail_sync_operations SET status='failed',reason_code='interrupted',finished_at=now() WHERE status IN ('queued','running') AND created_at<now()-interval '10 minutes'`)
}
func safeError(err error) string {
	var g *googleapi.Error
	if errors.As(err, &g) {
		return fmt.Sprintf("gmail_http_%d", g.Code)
	}
	return "internal_error"
}
func (w *Worker) lease(ctx context.Context, id string) (int64, string, time.Time, error) {
	var generation int64
	var history string
	var start time.Time
	err := w.DB.QueryRow(ctx, `UPDATE gmail_sync_state s SET lease_until=now()+interval '2 minutes',lease_generation=lease_generation+1,last_attempt=now()
		FROM gmail_integrations i WHERE s.integration_id=$1 AND i.id=s.integration_id AND i.status='connected' AND (s.lease_until IS NULL OR s.lease_until<now())
		RETURNING lease_generation,coalesce(history_id,''),coverage_start`, id).Scan(&generation, &history, &start)
	return generation, history, start, err
}
func (w *Worker) renew(ctx context.Context, id string, generation int64) error {
	tag, err := w.DB.Exec(ctx, `UPDATE gmail_sync_state s SET lease_until=now()+interval '2 minutes' FROM gmail_integrations i WHERE s.integration_id=$1 AND i.id=s.integration_id AND i.status='connected' AND s.lease_generation=$2 AND s.lease_until>now()`, id, generation)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return errors.New("discovery lease lost")
	}
	return nil
}
func (w *Worker) Discover(ctx context.Context, id string) error {
	generation, history, start, err := w.lease(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrBusy
	}
	if err != nil {
		return err
	}
	defer func() {
		release, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, _ = w.DB.Exec(release, `UPDATE gmail_sync_state SET lease_until=NULL WHERE integration_id=$1 AND lease_generation=$2`, id, generation)
	}()
	p, err := w.provider(ctx, id)
	if err != nil {
		return err
	}
	if history == "" {
		profile, err := p.API.Users.GetProfile("me").Context(ctx).Do()
		if err != nil {
			return err
		}
		history = strconv.FormatUint(profile.HistoryId, 10)
		if err = w.scan(ctx, p, id, generation, start); err != nil {
			return err
		}
	} else {
		_, err = strconv.ParseUint(history, 10, 64)
		if err != nil {
			return err
		}
	}
	next, err := w.replay(ctx, p, id, generation, history)
	if isGone(err) {
		_, _ = w.DB.Exec(ctx, `UPDATE gmail_sync_state SET recovery_status='recovering' WHERE integration_id=$1 AND lease_generation=$2`, id, generation)
		profile, e := p.API.Users.GetProfile("me").Context(ctx).Do()
		if e != nil {
			return e
		}
		history = strconv.FormatUint(profile.HistoryId, 10)
		if e = w.scan(ctx, p, id, generation, start); e != nil {
			return e
		}
		next, err = w.replay(ctx, p, id, generation, history)
	}
	if err != nil {
		return err
	}
	tag, err := w.DB.Exec(ctx, `UPDATE gmail_sync_state s SET history_id=$3,last_success=now(),recovery_status='complete',lease_until=NULL FROM gmail_integrations i WHERE s.integration_id=$1 AND i.id=s.integration_id AND i.status='connected' AND s.lease_generation=$2 AND s.lease_until>now()`, id, generation, next)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return errors.New("discovery lease lost")
	}
	return nil
}
func retryAfter(err error) time.Duration {
	var provider *googleapi.Error
	if !errors.As(err, &provider) {
		return 0
	}
	raw := provider.Header.Get("Retry-After")
	if seconds, e := strconv.Atoi(raw); e == nil && seconds > 0 {
		return min(time.Duration(seconds)*time.Second, time.Hour)
	}
	if when, e := http.ParseTime(raw); e == nil {
		return min(max(time.Until(when), 0), time.Hour)
	}
	return 0
}
func isGone(err error) bool { var g *googleapi.Error; return errors.As(err, &g) && g.Code == 404 }
func (w *Worker) scan(ctx context.Context, p *Provider, id string, generation int64, start time.Time) error {
	end := time.Now().Add(time.Second)
	for from := start; from.Before(end); {
		to := from.Add(7 * 24 * time.Hour)
		if to.After(end) {
			to = end
		}
		query := fmt.Sprintf("from:bca@bca.co.id after:%d before:%d", from.Unix()-1, to.Unix()+1)
		page := ""
		for {
			if err := w.renew(ctx, id, generation); err != nil {
				return err
			}
			call := p.API.Users.Messages.List("me").Q(query).MaxResults(500).IncludeSpamTrash(true).Fields("messages/id,nextPageToken")
			if page != "" {
				call = call.PageToken(page)
			}
			result, err := call.Context(ctx).Do()
			if err != nil {
				return err
			}
			for _, m := range result.Messages {
				if err = w.candidate(ctx, p, id, m.Id); err != nil {
					return err
				}
			}
			page = result.NextPageToken
			if page == "" {
				break
			}
		}
		from = to
	}
	return nil
}
func (w *Worker) replay(ctx context.Context, p *Provider, id string, generation int64, history string) (string, error) {
	start, err := strconv.ParseUint(history, 10, 64)
	if err != nil {
		return "", err
	}
	page := ""
	terminal := history
	seen := map[string]bool{}
	for {
		if err = w.renew(ctx, id, generation); err != nil {
			return "", err
		}
		call := p.API.Users.History.List("me").StartHistoryId(start).HistoryTypes("messageAdded").MaxResults(500).Fields("history/messagesAdded/message/id,nextPageToken,historyId")
		if page != "" {
			call = call.PageToken(page)
		}
		result, e := call.Context(ctx).Do()
		if e != nil {
			return "", e
		}
		terminal = strconv.FormatUint(result.HistoryId, 10)
		for _, h := range result.History {
			for _, a := range h.MessagesAdded {
				if a.Message != nil && !seen[a.Message.Id] {
					seen[a.Message.Id] = true
					if err = w.candidate(ctx, p, id, a.Message.Id); err != nil {
						return "", err
					}
				}
			}
		}
		page = result.NextPageToken
		if page == "" {
			return terminal, nil
		}
	}
}
func (w *Worker) candidate(ctx context.Context, p *Provider, integrationID, messageID string) error {
	if messageID == "" {
		return nil
	}
	var known bool
	if err := w.DB.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM bca_email_jobs WHERE integration_id=$1 AND message_id=$2)`, integrationID, messageID).Scan(&known); err != nil {
		return err
	}
	if known {
		return nil
	}
	ok, err := AuthorizedSender(ctx, p, messageID)
	if isGone(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	_, err = w.DB.Exec(ctx, `INSERT INTO bca_email_jobs(integration_id,message_id) SELECT $1,$2 FROM gmail_integrations WHERE id=$1 AND status='connected' ON CONFLICT DO NOTHING`, integrationID, messageID)
	return err
}
func (w *Worker) ProcessOne(ctx context.Context) (bool, error) {
	var id, integrationID, messageID string
	var payload []byte
	var attempts int
	err := w.DB.QueryRow(ctx, `WITH due AS (
		SELECT j.id FROM bca_email_jobs j JOIN gmail_integrations i ON i.id=j.integration_id
		WHERE i.status='connected' AND i.encrypted_refresh_token IS NOT NULL AND j.state IN ('queued','retry_wait','processing')
		AND j.next_attempt_at<=now() AND (j.lease_until IS NULL OR j.lease_until<now())
		ORDER BY j.next_attempt_at,j.created_at FOR UPDATE OF j SKIP LOCKED LIMIT 1
	) UPDATE bca_email_jobs j SET state='processing',attempts=j.attempts+1,lease_until=now()+interval '2 minutes',updated_at=now()
	FROM due WHERE j.id=due.id RETURNING j.id,j.integration_id,j.message_id,j.delivery_payload,j.attempts`).Scan(&id, &integrationID, &messageID, &payload, &attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	runCtx, cancel := context.WithTimeout(ctx, time.Minute)
	err = w.process(runCtx, id, integrationID, messageID, payload)
	cancel()
	if err != nil {
		var provider *googleapi.Error
		if errors.As(err, &provider) && provider.Code == 401 {
			_, _ = w.DB.Exec(ctx, `UPDATE gmail_integrations SET status='reconnect_required' WHERE id=$1`, integrationID)
		}
		if isGone(err) {
			w.outcome(ctx, id, "needs_review", "missing_source")
			return true, nil
		}
		delay := time.Minute*time.Duration(1<<min(attempts-1, 4)) + time.Duration(rand.Int63n(int64(time.Minute)))
		if providerDelay := retryAfter(err); providerDelay > delay {
			delay = providerDelay
		}
		state := "retry_wait"
		if attempts >= 5 {
			state = "failed"
		}
		_, _ = w.DB.Exec(ctx, `UPDATE bca_email_jobs SET state=$2,reason_code='transient_failure',next_attempt_at=now()+$3::interval,lease_until=NULL,updated_at=now() WHERE id=$1 AND state='processing'`, id, state, delay.String())
	}
	return true, err
}
func (w *Worker) outcome(ctx context.Context, id, state, reason string) {
	_, _ = w.DB.Exec(ctx, `UPDATE bca_email_jobs SET state=$2,reason_code=$3,lease_until=NULL,updated_at=now() WHERE id=$1`, id, state, reason)
}
func (w *Worker) process(ctx context.Context, id, integrationID, messageID string, payload []byte) error {
	if payload == nil {
		p, err := w.provider(ctx, integrationID)
		if err != nil {
			return err
		}
		ok, err := AuthorizedSender(ctx, p, messageID)
		if err != nil {
			return err
		}
		if !ok {
			w.outcome(ctx, id, "needs_review", "sender_changed")
			return nil
		}
		message, err := p.Full(ctx, messageID)
		if err != nil {
			return err
		}
		if !w.Config.TrustGmailAuthResults || !message.Verified {
			w.outcome(ctx, id, "needs_review", "authentication_unverified")
			return nil
		}
		receipt, outcome := bca.ParseWithSubject(message.Body, message.Subject)
		if outcome != "" {
			w.outcome(ctx, id, outcome, "parser_"+outcome)
			return nil
		}
		event := transaction.Event{SourceJobID: id, SourceMessageID: messageID, Receipt: receipt}
		if receipt.BeneficiaryAccount != "" {
			h := hmac.New(sha256.New, []byte(w.Config.AccountHMACKey))
			h.Write([]byte(strings.ToUpper(receipt.BeneficiaryBank) + "\x00" + receipt.BeneficiaryAccount))
			event.BeneficiaryMatchToken = hex.EncodeToString(h.Sum(nil))
		}
		event.BeneficiaryAccount = ""
		payload, err = json.Marshal(event)
		if err != nil {
			return err
		}
		ciphertext, err := w.Auth.Encrypt(string(message.Body), id)
		if err != nil {
			return err
		}
		_, err = w.DB.Exec(ctx, `UPDATE bca_email_jobs SET delivery_payload=$2,encrypted_source=$3,parser_version=$4,updated_at=now() WHERE id=$1 AND delivery_payload IS NULL`, id, payload, ciphertext, receipt.ParserVersion)
		if err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.Config.InternalIngestURL, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+w.Config.WorkerToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := w.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode == 200 || resp.StatusCode == 201 {
		return nil
	}
	if resp.StatusCode == 409 || resp.StatusCode == 422 {
		w.outcome(ctx, id, "needs_review", "ingestion_rejected")
		return nil
	}
	return fmt.Errorf("ingestion status %d", resp.StatusCode)
}
