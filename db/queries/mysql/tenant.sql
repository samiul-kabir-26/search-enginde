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
