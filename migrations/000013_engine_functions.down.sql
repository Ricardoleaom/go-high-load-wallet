DROP FUNCTION wager_retry_reference(uuid, uuid, uuid, uuid, boolean, interval);
DROP FUNCTION wager_submit(
    uuid, uuid, uuid, uuid, text, text, text, text, uuid, uuid, text, text, text, bigint, text, text, text, interval
);
DROP FUNCTION wallet_open(uuid, uuid, text, bigint, uuid, uuid, uuid, uuid);
DROP FUNCTION wager_finish(
    uuid, uuid, uuid, uuid, uuid, text, bigint, text, text, text, text, text, bigint, text, timestamptz
);
DROP FUNCTION wager_resolve_reference(text, text, text, uuid, uuid, text, bigint, text);
DROP FUNCTION wager_lost_race(text, text, text, text);
DROP FUNCTION wager_answer(transactions, text, text, boolean);
DROP FUNCTION wager_find(text, text, text);
DROP FUNCTION wager_replay(transactions, text, boolean);
DROP FUNCTION wager_emit(uuid, text, uuid, text, uuid, timestamptz, jsonb);
DROP FUNCTION wallet_money_text(bigint);
DROP TYPE wager_result;
