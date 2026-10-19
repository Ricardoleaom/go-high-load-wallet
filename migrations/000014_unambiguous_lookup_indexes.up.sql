-- The idempotency and external-ID indexes both started with provider_id,
-- so a lookup by provider and key could be planned on the external-ID index
-- with provider_id alone and read every transaction of that provider.
-- Being partial (WHERE kind <> 'OPENING') made it worse: with statistics
-- from before a provider's rows arrived, the planner took both indexes for
-- empty, and any scan of either for free.
--
-- Each index now leads with its own column, so a lookup has a single index
-- to use, and neither is partial: an OPENING has no key and no external
-- ID, and NULLs never conflict in a unique index, so the predicate never
-- bought anything but a size estimate the planner could not trust.
--
-- The new indexes are built before the old ones go, so uniqueness holds
-- throughout.
CREATE UNIQUE INDEX idx_transactions_idempotency_unique_v2
    ON transactions (idempotency_key, provider_id);

CREATE UNIQUE INDEX idx_transactions_external_id_unique_v2
    ON transactions (external_id, provider_id);

DROP INDEX idx_transactions_idempotency_unique;
DROP INDEX idx_transactions_external_id_unique;

ALTER INDEX idx_transactions_idempotency_unique_v2 RENAME TO idx_transactions_idempotency_unique;
ALTER INDEX idx_transactions_external_id_unique_v2 RENAME TO idx_transactions_external_id_unique;
