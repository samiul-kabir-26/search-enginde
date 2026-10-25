ALTER TABLE index_jobs DROP KEY ix_index_jobs_queue, DROP COLUMN created_at, DROP COLUMN index_version,
  DROP COLUMN upload_key, DROP COLUMN source;
DROP TABLE platform_settings;
DROP TABLE platform_admins;
DROP TABLE email_verifications;
DROP TABLE refresh_tokens;
DROP TABLE signing_keys;
