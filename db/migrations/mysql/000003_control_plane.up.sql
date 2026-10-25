-- Control-plane auth, platform administration and index-job sources (M4, PLAN.md R7).

-- Ed25519 signing keys. The private key is sealed with SECRETS_MASTER_KEY (AES-256-GCM);
-- retired keys stay in the JWKS until publish_until so outstanding tokens still verify.
CREATE TABLE signing_keys (
  kid            VARCHAR(32)    NOT NULL,
  public_key     VARBINARY(32)  NOT NULL,
  private_sealed VARBINARY(256) NOT NULL,
  status         ENUM('active','retired','revoked') NOT NULL,
  created_at     DATETIME(6)    NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  retired_at     DATETIME(6)    NULL,
  publish_until  DATETIME(6)    NULL,
  PRIMARY KEY (kid),
  KEY ix_signing_keys_status (status)
);

-- Rotating refresh tokens. A token is used once; reusing a rotated token revokes its family.
CREATE TABLE refresh_tokens (
  id           CHAR(26)     NOT NULL,
  family_id    CHAR(26)     NOT NULL,
  subject_type ENUM('tenant_user','platform_admin') NOT NULL,
  subject_id   CHAR(26)     NOT NULL,
  tenant_id    CHAR(26)     NULL,
  token_hash   BINARY(32)   NOT NULL,
  expires_at   DATETIME(6)  NOT NULL,
  rotated_at   DATETIME(6)  NULL,
  revoked_at   DATETIME(6)  NULL,
  created_at   DATETIME(6)  NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  PRIMARY KEY (id),
  UNIQUE KEY uq_refresh_tokens_hash (token_hash),
  KEY ix_refresh_tokens_family (family_id)
);

CREATE TABLE email_verifications (
  token_hash  BINARY(32)  NOT NULL,
  user_id     CHAR(26)    NOT NULL,
  expires_at  DATETIME(6) NOT NULL,
  used_at     DATETIME(6) NULL,
  created_at  DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  PRIMARY KEY (token_hash),
  KEY ix_email_verifications_user (user_id),
  CONSTRAINT fk_email_verifications_user FOREIGN KEY (user_id) REFERENCES tenant_users (id) ON DELETE CASCADE
);

CREATE TABLE platform_admins (
  id             CHAR(26)     NOT NULL,
  email          VARCHAR(320) NOT NULL,
  password_hash  VARCHAR(255) NOT NULL,
  created_at     DATETIME(6)  NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  PRIMARY KEY (id),
  UNIQUE KEY uq_platform_admins_email (email)
);

-- One typed JSON document per settings section, validated against bounds by control-api.
CREATE TABLE platform_settings (
  section     VARCHAR(64) NOT NULL,
  document    JSON        NOT NULL,
  updated_at  DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
  updated_by  CHAR(26)    NULL,
  PRIMARY KEY (section)
);

ALTER TABLE index_jobs
  ADD COLUMN source        ENUM('topic','upload') NOT NULL DEFAULT 'topic' AFTER config_version,
  ADD COLUMN upload_key    VARCHAR(512) NULL AFTER source,
  ADD COLUMN index_version INT NULL AFTER upload_key,
  ADD COLUMN created_at    DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  ADD KEY ix_index_jobs_queue (status, created_at);
