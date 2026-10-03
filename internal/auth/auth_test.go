package auth

import (
 "bytes"
 "context"
 "os"
 "testing"
 "time"

 "example.com/bca/internal/config"
 "github.com/google/uuid"
 "github.com/jackc/pgx/v5/pgxpool"
)

func TestEncryptionContext(t *testing.T) {
 s:=Service{Config:config.Config{EncryptionKey:bytes.Repeat([]byte{1},32)}}
 first,err:=s.Encrypt("secret","integration-a")
 if err!=nil{t.Fatal(err)}
 second,err:=s.Encrypt("secret","integration-a")
 if err!=nil || bytes.Equal(first,second){t.Fatal("nonce reuse")}
 value,err:=s.Decrypt(first,"integration-a")
 if err!=nil || value!="secret"{t.Fatal(err)}
 if _,err=s.Decrypt(first,"integration-b");err==nil{t.Fatal("wrong context accepted")}
}

func TestRefreshReuseRevokesFamily(t *testing.T) {
 raw:=os.Getenv("TEST_DATABASE_URL")
 if raw==""{t.Skip("TEST_DATABASE_URL not set")}
 ctx:=context.Background()
 db,err:=pgxpool.New(ctx,raw)
 if err!=nil{t.Fatal(err)}
 defer db.Close()
 var userID string
 if err=db.QueryRow(ctx,"INSERT INTO users(google_sub,email) VALUES($1,'synthetic@example.invalid') RETURNING id",uuid.NewString()).Scan(&userID);err!=nil{t.Fatal(err)}
 s:=Service{DB:db,Config:config.Config{JWTSecret:"synthetic-jwt-key-with-at-least-32-chars"}}
 pair,err:=s.issue(ctx,userID,uuid.NewString(),time.Now().Add(time.Hour))
 if err!=nil{t.Fatal(err)}
 rotated,err:=s.Refresh(ctx,pair.RefreshToken)
 if err!=nil{t.Fatal(err)}
 if _,err=s.Refresh(ctx,pair.RefreshToken);err==nil{t.Fatal("reused token accepted")}
 var active bool
 if err=db.QueryRow(ctx,"SELECT EXISTS(SELECT 1 FROM sessions WHERE refresh_hash=$1 AND revoked_at IS NULL)",Hash(rotated.RefreshToken)).Scan(&active);err!=nil||active{t.Fatalf("family still active: %v %v",active,err)}
}
