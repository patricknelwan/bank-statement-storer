package bca

import (
	"os"
	"strings"
	"testing"
)

func TestSenderAndMoney(t *testing.T) {
	for _, s := range []string{"BCA <bca@bca.co.id>", "bca@bca.co.id"} {
		if !ExactSender(s) {
			t.Fatal(s)
		}
	}
	for _, s := range []string{"BCA <fake@example.com>", "bca@bca.co.id.attacker.com", "bca@bca.co.id, fake@example.com", "bad"} {
		if ExactSender(s) {
			t.Fatal(s)
		}
	}
	if n, err := Money("IDR 3,300,000.00"); err != nil || n != 330000000 {
		t.Fatalf("%d %v", n, err)
	}
	for _, s := range []string{"3,30,000.00", "1.234", "92233720368547758.08"} {
		if _, err := Money(s); err == nil {
			t.Fatal(s)
		}
	}
}

func TestReceipts(t *testing.T) {
	for _, tt := range []struct {
		body, kind, amount string
		fee                bool
	}{
		{fixture(t, "account.html"), "bca_transfer", "90000.00", false},
		{fixture(t, "interbank.html"), "interbank_transfer", "3300000.00", true},
	} {
		r, outcome := Parse([]byte(tt.body))
		if outcome != "" || r.ReceiptKind != tt.kind || r.Amount != tt.amount || (r.Fee != nil) != tt.fee {
			t.Fatalf("%+v %s", r, outcome)
		}
	}
}

func TestQRISPayment(t *testing.T) {
	body := []byte(fixture(t, "qris-payment.html"))
	r, outcome := ParseWithSubject(body, "Internet Transaction Journal")
	if outcome != "" || r.ReceiptKind != "bca_payment" || r.ParserVersion != "bca-qris-payment-v1" || r.Amount != "30000.00" || r.PaymentTo != "Example Merchant" || r.SourceAccountAlias != "Tahapan - 1234****56" || r.BeneficiaryAccountMasked != "" || r.TransactionDateLocal != "2026-10-02T11:20:40" {
		t.Fatalf("unexpected QRIS result: %+v %s", r, outcome)
	}
	if _, outcome := ParseWithSubject(body, "Other subject"); outcome != "unsupported" {
		t.Fatalf("wrong subject: %s", outcome)
	}
	if _, outcome := ParseWithSubject([]byte(strings.Replace(string(body), "QRIS Payment", "Other Payment", 1)), "Internet Transaction Journal"); outcome != "unsupported" {
		t.Fatalf("unknown journal layout: %s", outcome)
	}
	if _, outcome := ParseWithSubject([]byte(strings.Replace(string(body), "IDR 30,000.00", "IDR bad", 1)), "Internet Transaction Journal"); outcome != "needs_review" {
		t.Fatalf("invalid amount: %s", outcome)
	}
}

func TestPlainText(t *testing.T) {
	body := "Transfer Type: BCA Account Transfer\nStatus: Successful\nTransfer Amount: IDR 90,000.00\nBeneficiary Account: 1234567890\nBeneficiary Name: Example\nReference No.: REF12345\nSource of Fund: 2180xxxx19\nTransaction Date: 02/10/2026 11:20:40"
	got, outcome := Parse([]byte(body))
	if outcome != "" || got.Amount != "90000.00" || got.Fee != nil {
		t.Fatalf("%+v %s", got, outcome)
	}
}

func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("../../../testdata/bca/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
