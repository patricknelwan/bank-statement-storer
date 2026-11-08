ALTER TABLE transactions ADD COLUMN payment_to text NOT NULL DEFAULT '';
ALTER TABLE transactions DROP CONSTRAINT transactions_receipt_kind_check;
ALTER TABLE transactions ADD CONSTRAINT transactions_receipt_kind_check CHECK (receipt_kind IN ('bca_transfer','interbank_transfer','bca_payment'));
