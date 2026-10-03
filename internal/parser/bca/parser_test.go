package bca

import (
	"fmt"
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

func TestJournalTransfersUseTransferParser(t *testing.T) {
	for _, tc := range []struct{ fixture, kind string }{
		{"account.html", "bca_transfer"},
		{"interbank.html", "interbank_transfer"},
	} {
		got, outcome := ParseWithSubject([]byte(fixture(t, tc.fixture)), "Internet Transaction Journal")
		if outcome != "" || got.ReceiptKind != tc.kind {
			t.Fatalf("%s: kind=%s outcome=%s", tc.fixture, got.ReceiptKind, outcome)
		}
	}
}

func TestJournalTopUpAndQRISTransfer(t *testing.T) {
	for _, tc := range []struct{ fixture, version, amount, payee string }{
		{"flazz-top-up.html", "bca-flazz-top-up-v1", "50000.00", "Flazz ************3456"},
		{"qris-transfer.html", "bca-qris-transfer-v1", "42000.00", "Example Recipient"},
	} {
		body := []byte(fixture(t, tc.fixture))
		r, outcome := ParseWithSubject(body, "Internet Transaction Journal")
		if outcome != "" || r.ReceiptKind != "bca_payment" || r.ParserVersion != tc.version || r.Amount != tc.amount || r.PaymentTo != tc.payee || r.Fee != nil {
			t.Fatalf("%s: %+v %s", tc.fixture, r, outcome)
		}
		if strings.Contains(fmt.Sprintf("%+v", r), "1234567890123456") || strings.Contains(fmt.Sprintf("%+v", r), "123456789012345678") {
			t.Fatalf("%s: full card/PAN leaked", tc.fixture)
		}
		if d := Diagnose(body, "Internet Transaction Journal"); d.ParserOutcome != "supported" || !d.RetryRecommended {
			t.Fatalf("%s: %+v", tc.fixture, d)
		}
	}
	flazz := fixture(t, "flazz-top-up.html")
	if _, outcome := ParseWithSubject([]byte(strings.Replace(flazz, "1234567890123456", "not-a-card", 1)), "Internet Transaction Journal"); outcome != "needs_review" {
		t.Fatalf("invalid card: %s", outcome)
	}
	transfer := fixture(t, "qris-transfer.html")
	if _, outcome := ParseWithSubject([]byte(strings.Replace(transfer, "123456789012345678", "invalid-pan", 1)), "Internet Transaction Journal"); outcome != "needs_review" {
		t.Fatalf("invalid PAN: %s", outcome)
	}
	if _, outcome := ParseWithSubject([]byte(strings.Replace(transfer, "IDR 42,000.00", "bad amount", 1)), "Internet Transaction Journal"); outcome != "needs_review" {
		t.Fatalf("invalid amount: %s", outcome)
	}
}

func TestVirtualAccountPayment(t *testing.T) {
	body := fixture(t, "virtual-account.html")
	r, outcome := ParseWithSubject([]byte(body), "Internet Transaction Journal")
	if outcome != "" || r.ReceiptKind != "bca_payment" || r.ParserVersion != "bca-virtual-account-v1" || r.Amount != "50000.00" || r.Fee == nil || *r.Fee != "1000.00" || r.PaymentTo != "EXAMPLE WALLET / TOP UP" {
		t.Fatalf("unexpected VA receipt: %+v outcome=%s", r, outcome)
	}
	if strings.Contains(fmt.Sprintf("%+v", r), "12345678901234567") {
		t.Fatal("full virtual account number leaked")
	}
	if _, outcome := ParseWithSubject([]byte(strings.Replace(body, "IDR 51,000.00", "IDR 52,000.00", 1)), "Internet Transaction Journal"); outcome != "needs_review" {
		t.Fatalf("mismatched total: %s", outcome)
	}
	if _, outcome := ParseWithSubject([]byte(strings.Replace(body, "12345678901234567", "invalid-account", 1)), "Internet Transaction Journal"); outcome != "needs_review" {
		t.Fatalf("invalid VA: %s", outcome)
	}
}

func TestDiagnoseUnsupported(t *testing.T) {
	body := `<table><tr><td>Status</td><td>Successful</td></tr><tr><td>Transaction Type</td><td>Card Payment</td></tr><tr><td>Total Payment</td><td>IDR 30,000.00</td></tr><tr><td>Payment To</td><td>Private Merchant</td></tr><tr><td>Reference No.</td><td>REF1234</td></tr></table>`
	d := Diagnose([]byte(body), "Internet Transaction Journal")
	if d.ParserOutcome != "unsupported" || d.ReasonCode != "parser_unsupported_transaction_type" || d.TransactionType != "Card Payment" || d.Subject != "Internet Transaction Journal" {
		t.Fatalf("unexpected diagnosis: %+v", d)
	}
	encoded := fmt.Sprintf("%+v", d)
	if strings.Contains(encoded, "30,000") || strings.Contains(encoded, "Private Merchant") || strings.Contains(encoded, "REF1234") {
		t.Fatalf("financial data leaked in diagnosis: %s", encoded)
	}
	if got := Diagnose([]byte(`<table><tr><td>Other</td><td>Value</td></tr></table>`), "Other").ReasonCode; got != "parser_missing_status" {
		t.Fatalf("missing status reason: %s", got)
	}
	if got := Diagnose([]byte(fixture(t, "qris-payment.html")), "Internet Transaction Journal"); got.ParserOutcome != "supported" || got.ReceiptKind != "bca_payment" || !got.RetryRecommended {
		t.Fatalf("supported diagnosis: %+v", got)
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
