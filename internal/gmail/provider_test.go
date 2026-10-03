package gmail

import (
	"context"
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
func (f *fakeSource) Full(context.Context, string) ([]byte, bool, error) {
	f.fullCalls++
	return []byte("private"), true, nil
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
