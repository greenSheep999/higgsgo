-- Migration 017: mailbox credentials per registration row.
--
-- The registration flow needs Microsoft Graph OAuth2 credentials to
-- fetch each account's OTP verification email. In the higgsfield-register
-- Node project the credentials come from a mail list file where every
-- line is `email----password----oauth_client_id----refresh_token`.
--
-- Before this migration the ports.RegistrationRequest carried Email +
-- ProxyURL only; a global HIGGSGO_REGISTER_MAILBOX_CLIENT env was used
-- (wrong — every mailbox has its own OAuth2 credentials). This
-- migration adds two columns so the registrar can persist the
-- per-row credentials and forward them to the Node driver on each
-- Register() call.
--
-- Both columns are nullable — legacy rows (pre-P4-3d bulk import)
-- have no mailbox_* set, and the OAuth flow (oauth_source != '')
-- skips mailbox entirely.

ALTER TABLE registrations ADD COLUMN mailbox_client_id TEXT;
ALTER TABLE registrations ADD COLUMN mailbox_refresh_token TEXT;

INSERT OR IGNORE INTO schema_versions (version, applied_at) VALUES (17, CURRENT_TIMESTAMP);
