-- name: GetTenant :one
SELECT * FROM tenants WHERE id = ?;

-- name: GetTenantBySlug :one
SELECT * FROM tenants WHERE slug = ?;

-- name: GetActiveConfigVersion :one
SELECT * FROM tenant_config_versions
WHERE tenant_id = ? AND state = 'active';

-- name: ListActiveConfigVersions :many
SELECT tenant_id, version FROM tenant_config_versions
WHERE state = 'active';

-- name: CreateTenant :exec
INSERT INTO tenants (id, slug, name, plan, region, status)
VALUES (?, ?, ?, ?, ?, ?);

-- name: UpsertConfigDraft :exec
INSERT INTO tenant_config_drafts (tenant_id, document, schema_version, updated_by)
VALUES (?, ?, ?, ?)
ON DUPLICATE KEY UPDATE document = VALUES(document), schema_version = VALUES(schema_version), updated_by = VALUES(updated_by);

-- name: CreateConfigVersion :exec
INSERT INTO tenant_config_versions (id, tenant_id, version, document, schema_version, state, created_by)
VALUES (?, ?, ?, ?, ?, ?, ?);

-- name: CreateOnboardingState :exec
INSERT INTO onboarding_state (tenant_id, current_step, completed_steps)
VALUES (?, ?, ?);
