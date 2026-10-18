-- The PostgreSQL engine runs every command as one call to one of these
-- functions: one round trip and one commit, with the same rules as the
-- reference model in internal/app/engine/model (see ADR 0014).
--
-- Lock order, the same everywhere so two commands never wait on each other
-- in a cycle: a pending reversal's own row, then the original a reversal
-- names, then the wallet. Wallets are locked FOR NO KEY UPDATE, which does
-- not block the KEY SHARE locks that foreign keys to them take.

CREATE TYPE wager_result AS (
    outcome        text,    -- APPLIED, REPLAY, IDEMPOTENCY_CONFLICT, KEY_MISMATCH, WALLET_NOT_FOUND, NOT_FOUND
    transaction_id uuid,
    status         text,
    failure_code   text,
    balance        bigint,  -- set for PROCESSED only
    currency       text,
    raced          boolean  -- lost a race and answered as a replay
);

-- wallet_money_text renders minor units as money.DecimalString does.
CREATE FUNCTION wallet_money_text(units bigint) RETURNS text
LANGUAGE sql IMMUTABLE STRICT PARALLEL SAFE AS $$
    SELECT CASE
        WHEN units >= 0 THEN (units / 100)::text || '.' || lpad((units % 100)::text, 2, '0')
        ELSE '-' || trunc(-(units::numeric) / 100)::text || '.' || lpad((-(units::numeric) % 100)::text, 2, '0')
    END
$$;

