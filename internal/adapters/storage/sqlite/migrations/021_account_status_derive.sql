-- migration 021: derive upstream account-status signals.
--
-- These four columns mirror state higgsfield reports on GET /user
-- (blocked_at / suspended_at / is_paused) and GET /workspaces/notice
-- (normalized to grace_status). They are DERIVED signals for the
-- replenish alerter — they deliberately do NOT gate pool eligibility
-- (status='active' remains the sole入池 gate). An operator decides
-- whether to pull a flagged account after seeing the alert.
--
-- grace_status holds a small normalized token (see
-- upstream.NormalizeNoticeStatus): "grace" / "enforcement" /
-- "access_lose" / "card_declined" / "backup_card", or "" for
-- marketing / hide notices. blocked_at / suspended_at store the raw
-- upstream timestamp string (empty when unset); non-empty = flagged.

ALTER TABLE accounts ADD COLUMN grace_status TEXT    NOT NULL DEFAULT '';
ALTER TABLE accounts ADD COLUMN blocked_at   TEXT    NOT NULL DEFAULT '';
ALTER TABLE accounts ADD COLUMN suspended_at TEXT    NOT NULL DEFAULT '';
ALTER TABLE accounts ADD COLUMN is_paused    INTEGER NOT NULL DEFAULT 0;

INSERT OR IGNORE INTO schema_versions (version, applied_at)
  VALUES (21, CURRENT_TIMESTAMP);
