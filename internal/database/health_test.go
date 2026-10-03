package database

import (
 "context"
 "os"
 "testing"
 "github.com/jackc/pgx/v5/pgxpool"
)
func TestGeneratedHealthQueries(t *testing.T) {
 raw:=os.Getenv("TEST_DATABASE_URL")
 if raw==""{t.Skip("TEST_DATABASE_URL not set")}
 ctx:=context.Background()
 db,err:=pgxpool.New(ctx,raw)
 if err!=nil{t.Fatal(err)}
 defer db.Close()
 q:=New(db)
 ready,err:=q.DatabaseReady(ctx)
 if err!=nil||!ready{t.Fatalf("ready=%v err=%v",ready,err)}
 if _,err=q.CountPendingJobs(ctx);err!=nil{t.Fatal(err)}
}