-- wager_emit writes one outbox event in the envelope internal/app/events
-- builds; causationId is left out when there is none.
CREATE FUNCTION wager_emit(
    p_id uuid, p_type text, p_aggregate_id uuid, p_correlation_id text,
    p_causation_id uuid, p_occurred_at timestamptz, p_data jsonb
) RETURNS void
LANGUAGE sql AS $$
    INSERT INTO outbox_events (id, aggregate_id, event_type, payload, correlation_id, causation_id, occurred_at)
    VALUES (
        p_id, p_aggregate_id::text, p_type,
        jsonb_build_object(
            'eventId', p_id,
            'eventType', p_type,
            'aggregateId', p_aggregate_id,
            'correlationId', p_correlation_id,
            'occurredAt', to_char(p_occurred_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
            'version', 1,
            'data', p_data
        ) || CASE WHEN p_causation_id IS NULL THEN '{}'::jsonb
                  ELSE jsonb_build_object('causationId', p_causation_id) END,
        p_correlation_id, p_causation_id::text, p_occurred_at
    )
$$;

-- wager_replay answers what a recorded transaction answered the first
-- time: the balance right after it for a movement, the current balance for
-- a LOSS. A payload hash that differs is a conflict; NULL skips the check.
CREATE FUNCTION wager_replay(t transactions, p_payload_hash text, p_raced boolean)
RETURNS wager_result
LANGUAGE plpgsql AS $$
DECLARE
    r wager_result;
BEGIN
    IF p_payload_hash IS NOT NULL AND t.payload_hash IS DISTINCT FROM p_payload_hash THEN
        r.outcome := 'IDEMPOTENCY_CONFLICT';
        RETURN r;
    END IF;
    r.outcome := 'REPLAY';
    r.transaction_id := t.id;
    r.status := t.status;
    r.failure_code := t.failure_code;
    r.raced := p_raced;
    IF t.status = 'PROCESSED' THEN
        r.currency := t.currency;
        SELECT le.balance_after_minor_units INTO r.balance
          FROM ledger_entries le
         WHERE le.transaction_id = t.id;
        IF NOT FOUND THEN
            SELECT w.balance_minor_units INTO r.balance FROM wallets w WHERE w.id = t.wallet_id;
        END IF;
    END IF;
    RETURN r;
END
$$;

-- wager_lost_race answers a submission whose insert found its key or its
-- external ID taken by a transaction that committed in the meantime. Each
-- statement of a function sees what committed before it started, so the
-- winner is visible here.
CREATE FUNCTION wager_lost_race(
    p_provider_id text, p_idempotency_key text, p_external_id text, p_payload_hash text
) RETURNS wager_result
LANGUAGE plpgsql AS $$
DECLARE
    t transactions%ROWTYPE;
    r wager_result;
BEGIN
    SELECT * INTO t FROM transactions
     WHERE provider_id = p_provider_id AND idempotency_key = p_idempotency_key AND kind <> 'OPENING';
    IF FOUND THEN
        RETURN wager_replay(t, p_payload_hash, true);
    END IF;
    PERFORM 1 FROM transactions
     WHERE provider_id = p_provider_id AND external_id = p_external_id AND kind <> 'OPENING';
    IF FOUND THEN
        r.outcome := 'KEY_MISMATCH';
        RETURN r;
    END IF;
    RAISE EXCEPTION 'transaction (%, %) conflicted with no committed transaction', p_provider_id, p_idempotency_key;
END
$$;

-- wager_resolve_reference finds and locks the original a REFUND or ROLLBACK
-- names and decides, as wager.DecideReversal does, whether the reversal
-- waits (WAIT), is rejected (REJECT with a failure code) or moves money
-- back (APPLY with the direction). The original's lock serializes every
-- reversal of it, so the already-reversed check cannot race.
CREATE FUNCTION wager_resolve_reference(
    p_provider_id text, p_reference_external_id text, p_kind text,
    p_player_id uuid, p_wallet_id uuid, p_round_id text, p_amount bigint, p_currency text,
    OUT o_decision text, OUT o_failure_code text, OUT o_original_id uuid, OUT o_direction text
)
LANGUAGE plpgsql AS $$
DECLARE
    orig transactions%ROWTYPE;
BEGIN
    SELECT * INTO orig FROM transactions
     WHERE provider_id = p_provider_id AND external_id = p_reference_external_id AND kind <> 'OPENING'
       FOR UPDATE;
    IF NOT FOUND OR orig.status IN ('PENDING', 'PENDING_REFERENCE') THEN
        o_decision := 'WAIT';
        RETURN;
    END IF;

    o_original_id := orig.id;
    o_decision := 'REJECT';
    IF orig.status <> 'PROCESSED' THEN
        o_failure_code := 'REFERENCE_NOT_PROCESSED';
    ELSIF orig.player_id <> p_player_id OR orig.wallet_id <> p_wallet_id
          OR orig.round_id IS DISTINCT FROM p_round_id OR orig.currency <> p_currency
          OR orig.amount_minor_units <> p_amount THEN
        o_failure_code := 'REFERENCE_MISMATCH';
    ELSIF NOT ((p_kind = 'REFUND' AND orig.kind = 'BET')
               OR (p_kind = 'ROLLBACK' AND orig.kind IN ('BET', 'WIN', 'REFUND'))) THEN
        o_failure_code := 'REFERENCE_KIND_NOT_REVERSIBLE';
    ELSIF EXISTS (
        SELECT 1 FROM transactions rev
         WHERE rev.provider_id = p_provider_id AND rev.reference_id = orig.id::text
           AND rev.status = 'PROCESSED' AND rev.kind IN ('REFUND', 'ROLLBACK')
    ) THEN
        o_failure_code := 'REFERENCE_ALREADY_REVERSED';
    ELSE
        o_decision := 'APPLY';
        o_direction := CASE WHEN p_kind = 'REFUND' OR orig.kind = 'BET' THEN 'CREDIT' ELSE 'DEBIT' END;
    END IF;
END
$$;

-- wager_finish writes what follows a transaction's final status: its event
-- and, for a PROCESSED movement, the wallet update, the ledger entry and
-- the WalletBalanceChanged event caused by the first. p_current_balance is
-- the balance a PROCESSED LOSS answers with.
CREATE FUNCTION wager_finish(
    p_transaction_id uuid, p_entry_id uuid, p_event_id uuid, p_balance_event_id uuid,
    p_wallet_id uuid, p_kind text, p_amount bigint, p_currency text, p_reference_external_id text,
    p_status text, p_failure_code text, p_direction text, p_current_balance bigint,
    p_correlation_id text, p_now timestamptz
) RETURNS wager_result
LANGUAGE plpgsql AS $$
DECLARE
    r       wager_result;
    v_delta bigint;
    v_after bigint;
    v_version bigint;
BEGIN
    r.outcome := 'APPLIED';
    r.transaction_id := p_transaction_id;
    r.status := p_status;
    r.failure_code := p_failure_code;
    r.raced := false;

    IF p_status = 'REJECTED' THEN
        PERFORM wager_emit(p_event_id, 'WagerTransactionRejected', p_transaction_id, p_correlation_id, NULL, p_now,
            jsonb_build_object('transactionId', p_transaction_id, 'failureCode', p_failure_code));
        RETURN r;
    END IF;
    IF p_status = 'PENDING_REFERENCE' THEN
        PERFORM wager_emit(p_event_id, 'WagerTransactionPendingReference', p_transaction_id, p_correlation_id, NULL, p_now,
            jsonb_build_object('transactionId', p_transaction_id, 'referenceExternalTransactionId', p_reference_external_id));
        RETURN r;
    END IF;

    PERFORM wager_emit(p_event_id, 'WagerTransactionProcessed', p_transaction_id, p_correlation_id, NULL, p_now,
        jsonb_build_object(
            'transactionId', p_transaction_id,
            'walletId', p_wallet_id,
            'kind', p_kind,
            'money', jsonb_build_object('amount', wallet_money_text(p_amount), 'currency', p_currency)));
    r.currency := p_currency;
    IF p_direction IS NULL THEN
        r.balance := p_current_balance;
        RETURN r;
    END IF;

    v_delta := CASE WHEN p_direction = 'DEBIT' THEN -p_amount ELSE p_amount END;
    UPDATE wallets
       SET balance_minor_units = balance_minor_units + v_delta,
           version = version + 1,
           updated_at = p_now
     WHERE id = p_wallet_id
    RETURNING balance_minor_units, version INTO v_after, v_version;

    INSERT INTO ledger_entries (
        id, wallet_id, transaction_id, direction, amount_delta_minor_units,
        balance_before_minor_units, balance_after_minor_units, created_at
    ) VALUES (
        p_entry_id, p_wallet_id, p_transaction_id, p_direction, v_delta,
        v_after - v_delta, v_after, p_now
    );

    PERFORM wager_emit(p_balance_event_id, 'WalletBalanceChanged', p_wallet_id, p_correlation_id, p_event_id, p_now,
        jsonb_build_object(
            'walletId', p_wallet_id,
            'transactionId', p_transaction_id,
            'direction', p_direction,
            'money', jsonb_build_object('amount', wallet_money_text(p_amount), 'currency', p_currency),
            'balanceBefore', jsonb_build_object('amount', wallet_money_text(v_after - v_delta), 'currency', p_currency),
            'balanceAfter', jsonb_build_object('amount', wallet_money_text(v_after), 'currency', p_currency),
            'walletVersion', v_version));
    r.balance := v_after;
    RETURN r;
END
$$;

-- wallet_open creates a wallet at version 1 and, for a positive balance,
-- its OPENING transaction, ledger credit and events. It returns the
-- creation time, or NULL when the player already has a wallet in that
-- currency.
CREATE FUNCTION wallet_open(
    p_wallet_id uuid, p_player_id uuid, p_currency text, p_balance bigint,
    p_transaction_id uuid, p_entry_id uuid, p_event_id uuid, p_balance_event_id uuid
) RETURNS timestamptz
LANGUAGE plpgsql AS $$
DECLARE
    v_now  timestamptz := clock_timestamp();
    v_corr text := p_transaction_id::text;
BEGIN
    INSERT INTO wallets (id, player_id, currency, balance_minor_units, version, created_at, updated_at)
    VALUES (p_wallet_id, p_player_id, p_currency, p_balance, 1, v_now, v_now)
    ON CONFLICT DO NOTHING;
    IF NOT FOUND THEN
        RETURN NULL;
    END IF;
    IF p_balance = 0 THEN
        RETURN v_now;
    END IF;

    INSERT INTO transactions (id, wallet_id, player_id, kind, amount_minor_units, currency, status, created_at, updated_at)
    VALUES (p_transaction_id, p_wallet_id, p_player_id, 'OPENING', p_balance, p_currency, 'PROCESSED', v_now, v_now);
    INSERT INTO ledger_entries (
        id, wallet_id, transaction_id, direction, amount_delta_minor_units,
        balance_before_minor_units, balance_after_minor_units, created_at
    ) VALUES (p_entry_id, p_wallet_id, p_transaction_id, 'CREDIT', p_balance, 0, p_balance, v_now);

    PERFORM wager_emit(p_event_id, 'WagerTransactionProcessed', p_transaction_id, v_corr, NULL, v_now,
        jsonb_build_object(
            'transactionId', p_transaction_id,
            'walletId', p_wallet_id,
            'kind', 'OPENING',
            'money', jsonb_build_object('amount', wallet_money_text(p_balance), 'currency', p_currency)));
    PERFORM wager_emit(p_balance_event_id, 'WalletBalanceChanged', p_wallet_id, v_corr, p_event_id, v_now,
        jsonb_build_object(
            'walletId', p_wallet_id,
            'transactionId', p_transaction_id,
            'direction', 'CREDIT',
            'money', jsonb_build_object('amount', wallet_money_text(p_balance), 'currency', p_currency),
            'balanceBefore', jsonb_build_object('amount', wallet_money_text(0), 'currency', p_currency),
            'balanceAfter', jsonb_build_object('amount', wallet_money_text(p_balance), 'currency', p_currency),
            'walletVersion', 1));
    RETURN v_now;
END
$$;

-- wager_submit applies a provider's transaction once:
--   1. a transaction under the same key is replayed (or conflicts when its
--      payload differs), and one under the same external ID with another
--      key is a key mismatch, all before taking any lock;
--   2. the wallet must exist and belong to the player (BET and WIN lock it
--      here; the others only read it, to keep the lock order);
--   3. the transaction is inserted directly in its final status; when a
--      concurrent submission inserted the same key or external ID first,
--      nothing has moved yet and the winner answers instead;
--   4. a PROCESSED movement updates the wallet and writes its ledger entry;
--      every outcome writes its events.
-- p_retry_delay is how long a reversal waiting for its original waits
-- before the first retry.
CREATE FUNCTION wager_submit(
    p_transaction_id uuid, p_entry_id uuid, p_event_id uuid, p_balance_event_id uuid,
    p_provider_id text, p_external_id text, p_idempotency_key text, p_payload_hash text,
    p_wallet_id uuid, p_player_id uuid, p_round_id text, p_game_id text,
    p_kind text, p_amount bigint, p_currency text, p_reference_external_id text,
    p_correlation_id text, p_retry_delay interval
) RETURNS wager_result
LANGUAGE plpgsql AS $$
DECLARE
    t           transactions%ROWTYPE;
    w           wallets%ROWTYPE;
    ref         record;
    r           wager_result;
    v_now       timestamptz;
    v_status    text := 'PROCESSED';
    v_failure   text;
    v_direction text;
    v_reference uuid;
    v_attempts  int := 0;
    v_next      timestamptz;
    v_corr      text := COALESCE(NULLIF(p_correlation_id, ''), p_transaction_id::text);
BEGIN
    SELECT * INTO t FROM transactions
     WHERE provider_id = p_provider_id AND idempotency_key = p_idempotency_key AND kind <> 'OPENING';
    IF FOUND THEN
        RETURN wager_replay(t, p_payload_hash, false);
    END IF;
    PERFORM 1 FROM transactions
     WHERE provider_id = p_provider_id AND external_id = p_external_id AND kind <> 'OPENING';
    IF FOUND THEN
        r.outcome := 'KEY_MISMATCH';
        RETURN r;
    END IF;

    IF p_kind IN ('BET', 'WIN') THEN
        SELECT * INTO w FROM wallets WHERE id = p_wallet_id FOR NO KEY UPDATE;
    ELSE
        SELECT * INTO w FROM wallets WHERE id = p_wallet_id;
    END IF;
    IF NOT FOUND OR w.player_id <> p_player_id THEN
        r.outcome := 'WALLET_NOT_FOUND';
        RETURN r;
    END IF;

    IF p_kind IN ('REFUND', 'ROLLBACK') THEN
        SELECT * INTO ref FROM wager_resolve_reference(
            p_provider_id, p_reference_external_id, p_kind,
            p_player_id, p_wallet_id, p_round_id, p_amount, p_currency);
        v_reference := ref.o_original_id;
        IF ref.o_decision = 'WAIT' THEN
            v_status := 'PENDING_REFERENCE';
            v_attempts := 1;
        ELSIF ref.o_decision = 'REJECT' THEN
            v_status := 'REJECTED';
            v_failure := ref.o_failure_code;
        ELSE
            SELECT * INTO w FROM wallets WHERE id = p_wallet_id FOR NO KEY UPDATE;
            IF ref.o_direction = 'DEBIT' AND w.balance_minor_units < p_amount THEN
                v_status := 'REJECTED';
                v_failure := 'ROLLBACK_INSUFFICIENT_BALANCE';
            ELSE
                v_direction := ref.o_direction;
            END IF;
        END IF;
    ELSIF w.currency <> p_currency THEN
        v_status := 'REJECTED';
        v_failure := 'CURRENCY_MISMATCH';
    ELSIF p_kind = 'BET' AND w.balance_minor_units < p_amount THEN
        v_status := 'REJECTED';
        v_failure := 'INSUFFICIENT_BALANCE';
    ELSIF p_kind = 'BET' THEN
        v_direction := 'DEBIT';
    ELSIF p_kind = 'WIN' THEN
        v_direction := 'CREDIT';
    END IF;

    -- Taken after the wallet lock: a wallet's ledger reads in the order its
    -- movements were applied.
    v_now := clock_timestamp();
    IF v_status = 'PENDING_REFERENCE' THEN
        v_next := v_now + p_retry_delay;
    END IF;

    INSERT INTO transactions (
        id, provider_id, external_id, idempotency_key, payload_hash, wallet_id, player_id,
        round_id, game_id, kind, amount_minor_units, currency, reference_external_id, reference_id,
        status, failure_code, reference_attempts, reference_next_attempt_at, created_at, updated_at
    ) VALUES (
        p_transaction_id, p_provider_id, p_external_id, p_idempotency_key, p_payload_hash, p_wallet_id, p_player_id,
        p_round_id, p_game_id, p_kind, p_amount, p_currency, NULLIF(p_reference_external_id, ''), v_reference::text,
        v_status, v_failure, v_attempts, v_next, v_now, v_now
    )
    ON CONFLICT DO NOTHING;
    IF NOT FOUND THEN
        RETURN wager_lost_race(p_provider_id, p_idempotency_key, p_external_id, p_payload_hash);
    END IF;

    RETURN wager_finish(
        p_transaction_id, p_entry_id, p_event_id, p_balance_event_id,
        p_wallet_id, p_kind, p_amount, p_currency, p_reference_external_id,
        v_status, v_failure, v_direction, w.balance_minor_units, v_corr, v_now);
END
$$;

-- wager_retry_reference tries again to resolve a PENDING_REFERENCE
-- reversal. Still unresolved, it waits p_retry_delay more, or is rejected
-- with REFERENCE_NOT_FOUND_TIMEOUT when p_give_up. A transaction that is no
-- longer pending is answered as a replay.
CREATE FUNCTION wager_retry_reference(
    p_transaction_id uuid, p_entry_id uuid, p_event_id uuid, p_balance_event_id uuid,
    p_give_up boolean, p_retry_delay interval
) RETURNS wager_result
LANGUAGE plpgsql AS $$
DECLARE
    t           transactions%ROWTYPE;
    w           wallets%ROWTYPE;
    ref         record;
    r           wager_result;
    v_now       timestamptz;
    v_status    text := 'PROCESSED';
    v_failure   text;
    v_direction text;
BEGIN
    SELECT * INTO t FROM transactions WHERE id = p_transaction_id FOR UPDATE;
    IF NOT FOUND THEN
        r.outcome := 'NOT_FOUND';
        RETURN r;
    END IF;
    IF t.status <> 'PENDING_REFERENCE' THEN
        RETURN wager_replay(t, NULL, false);
    END IF;

    SELECT * INTO ref FROM wager_resolve_reference(
        t.provider_id, t.reference_external_id, t.kind,
        t.player_id, t.wallet_id, t.round_id, t.amount_minor_units, t.currency);
    IF ref.o_decision = 'WAIT' AND NOT p_give_up THEN
        UPDATE transactions
           SET reference_attempts = reference_attempts + 1,
               reference_next_attempt_at = clock_timestamp() + p_retry_delay
         WHERE id = p_transaction_id;
        r.outcome := 'APPLIED';
        r.transaction_id := t.id;
        r.status := 'PENDING_REFERENCE';
        r.raced := false;
        RETURN r;
    ELSIF ref.o_decision = 'WAIT' THEN
        v_status := 'REJECTED';
        v_failure := 'REFERENCE_NOT_FOUND_TIMEOUT';
    ELSIF ref.o_decision = 'REJECT' THEN
        v_status := 'REJECTED';
        v_failure := ref.o_failure_code;
    ELSE
        SELECT * INTO w FROM wallets WHERE id = t.wallet_id FOR NO KEY UPDATE;
        IF ref.o_direction = 'DEBIT' AND w.balance_minor_units < t.amount_minor_units THEN
            v_status := 'REJECTED';
            v_failure := 'ROLLBACK_INSUFFICIENT_BALANCE';
        ELSE
            v_direction := ref.o_direction;
        END IF;
    END IF;

    v_now := clock_timestamp();
    UPDATE transactions
       SET status = v_status,
           failure_code = v_failure,
           reference_id = ref.o_original_id::text,
           reference_next_attempt_at = NULL,
           updated_at = v_now
     WHERE id = p_transaction_id;

    RETURN wager_finish(
        t.id, p_entry_id, p_event_id, p_balance_event_id,
        t.wallet_id, t.kind, t.amount_minor_units, t.currency, t.reference_external_id,
        v_status, v_failure, v_direction, NULL, t.id::text, v_now);
END
$$;
