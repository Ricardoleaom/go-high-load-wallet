CREATE UNIQUE INDEX idx_transactions_idempotency_unique_v1
    ON transactions (provider_id, idempotency_key)
    WHERE kind <> 'OPENING';

CREATE UNIQUE INDEX idx_transactions_external_id_unique_v1
    ON transactions (provider_id, external_id)
    WHERE kind <> 'OPENING';

DROP INDEX idx_transactions_idempotency_unique;
DROP INDEX idx_transactions_external_id_unique;

ALTER INDEX idx_transactions_idempotency_unique_v1 RENAME TO idx_transactions_idempotency_unique;
ALTER INDEX idx_transactions_external_id_unique_v1 RENAME TO idx_transactions_external_id_unique;
