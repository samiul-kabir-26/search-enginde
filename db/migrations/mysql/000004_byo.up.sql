-- BYO providers (M5): model settings (transport, batch size, text-to-image) and the last
-- validation or test error. Status values: models registered|validated|failed, connections
-- untested|validated|failed.
ALTER TABLE tenant_models
  ADD COLUMN settings   JSON        NULL AFTER output_schema,
  ADD COLUMN last_error TEXT        NULL AFTER validated_at,
  ADD COLUMN updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6);

ALTER TABLE tenant_connections
  ADD COLUMN updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6);

ALTER TABLE tenant_secrets ADD KEY ix_tenant_secrets_tenant_id (tenant_id, id);
