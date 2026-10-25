ALTER TABLE tenant_secrets DROP KEY ix_tenant_secrets_tenant_id;
ALTER TABLE tenant_connections DROP COLUMN updated_at;
ALTER TABLE tenant_models DROP COLUMN updated_at, DROP COLUMN last_error, DROP COLUMN settings;
