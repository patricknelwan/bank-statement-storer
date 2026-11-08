package auth

import (
	"context"
	"os"
	"testing"

	"example.com/bca/internal/config"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestMobileCodeRequiresMatchingVerifier(t *testing.T) {
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
	var userID string
	if err = db.QueryRow(ctx, "INSERT INTO users(google_sub,email) VALUES($1,'synthetic@example.invalid') RETURNING id", uuid.NewString()).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	code, err := Random()
	if err != nil {
		t.Fatal(err)
	}
	verifier := "0123456789012345678901234567890123456789012"
	if _, err = db.Exec(ctx, `INSERT INTO auth_flows(kind,secret_hash,user_id,client_challenge,expires_at) VALUES('login_code',$1,$2,$3,now()+interval '1 minute')`, Hash(code), userID, appChallenge(verifier)); err != nil {
		t.Fatal(err)
	}
	s := Service{DB: db, Config: config.Config{JWTSecret: "synthetic-jwt-key-with-at-least-32-chars"}}
	if _, err = s.Exchange(ctx, code, "wrong"); err == nil {
		t.Fatal("wrong verifier accepted")
	}
	if _, err = s.Exchange(ctx, code, verifier); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Exchange(ctx, code, verifier); err == nil {
		t.Fatal("code replay accepted")
	}
}
