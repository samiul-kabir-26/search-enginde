-- name: CreateTenantSecret :exec
INSERT INTO tenant_secrets (id, tenant_id, kind, ciphertext, dek_wrapped) VALUES (?, ?, ?, ?, ?);

-- name: GetTenantSecret :one
SELECT id, tenant_id, kind, ciphertext, dek_wrapped, created_at, rotated_at
FROM tenant_secrets WHERE tenant_id = ? AND id = ?;

-- name: CreateTenantModel :exec
INSERT INTO tenant_models (id, tenant_id, kind, type, name, version, artifact_uri, artifact_sha256, endpoint,
  secret_ref, input_schema, output_schema, settings, status, validated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: GetTenantModel :one
SELECT * FROM tenant_models WHERE tenant_id = ? AND id = ?;

-- name: ListTenantModels :many
SELECT * FROM tenant_models WHERE tenant_id = ? ORDER BY created_at DESC, id DESC LIMIT ?;

-- name: SetTenantModelValidation :exec
UPDATE tenant_models SET status = ?, settings = ?, validated_at = ?, last_error = ?
WHERE tenant_id = ? AND id = ?;

-- name: CreateTenantConnection :exec
INSERT INTO tenant_connections (id, tenant_id, kind, type, endpoint, secret_ref, settings, status)
VALUES (?, ?, ?, ?, ?, ?, ?, 'untested');

-- name: GetTenantConnection :one
SELECT * FROM tenant_connections WHERE tenant_id = ? AND id = ?;

-- name: ListTenantConnections :many
SELECT * FROM tenant_connections WHERE tenant_id = ? ORDER BY created_at DESC, id DESC LIMIT ?;

-- name: UpdateTenantConnection :execrows
UPDATE tenant_connections SET kind = ?, type = ?, endpoint = ?, secret_ref = ?, settings = ?,
  status = 'untested', last_tested_at = NULL, last_error = NULL
WHERE tenant_id = ? AND id = ?;

-- name: DeleteTenantConnection :execrows
DELETE FROM tenant_connections WHERE tenant_id = ? AND id = ?;

-- name: SetTenantConnectionStatus :exec
UPDATE tenant_connections SET status = ?, last_tested_at = ?, last_error = ? WHERE tenant_id = ? AND id = ?;

-- name: ListVersionsReferencing :many
SELECT version, state FROM tenant_config_versions
WHERE tenant_id = ? AND state <> 'retired' AND JSON_SEARCH(document, 'one', sqlc.arg(ref)) IS NOT NULL
ORDER BY version;

-- name: DraftReferences :one
SELECT COUNT(*) FROM tenant_config_drafts
WHERE tenant_id = ? AND JSON_SEARCH(document, 'one', sqlc.arg(ref)) IS NOT NULL;
