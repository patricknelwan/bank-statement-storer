package transaction

import (
	"example.com/bca/internal/parser/bca"
	"testing"
)

func TestValidate(t *testing.T) {
	fee := "2500.00"
	e := Event{SourceJobID: "00000000-0000-4000-8000-000000000001", SourceMessageID: "test", Receipt: bca.Receipt{
		ParserVersion: "bca-interbank-v1", BankReference: "REF1234", ReceiptKind: "interbank_transfer",
		Status: "successful", Currency: "IDR", Amount: "3300000.00", Fee: &fee,
		SourceAccountAlias: "2180xxxx19", BeneficiaryBank: "BANK MANDIRI",
		BeneficiaryAccountMasked: "******9066", TransactionDateLocal: "2026-10-02T11:20:40", Timezone: "Asia/Jakarta",
	}}
	amount, charge, occurred, err := Validate(e)
	if err != nil || amount != 330000000 || *charge != 250000 || occurred.Format("15:04:05") != "04:20:40" {
		t.Fatalf("%d %v %v %v", amount, charge, occurred, err)
	}
	e.Amount = "1.234"
	if _, _, _, err := Validate(e); err == nil {
		t.Fatal("invalid precision accepted")
	}
}

func TestValidateQRISPayment(t *testing.T) {
	e := Event{SourceJobID: "00000000-0000-4000-8000-000000000001", SourceMessageID: "test", Receipt: bca.Receipt{
		ParserVersion: "bca-qris-payment-v1", BankReference: "REF1234", ReceiptKind: "bca_payment",
		Status: "successful", Currency: "IDR", Amount: "30000.00", SourceAccountAlias: "Tahapan - 1234****56",
		PaymentTo: "Example Merchant", TransactionDateLocal: "2026-10-02T11:20:40", Timezone: "Asia/Jakarta",
	}}
	if _, _, _, err := Validate(e); err != nil {
		t.Fatal(err)
	}
	e.BeneficiaryMatchToken = "deadbeef"
	if _, _, _, err := Validate(e); err == nil {
		t.Fatal("payment match token accepted")
	}
}

func TestValidateAdditionalPaymentVersions(t *testing.T) {
	for _, version := range []string{"bca-flazz-top-up-v1", "bca-qris-transfer-v1"} {
		e := Event{SourceJobID: "00000000-0000-4000-8000-000000000001", SourceMessageID: "test", Receipt: bca.Receipt{
			ParserVersion: version, BankReference: "REF1234", ReceiptKind: "bca_payment", Status: "successful", Currency: "IDR",
			Amount: "42000.00", SourceAccountAlias: "Tahapan - 1234****56", PaymentTo: "Example Recipient",
			TransactionDateLocal: "2026-10-03T09:10:11", Timezone: "Asia/Jakarta",
		}}
		if _, _, _, err := Validate(e); err != nil {
			t.Fatalf("%s: %v", version, err)
		}
	}
}
