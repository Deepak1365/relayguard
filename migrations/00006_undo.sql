-- Phase 6 step 6: admin undo.
-- 'undone' is a terminal inbox status: the worker only claims 'pending', so an
-- undone event is never picked up again, and preview/replay refuse it.
-- admin_actions_undo_once makes "undo the same event twice" impossible at the
-- database level, even if two undos raced past the application's checks.

-- +goose Up
ALTER TABLE inbox DROP CONSTRAINT inbox_status_check;
ALTER TABLE inbox ADD CONSTRAINT inbox_status_check
    CHECK (status IN ('pending', 'done', 'skipped', 'dead', 'stale', 'undone'));

CREATE UNIQUE INDEX admin_actions_undo_once ON admin_actions (inbox_id)
    WHERE action = 'undo' AND outcome = 'done';

-- +goose Down
-- Lossy, like 00003's down: an undone event becomes 'done' again although its
-- effect was reverted (its undo change_log rows and audit row remain).
-- After down and up again, that remaining 'undo'/'done' audit row still makes
-- Undo refuse it ("already undone") and Replay refuse it (undone guard), so
-- the reverted effect cannot be applied or undone a second time.
DROP INDEX admin_actions_undo_once;
UPDATE inbox SET status = 'done' WHERE status = 'undone';
ALTER TABLE inbox DROP CONSTRAINT inbox_status_check;
ALTER TABLE inbox ADD CONSTRAINT inbox_status_check
    CHECK (status IN ('pending', 'done', 'skipped', 'dead', 'stale'));
