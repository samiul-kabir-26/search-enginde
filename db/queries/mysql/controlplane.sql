-- name: CreateUser :exec
INSERT INTO tenant_users (id, tenant_id, email, password_hash, role) VALUES (?, ?, ?, ?, ?);

-- name: GetUserByEmail :one
SELECT * FROM tenant_users WHERE tenant_id = ? AND email = ?;

-- name: GetUser :one
SELECT * FROM tenant_users WHERE id = ?;

-- name: ListUsers :many
SELECT * FROM tenant_users WHERE tenant_id = ? ORDER BY created_at, id;

-- name: UpdateUserRole :execrows
UPDATE tenant_users SET role = ? WHERE id = ? AND tenant_id = ?;

-- name: UpdateUserPassword :exec
UPDATE tenant_users SET password_hash = ? WHERE id = ?;

-- name: DeleteUser :execrows
DELETE FROM tenant_users WHERE id = ? AND tenant_id = ?;

-- name: MarkEmailVerified :exec
UPDATE tenant_users SET email_verified_at = CURRENT_TIMESTAMP(6) WHERE id = ? AND email_verified_at IS NULL;

-- name: CountOwners :one
SELECT COUNT(*) FROM tenant_users WHERE tenant_id = ? AND role = 'owner';

-- name: CreateEmailVerification :exec
INSERT INTO email_verifications (token_hash, user_id, expires_at) VALUES (?, ?, ?);

-- name: GetEmailVerification :one
SELECT * FROM email_verifications WHERE token_hash = ?;

-- name: UseEmailVerification :execrows
UPDATE email_verifications SET used_at = CURRENT_TIMESTAMP(6) WHERE token_hash = ? AND used_at IS NULL;

-- name: CreateRefreshToken :exec
INSERT INTO refresh_tokens (id, family_id, subject_type, subject_id, tenant_id, token_hash, expires_at)
VALUES (?, ?, ?, ?, ?, ?, ?);

-- name: GetRefreshToken :one
SELECT * FROM refresh_tokens WHERE token_hash = ?;

-- name: RotateRefreshToken :execrows
UPDATE refresh_tokens SET rotated_at = CURRENT_TIMESTAMP(6) WHERE id = ? AND rotated_at IS NULL AND revoked_at IS NULL;

-- name: RevokeRefreshFamily :exec
UPDATE refresh_tokens SET revoked_at = CURRENT_TIMESTAMP(6) WHERE family_id = ? AND revoked_at IS NULL;

-- name: InsertSigningKey :exec
INSERT INTO signing_keys (kid, public_key, private_sealed, status) VALUES (?, ?, ?, 'active');

-- name: ListSigningKeys :many
SELECT * FROM signing_keys ORDER BY created_at DESC;

-- name: RetireSigningKey :exec
UPDATE signing_keys SET status = 'retired', retired_at = CURRENT_TIMESTAMP(6), publish_until = ? WHERE kid = ? AND status = 'active';

-- name: RevokeSigningKey :execrows
UPDATE signing_keys SET status = 'revoked', publish_until = CURRENT_TIMESTAMP(6) WHERE kid = ? AND status <> 'revoked';

-- name: CreatePlatformAdmin :exec
INSERT INTO platform_admins (id, email, password_hash) VALUES (?, ?, ?);

-- name: GetPlatformAdminByEmail :one
SELECT * FROM platform_admins WHERE email = ?;

-- name: UpdatePlatformAdminPassword :exec
UPDATE platform_admins SET password_hash = ? WHERE id = ?;

-- name: ListPlatformSettings :many
SELECT * FROM platform_settings;

-- name: UpsertPlatformSetting :exec
INSERT INTO platform_settings (section, document, updated_by) VALUES (?, ?, ?)
ON DUPLICATE KEY UPDATE document = VALUES(document), updated_by = VALUES(updated_by);

-- name: ListPlans :many
SELECT * FROM plans ORDER BY max_total_ms, id;

