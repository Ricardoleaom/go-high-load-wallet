-- The amount alone does not say which currency was requested: a bet sent in
-- USD against a BRL wallet is rejected, and must still read back as USD.
ALTER TABLE transactions ADD COLUMN currency CHAR(3);

UPDATE transactions t
SET currency = w.currency
FROM wallets w
WHERE w.id = t.wallet_id;

ALTER TABLE transactions
    ALTER COLUMN currency SET NOT NULL,
    ADD CONSTRAINT transactions_currency_format CHECK (currency ~ '^[A-Z]{3}$');
