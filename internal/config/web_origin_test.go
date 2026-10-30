package config

import "testing"

func TestPublicOrigin(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{"https://Statement.test:443/", "https://statement.test"},
		{"http://localhost:5174/", "http://localhost:5174"},
		{"http://127.0.0.1:5174", "http://127.0.0.1:5174"},
	} {
		got, err := PublicOrigin(tc.raw)
		if err != nil || got != tc.want {
			t.Fatalf("%q => %q, %v", tc.raw, got, err)
		}
	}
	for _, raw := range []string{"http://service", "http://192.168.0.1:5174", "http://0.0.0.0:5174", "https://statement.test/path", "https://statement.test?x=1", "https://user:password@statement.test", "https://statement.test/#fragment"} {
		if _, err := PublicOrigin(raw); err == nil {
			t.Fatalf("unsafe public origin accepted: %q", raw)
		}
	}
}
