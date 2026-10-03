package gmail

import (
	"context"
	"encoding/base64"
	"example.com/bca/internal/config"
	"fmt"
	gmailapi "google.golang.org/api/gmail/v1"
	"google.golang.org/api/option"
	"net/http"
	"net/http/httptest"
	"testing"
)

type fakeSource struct {
	from      string
	fullCalls int
}

func (f *fakeSource) Metadata(context.Context, string) (string, error) { return f.from, nil }
func (f *fakeSource) Full(context.Context, string) (FullMessage, error) {
	f.fullCalls++
	return FullMessage{Body: []byte("private"), Verified: true}, nil
}
func TestPrivacyGate(t *testing.T) {
	for _, from := range []string{"BCA <fake@example.com>", "bca@bca.co.id.evil", "bca@bca.co.id, x@example.com"} {
		f := &fakeSource{from: from}
		ok, err := AuthorizedSender(context.Background(), f, "id")
		if err != nil || ok || f.fullCalls != 0 {
			t.Fatalf("leak for %q", from)
		}
	}
	f := &fakeSource{from: "BCA <bca@bca.co.id>"}
	ok, err := AuthorizedSender(context.Background(), f, "id")
	if err != nil || !ok {
		t.Fatal("exact sender rejected")
	}
}

func TestMetadataRequest(t *testing.T) {
	var count int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count++
		if r.URL.Query().Get("format") != "metadata" || r.URL.Query().Get("metadataHeaders") != "From" || r.URL.Query().Get("fields") != "id,payload/headers" {
			t.Errorf("unsafe Gmail request: %s", r.URL.String())
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("{\"id\":\"test\",\"payload\":{\"headers\":[{\"name\":\"From\",\"value\":\"fake@example.com\"}]}}"))
	}))
	defer server.Close()
	api, err := gmailapi.NewService(context.Background(), option.WithEndpoint(server.URL+"/"), option.WithHTTPClient(server.Client()))
	if err != nil {
		t.Fatal(err)
	}
	ok, err := AuthorizedSender(context.Background(), Provider{API: api}, "test")
	if err != nil || ok || count != 1 {
		t.Fatalf("gate: ok=%v requests=%d err=%v", ok, count, err)
	}
}

func TestFullSubject(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"test","payload":{"mimeType":"text/html","body":{"data":"c3R1ZmY","size":5},"headers":[{"name":"Subject","value":"Internet Transaction Journal"},{"name":"Authentication-Results","value":"mx.google.com; dkim=pass header.i=@bca.co.id; dmarc=pass header.from=bca.co.id"}]}}`))
	}))
	defer server.Close()
	api, err := gmailapi.NewService(context.Background(), option.WithEndpoint(server.URL+"/"), option.WithHTTPClient(server.Client()))
	if err != nil {
		t.Fatal(err)
	}
	message, err := (Provider{API: api}).Full(context.Background(), "test")
	if err != nil || message.Subject != "Internet Transaction Journal" || !message.Verified || string(message.Body) != "stuff" {
		t.Fatalf("full message fields not extracted: subject=%q verified=%t error=%v", message.Subject, message.Verified, err)
	}
}

func TestInspectUnsupportedChecksSender(t *testing.T) {
	from := "bca@bca.co.id"
	fullCalls := 0
	body := `<table><tr><td>Status</td><td>Successful</td></tr><tr><td>Transaction Type</td><td>Card Payment</td></tr></table>`
	encoded := base64.RawURLEncoding.EncodeToString([]byte(body))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("format") == "metadata" {
			fmt.Fprintf(w, `{"payload":{"headers":[{"name":"From","value":%q}]}}`, from)
			return
		}
		fullCalls++
		fmt.Fprintf(w, `{"payload":{"mimeType":"text/html","body":{"data":%q,"size":%d},"headers":[{"name":"Subject","value":"Internet Transaction Journal"},{"name":"Authentication-Results","value":"mx.google.com; dkim=pass header.i=@bca.co.id; dmarc=pass header.from=bca.co.id"}]}}`, encoded, len(body))
	}))
	defer server.Close()
	api, err := gmailapi.NewService(context.Background(), option.WithEndpoint(server.URL+"/"), option.WithHTTPClient(server.Client()))
	if err != nil {
		t.Fatal(err)
	}
	worker := Worker{Config: config.Config{TrustGmailAuthResults: true}, providerFactory: func(context.Context, string) (*Provider, error) { return &Provider{API: api}, nil }}
	d, err := worker.Inspect(context.Background(), "integration", "message")
	if err != nil || d.ReasonCode != "parser_unsupported_transaction_type" || d.TransactionType != "Card Payment" || fullCalls != 1 {
		t.Fatalf("inspection failed: %+v err=%v full=%d", d, err, fullCalls)
	}
	from = "someone@example.com"
	d, err = worker.Inspect(context.Background(), "integration", "message")
	if err != nil || d.ReasonCode != "sender_changed" || fullCalls != 1 {
		t.Fatalf("sender gate failed: %+v err=%v full=%d", d, err, fullCalls)
	}
}
