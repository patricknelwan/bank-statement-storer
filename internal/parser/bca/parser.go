package bca

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"net/mail"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"golang.org/x/net/html"
)

const MaxBody = 1 << 20

var amountPattern = regexp.MustCompile(`^(?:IDR|Rp)?\s*([0-9]{1,3}(?:,[0-9]{3})+|[0-9]+)\.([0-9]{2})$`)
var referencePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9/_-]{3,79}$`)

type Receipt struct {
	ParserVersion            string  `json:"parser_version"`
	BankReference            string  `json:"bank_reference"`
	ReceiptKind              string  `json:"receipt_kind"`
	Status                   string  `json:"status"`
	Currency                 string  `json:"currency"`
	Amount                   string  `json:"amount"`
	Fee                      *string `json:"fee"`
	SourceAccountAlias       string  `json:"source_account_alias"`
	BeneficiaryBank          string  `json:"beneficiary_bank"`
	BeneficiaryAccountMasked string  `json:"beneficiary_account_masked"`
	BeneficiaryAccount       string  `json:"beneficiary_account,omitempty"`
	PaymentTo                string  `json:"payment_to,omitempty"`
	TransactionDateLocal     string  `json:"transaction_date_local"`
	Timezone                 string  `json:"timezone"`
}

// Diagnostic returns bounded subject and type previews plus allowlisted field names.
type Diagnostic struct {
	ParserOutcome    string   `json:"parser_outcome"`
	ReasonCode       string   `json:"reason_code"`
	RetryRecommended bool     `json:"retry_recommended"`
	Subject          string   `json:"subject,omitempty"`
	TransactionType  string   `json:"transaction_type,omitempty"`
	TransferType     string   `json:"transfer_type,omitempty"`
	DetectedFields   []string `json:"detected_fields"`
	ReceiptKind      string   `json:"receipt_kind,omitempty"`
}

func ExactSender(from string) bool {
	addresses, err := mail.ParseAddressList(from)
	return err == nil && len(addresses) == 1 && strings.EqualFold(addresses[0].Address, "bca@bca.co.id")
}

func Money(raw string) (int64, error) {
	raw = strings.TrimSpace(strings.ReplaceAll(raw, "\u00a0", " "))
	m := amountPattern.FindStringSubmatch(raw)
	if m == nil {
		return 0, errors.New("invalid decimal amount")
	}
	whole := strings.ReplaceAll(m[1], ",", "")
	n, err := strconv.ParseInt(whole, 10, 64)
	if err != nil || n > (math.MaxInt64-99)/100 {
		return 0, errors.New("amount overflow")
	}
	cents, _ := strconv.ParseInt(m[2], 10, 64)
	return n*100 + cents, nil
}

func normalize(s string) string {
	return strings.Join(strings.Fields(strings.ReplaceAll(s, "\u00a0", " ")), " ")
}
func label(s string) string {
	return strings.ToLower(strings.TrimSpace(strings.TrimSuffix(normalize(s), ":")))
}

func cellText(n *html.Node) string {
	if n.Type == html.TextNode {
		return n.Data
	}
	if n.Type == html.ElementNode && (n.Data == "table" || n.Data == "script" || n.Data == "style") {
		return ""
	}
	var b strings.Builder
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		b.WriteString(cellText(c))
		b.WriteByte(' ')
	}
	return b.String()
}

func fieldsFromHTML(body []byte) (map[string]string, error) {
	if len(body) > MaxBody {
		return nil, errors.New("body too large")
	}
	root, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	fields := map[string]string{}
	var visit func(*html.Node)
	visit = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "tr" {
			var cells []string
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				if c.Type == html.ElementNode && (c.Data == "td" || c.Data == "th") {
					cells = append(cells, normalize(cellText(c)))
				}
			}
			if len(cells) >= 2 {
				key := label(cells[0])
				value := ""
				for _, s := range cells[1:] {
					if s != "" && s != ":" {
						value = s
						break
					}
				}
				if key != "" && value != "" {
					fields[key] = value
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			visit(c)
		}
	}
	visit(root)
	return fields, nil
}

// Parse returns ignored, unsupported, or needs_review for emails that cannot be imported.
func Parse(body []byte) (Receipt, string) { return ParseWithSubject(body, "") }

func extractFields(body []byte) (map[string]string, error) {
	f, err := fieldsFromHTML(body)
	if err == nil && len(f) == 0 {
		for _, line := range strings.Split(string(body), "\n") {
			key, value, ok := strings.Cut(line, ":")
			if ok && label(key) != "" && normalize(value) != "" {
				f[label(key)] = normalize(value)
			}
		}
	}
	return f, err
}

func ParseWithSubject(body []byte, subject string) (Receipt, string) {
	f, err := extractFields(body)
	if err != nil {
		return Receipt{}, "needs_review"
	}
	if f["status"] == "" {
		return Receipt{}, "unsupported"
	}
	if !strings.EqualFold(f["status"], "successful") {
		return Receipt{}, "ignored"
	}
	if strings.EqualFold(strings.TrimSpace(subject), "Internet Transaction Journal") {
		if strings.EqualFold(f["transaction type"], "QRIS Payment") {
			return parseQRISPayment(f)
		}
		if strings.EqualFold(f["transaction type"], "Flazz Top Up") {
			return parseFlazzTopUp(f)
		}
		if f["transaction type"] != "" {
			return Receipt{}, "unsupported"
		}
		if strings.EqualFold(f["type of transaction"], "QRIS Transfer") {
			return parseQRISTransfer(f)
		}
		if f["type of transaction"] != "" {
			return Receipt{}, "unsupported"
		}
		if strings.EqualFold(f["transfer type"], "Transfer to BCA Virtual Account") {
			return parseVirtualAccount(f)
		}
	}
	kind := "bca_transfer"
	if strings.Contains(strings.ToLower(f["transfer type"]), "interbank") || f["beneficiary bank"] != "" {
		kind = "interbank_transfer"
	}
	if f["transfer type"] == "" {
		return Receipt{}, "unsupported"
	}
	typeName := strings.ToLower(f["transfer type"])
	if !strings.Contains(typeName, "transfer") || (kind == "bca_transfer" && !strings.Contains(typeName, "bca")) || (kind == "interbank_transfer" && f["beneficiary bank"] == "") {
		return Receipt{}, "unsupported"
	}
	amount := f["transfer amount"]
	if kind == "interbank_transfer" {
		amount = f["amount"]
	}
	if amount == "" {
		return Receipt{}, "needs_review"
	}
	minor, err := Money(amount)
	if err != nil || minor <= 0 {
		return Receipt{}, "needs_review"
	}
	if currency := f["currency"]; currency != "" && currency != "IDR" {
		return Receipt{}, "needs_review"
	}
	fee := (*string)(nil)
	if raw := f["fee"]; raw != "" {
		n, e := Money(raw)
		if e != nil {
			return Receipt{}, "needs_review"
		}
		v := fmt.Sprintf("%d.%02d", n/100, n%100)
		fee = &v
	}
	reference := f["reference no."]
	if reference == "" {
		reference = f["reference no"]
	}
	destination := f["beneficiary account"]
	if destination == "" {
		destination = f["beneficiary account no."]
	}
	if destination == "" {
		destination = f["beneficiary account no"]
	}
	if !referencePattern.MatchString(reference) || f["source of fund"] == "" || destination == "" || f["beneficiary name"] == "" {
		return Receipt{}, "needs_review"
	}
	destination = strings.ReplaceAll(strings.ReplaceAll(destination, " ", ""), "-", "")
	if len(destination) < 5 || len(destination) > 40 {
		return Receipt{}, "needs_review"
	}
	for _, digit := range destination {
		if digit < '0' || digit > '9' {
			return Receipt{}, "needs_review"
		}
	}
	local := f["transaction date"]
	var parsed time.Time
	for _, layout := range []string{"02/01/2006 15:04:05", "02 Jan 2006 15:04:05", "2006-01-02 15:04:05", "02/01/2006 15:04"} {
		parsed, err = time.Parse(layout, local)
		if err == nil {
			break
		}
	}
	if err != nil {
		return Receipt{}, "needs_review"
	}
	if kind == "bca_transfer" {
		f["beneficiary bank"] = "BCA"
	}
	source := maskSource(f["source of fund"])
	masked := destination
	if len(masked) > 4 {
		masked = strings.Repeat("*", len(masked)-4) + masked[len(masked)-4:]
	}
	version := "bca-account-v1"
	if kind == "interbank_transfer" {
		version = "bca-interbank-v1"
	}
	return Receipt{
		ParserVersion: version, BankReference: reference, ReceiptKind: kind, Status: "successful",
		Currency: "IDR", Amount: fmt.Sprintf("%d.%02d", minor/100, minor%100), Fee: fee,
		SourceAccountAlias: source, BeneficiaryBank: f["beneficiary bank"],
		BeneficiaryAccountMasked: masked, BeneficiaryAccount: destination,
		TransactionDateLocal: parsed.Format("2006-01-02T15:04:05"), Timezone: "Asia/Jakarta",
	}, ""
}

func preview(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, normalize(s))
	runes := []rune(s)
	if len(runes) > 120 {
		return string(runes[:120])
	}
	return s
}

func Diagnose(body []byte, subject string) Diagnostic {
	receipt, outcome := ParseWithSubject(body, subject)
	d := Diagnostic{ParserOutcome: outcome, ReasonCode: "parser_" + outcome, Subject: preview(subject), DetectedFields: []string{}}
	if outcome == "" {
		d.ParserOutcome, d.ReasonCode, d.ReceiptKind = "supported", "supported_now", receipt.ReceiptKind
		d.RetryRecommended = true
	}
	f, err := extractFields(body)
	if err != nil {
		return d
	}
	d.TransactionType, d.TransferType = preview(f["transaction type"]), preview(f["transfer type"])
	if d.TransactionType == "" {
		d.TransactionType = preview(f["type of transaction"])
	}
	for _, name := range []string{"status", "transaction type", "type of transaction", "transfer type", "transaction date", "payment to", "source of fund", "total payment", "pay amount", "admin fee", "bca virtual account no.", "name", "company/product name", "top up amount", "flazz card number", "transfer amount", "amount", "beneficiary account", "beneficiary bank", "beneficiary name", "beneficiary pan", "acquirer", "reference no."} {
		if f[name] != "" {
			d.DetectedFields = append(d.DetectedFields, name)
		}
	}
	if outcome != "unsupported" {
		return d
	}
	switch {
	case f["status"] == "":
		d.ReasonCode = "parser_missing_status"
	case strings.EqualFold(strings.TrimSpace(subject), "Internet Transaction Journal") && (f["transaction type"] != "" || f["type of transaction"] != ""):
		d.ReasonCode = "parser_unsupported_transaction_type"
	case strings.EqualFold(strings.TrimSpace(subject), "Internet Transaction Journal") && f["transfer type"] == "":
		d.ReasonCode = "parser_missing_transaction_type"
	case f["transfer type"] == "":
		d.ReasonCode = "parser_missing_transfer_type"
	default:
		d.ReasonCode = "parser_unsupported_transfer_type"
	}
	return d
}

func maskSource(source string) string {
	if len(source) > 4 && !strings.ContainsAny(source, "xX*") {
		return strings.Repeat("*", len(source)-4) + source[len(source)-4:]
	}
	return source
}

func parseQRISPayment(f map[string]string) (Receipt, string) {
	return parseJournalPayment(f, "total payment", f["payment to"], "bca-qris-payment-v1")
}

func parseVirtualAccount(f map[string]string) (Receipt, string) {
	account := strings.NewReplacer(" ", "", "-", "").Replace(f["bca virtual account no."])
	if len(account) < 10 || len(account) > 25 || f["name"] == "" {
		return Receipt{}, "needs_review"
	}
	for _, r := range account {
		if r < '0' || r > '9' {
			return Receipt{}, "needs_review"
		}
	}
	pay, payErr := Money(f["pay amount"])
	fee, feeErr := Money(f["admin fee"])
	total, totalErr := Money(f["total payment"])
	if payErr != nil || feeErr != nil || totalErr != nil || pay <= 0 || fee > math.MaxInt64-pay || pay+fee != total {
		return Receipt{}, "needs_review"
	}
	receipt, outcome := parseJournalPayment(f, "pay amount", f["company/product name"], "bca-virtual-account-v1")
	if outcome != "" {
		return receipt, outcome
	}
	formattedFee := fmt.Sprintf("%d.%02d", fee/100, fee%100)
	receipt.Fee = &formattedFee
	return receipt, ""
}

func digitsOnly(raw string, length int) (string, bool) {
	clean := strings.NewReplacer(" ", "", "-", "").Replace(raw)
	if len(clean) != length {
		return "", false
	}
	for _, r := range clean {
		if r < '0' || r > '9' {
			return "", false
		}
	}
	return clean, true
}

func parseFlazzTopUp(f map[string]string) (Receipt, string) {
	card, ok := digitsOnly(f["flazz card number"], 16)
	if !ok {
		return Receipt{}, "needs_review"
	}
	masked := strings.Repeat("*", len(card)-4) + card[len(card)-4:]
	return parseJournalPayment(f, "top up amount", "Flazz "+masked, "bca-flazz-top-up-v1")
}

func parseQRISTransfer(f map[string]string) (Receipt, string) {
	account := strings.NewReplacer(" ", "", "-", "").Replace(f["beneficiary account"])
	if len(account) < 5 || len(account) > 40 || f["acquirer"] == "" || f["beneficiary name"] == "" || len(f["beneficiary pan"]) < 16 || len(f["beneficiary pan"]) > 19 {
		return Receipt{}, "needs_review"
	}
	for _, r := range account + f["beneficiary pan"] {
		if r < '0' || r > '9' {
			return Receipt{}, "needs_review"
		}
	}
	return parseJournalPayment(f, "amount", f["beneficiary name"], "bca-qris-transfer-v1")
}

func parseJournalPayment(f map[string]string, amountField, paymentTo, version string) (Receipt, string) {
	amount, err := Money(f[amountField])
	if err != nil || amount <= 0 || !referencePattern.MatchString(f["reference no."]) ||
		f["source of fund"] == "" || len(f["source of fund"]) > 80 || paymentTo == "" || len(paymentTo) > 200 ||
		(f["currency"] != "" && f["currency"] != "IDR") {
		return Receipt{}, "needs_review"
	}
	var parsed time.Time
	for _, layout := range []string{"02 Jan 2006 15:04:05", "02/01/2006 15:04:05", "2006-01-02 15:04:05"} {
		parsed, err = time.Parse(layout, f["transaction date"])
		if err == nil {
			break
		}
	}
	if err != nil {
		return Receipt{}, "needs_review"
	}
	return Receipt{
		ParserVersion: version, BankReference: f["reference no."],
		ReceiptKind: "bca_payment", Status: "successful", Currency: "IDR",
		Amount:             fmt.Sprintf("%d.%02d", amount/100, amount%100),
		SourceAccountAlias: maskSource(f["source of fund"]), PaymentTo: paymentTo,
		TransactionDateLocal: parsed.Format("2006-01-02T15:04:05"), Timezone: "Asia/Jakarta",
	}, ""
}
