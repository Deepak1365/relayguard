-- Phase 6 step 5: replay generations.
-- replay_count is bumped by every admin replay (step 5b). The worker's claim
-- token is (replay_count, attempts): a replay resets attempts to 0, so
-- attempts alone could repeat a value an old, stalled claim still holds (ABA).
-- change_log.generation records replay_count at the time of the apply.
-- The change_log CHECKs were optional in 00004 (already applied, so not
-- edited); they make impossible numbering fail at write time.

-- +goose Up
ALTER TABLE inbox ADD COLUMN replay_count INTEGER NOT NULL DEFAULT 0
    CONSTRAINT inbox_replay_count_nonneg CHECK (replay_count >= 0);

ALTER TABLE change_log ADD CONSTRAINT change_log_generation_nonneg CHECK (generation >= 0);
ALTER TABLE change_log ADD CONSTRAINT change_log_seq_positive      CHECK (seq >= 1);

-- +goose Down
ALTER TABLE change_log DROP CONSTRAINT change_log_seq_positive;
ALTER TABLE change_log DROP CONSTRAINT change_log_generation_nonneg;
ALTER TABLE inbox DROP COLUMN replay_count;
