package transaction

import (
 "context"
 "encoding/json"
 "os"
 "sync"
 "testing"

 "example.com/bca/internal/parser/bca"
 "github.com/google/uuid"
 "github.com/jackc/pgx/v5/pgxpool"
)

func TestConcurrentDelivery(t *testing.T) {
 url:=os.Getenv("TEST_DATABASE_URL")
 if url=="" { t.Skip("TEST_DATABASE_URL not set") }
 ctx:=context.Background()
 db,err:=pgxpool.New(ctx,url)
 if err!=nil { t.Fatal(err) }
 defer db.Close()
 sub:=uuid.NewString()
 var userID,integrationID,jobID string
 if err=db.QueryRow(ctx,"INSERT INTO users(google_sub,email) VALUES($1,'test@example.invalid') RETURNING id",sub).Scan(&userID);err!=nil{t.Fatal(err)}
 if err=db.QueryRow(ctx,"INSERT INTO gmail_integrations(user_id,mailbox_id,status) VALUES($1,$2,'connected') RETURNING id",userID,sub).Scan(&integrationID);err!=nil{t.Fatal(err)}
 messageID:="synthetic-"+sub
 if err=db.QueryRow(ctx,"INSERT INTO bca_email_jobs(integration_id,message_id,state) VALUES($1,$2,'processing') RETURNING id",integrationID,messageID).Scan(&jobID);err!=nil{t.Fatal(err)}
 e:=Event{SourceJobID:jobID,SourceMessageID:messageID,Receipt:bca.Receipt{ParserVersion:"bca-account-v1",BankReference:"TEST"+sub[:8],ReceiptKind:"bca_transfer",Status:"successful",Currency:"IDR",Amount:"90000.00",SourceAccountAlias:"****1234",BeneficiaryBank:"BCA",BeneficiaryAccountMasked:"******7890",TransactionDateLocal:"2026-10-02T11:20:40",Timezone:"Asia/Jakarta"}}
 payload,_:=json.Marshal(e)
 if _,err=db.Exec(ctx,"UPDATE bca_email_jobs SET delivery_payload=$2 WHERE id=$1",jobID,payload);err!=nil{t.Fatal(err)}
 var wg sync.WaitGroup
 results:=make(chan Result,2);errs:=make(chan error,2)
 for i:=0;i<2;i++{wg.Add(1);go func(){defer wg.Done();r,e:=(Service{DB:db}).Ingest(ctx,e);results<-r;errs<-e}()}
 wg.Wait();close(results);close(errs)
 created:=0
 var id string
 for e:=range errs{if e!=nil{t.Fatal(e)}}
 for r:=range results{if r.Created{created++};if id!=""&&r.ID!=id{t.Fatal("different transactions")};id=r.ID}
 if created!=1{t.Fatalf("created %d records",created)}
 var count int
 if err=db.QueryRow(ctx,"SELECT count(*) FROM transaction_sources WHERE job_id=$1",jobID).Scan(&count);err!=nil||count!=1{t.Fatalf("source count %d %v",count,err)}
 e.Amount="90001.00"
 if _,err:=(Service{DB:db}).Ingest(ctx,e);err==nil{t.Fatal("contradictory replay accepted")}
}
