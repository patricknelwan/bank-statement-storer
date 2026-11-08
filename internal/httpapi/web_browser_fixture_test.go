package httpapi

import (
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// This opt-in fixture serves the built app with real Go sessions and a synthetic issuer.
func TestWebBrowserFixture(t *testing.T) {
	static := os.Getenv("BCA_WEB_TEST_STATIC_DIR")
	if static == "" {
		t.Skip("BCA_WEB_TEST_STATIC_DIR not set")
	}
	if _, err := os.Stat(filepath.Join(static, "index.html")); err != nil {
		t.Fatal(err)
	}
	f := newWebFixture(t, false)
	stop := make(chan struct{})
	var once sync.Once
	files := http.FileServer(http.Dir(static))
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/_test/stop" && r.Method == http.MethodPost {
			w.WriteHeader(204)
			once.Do(func() { close(stop) })
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/auth/") {
			f.router.ServeHTTP(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/assets/") || strings.HasPrefix(r.URL.Path, "/fonts/") {
			files.ServeHTTP(w, r)
			return
		}
		http.ServeFile(w, r, filepath.Join(static, "index.html"))
	}))
	listener, err := net.Listen("tcp", "127.0.0.1:15439")
	if err != nil {
		t.Fatal(err)
	}
	server.Listener = listener
	f.service.Config.PublicURL = "http://127.0.0.1:15439"
	f.service.OAuth.RedirectURL = f.service.Config.PublicURL + "/auth/google/callback"
	server.Start()
	defer server.Close()
	t.Log("Synthetic browser fixture ready at http://127.0.0.1:15439")
	select {
	case <-stop:
	case <-time.After(10 * time.Minute):
		t.Fatal("browser fixture timed out")
	}
}
