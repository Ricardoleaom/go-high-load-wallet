CREATE INDEX idx_wallets_player_id
    ON wallets (player_id);

ALTER TABLE wallets RESET (fillfactor);
