-- Leave a fifth of every wallets page free, so the new version of a row
-- that a balance update writes fits on the same page. No indexed column
-- changes on such an update, so PostgreSQL then writes no index entry at
-- all (a HOT update).
ALTER TABLE wallets SET (fillfactor = 80);

-- The unique (player_id, currency) index already serves lookups by player.
DROP INDEX idx_wallets_player_id;