-- name: UpsertPlan :exec
INSERT INTO plans (id, name, max_total_ms, search_rps, search_burst, events_rps, byo_allowed, egress_allowlist, max_custom_fields, max_page_depth)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON DUPLICATE KEY UPDATE name = VALUES(name), max_total_ms = VALUES(max_total_ms), search_rps = VALUES(search_rps),
  search_burst = VALUES(search_burst), events_rps = VALUES(events_rps), byo_allowed = VALUES(byo_allowed),
  egress_allowlist = VALUES(egress_allowlist), max_custom_fields = VALUES(max_custom_fields), max_page_depth = VALUES(max_page_depth);

-- name: ListTenants :many
SELECT * FROM tenants ORDER BY created_at DESC, id LIMIT ? OFFSET ?;

-- name: ListAPIKeys :many
SELECT * FROM tenant_api_keys WHERE tenant_id = ? ORDER BY created_at DESC, id;

-- name: RevokeAPIKey :execrows
UPDATE tenant_api_keys SET revoked_at = CURRENT_TIMESTAMP(6) WHERE id = ? AND tenant_id = ? AND revoked_at IS NULL;

-- name: GetConfigDraft :one
SELECT * FROM tenant_config_drafts WHERE tenant_id = ?;

-- name: ListConfigVersions :many
SELECT id, tenant_id, version, schema_version, state, shadow_sample_rate, created_at, created_by
FROM tenant_config_versions WHERE tenant_id = ? ORDER BY version DESC;

-- name: GetConfigVersion :one
SELECT * FROM tenant_config_versions WHERE tenant_id = ? AND version = ?;

-- name: LockMaxConfigVersion :one
SELECT CAST(COALESCE(MAX(version), 0) AS SIGNED) AS max_version FROM tenant_config_versions WHERE tenant_id = ? FOR UPDATE;

-- name: SetConfigVersionState :execrows
UPDATE tenant_config_versions SET state = ?, shadow_sample_rate = ? WHERE tenant_id = ? AND version = ?;

-- name: RetireActiveConfig :exec
UPDATE tenant_config_versions SET state = 'retired', shadow_sample_rate = NULL WHERE tenant_id = ? AND state = 'active';

-- name: ClearShadowConfig :exec
UPDATE tenant_config_versions SET state = 'published', shadow_sample_rate = NULL WHERE tenant_id = ? AND state = 'shadow';

-- name: GetShadowConfigVersion :one
SELECT * FROM tenant_config_versions WHERE tenant_id = ? AND state = 'shadow';

-- name: ListLiveConfigVersions :many
SELECT tenant_id, version, state, shadow_sample_rate FROM tenant_config_versions WHERE state IN ('active', 'shadow');

-- name: GetOnboardingState :one
SELECT * FROM onboarding_state WHERE tenant_id = ?;

-- name: UpdateOnboardingState :exec
UPDATE onboarding_state SET current_step = ?, completed_steps = ? WHERE tenant_id = ?;

-- name: CreateIndexJob :exec
INSERT INTO index_jobs (id, tenant_id, config_version, source, upload_key, status) VALUES (?, ?, ?, ?, ?, 'queued');

-- name: GetIndexJob :one
SELECT * FROM index_jobs WHERE id = ? AND tenant_id = ?;

-- name: ListIndexJobs :many
SELECT * FROM index_jobs WHERE tenant_id = ? ORDER BY created_at DESC, id LIMIT 50;

-- name: NextQueuedIndexJob :one
SELECT * FROM index_jobs WHERE status = 'queued' ORDER BY created_at, id LIMIT 1 FOR UPDATE SKIP LOCKED;

-- name: StartIndexJob :execrows
UPDATE index_jobs SET status = 'running', started_at = CURRENT_TIMESTAMP(6) WHERE id = ? AND status = 'queued';

-- name: UpdateIndexJobProgress :exec
UPDATE index_jobs SET total = ?, done = ?, failed = ? WHERE id = ?;

-- name: FinishIndexJob :exec
UPDATE index_jobs SET status = ?, error = ?, index_version = ?, finished_at = CURRENT_TIMESTAMP(6) WHERE id = ?;

-- name: GetPlatformAdmin :one
SELECT * FROM platform_admins WHERE id = ?;

-- name: CountPlatformAdmins :one
SELECT COUNT(*) FROM platform_admins;
