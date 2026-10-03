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
