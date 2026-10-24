-- Control-plane schema (docs/TENANT_CONFIG.md §2). IDs are ULIDs (CHAR(26)).
-- Every tenant-scoped table leads its indexes with tenant_id.

CREATE TABLE tenants (
  id          CHAR(26)     NOT NULL,
  slug        VARCHAR(63)  NOT NULL,
  name        VARCHAR(255) NOT NULL,
  plan        VARCHAR(32)  NOT NULL,
  region      VARCHAR(32)  NOT NULL,
  status      VARCHAR(32)  NOT NULL,
  created_at  DATETIME(6)  NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  updated_at  DATETIME(6)  NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
  PRIMARY KEY (id),
  UNIQUE KEY uq_tenants_slug (slug)
);

CREATE TABLE tenant_users (
  id             CHAR(26)     NOT NULL,
  tenant_id      CHAR(26)     NOT NULL,
  email          VARCHAR(320) NOT NULL,
  password_hash  VARCHAR(255) NOT NULL,
  role           ENUM('owner','admin','developer','viewer') NOT NULL,
  email_verified_at DATETIME(6) NULL,
  created_at     DATETIME(6)  NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  updated_at     DATETIME(6)  NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
  PRIMARY KEY (id),
  UNIQUE KEY uq_tenant_users_email (tenant_id, email),
  CONSTRAINT fk_tenant_users_tenant FOREIGN KEY (tenant_id) REFERENCES tenants (id)
);

CREATE TABLE tenant_api_keys (
  id            CHAR(26)    NOT NULL,
  tenant_id     CHAR(26)    NOT NULL,
  prefix        VARCHAR(16) NOT NULL,
  hash          VARBINARY(64) NOT NULL,
  scopes        JSON        NOT NULL,
  created_at    DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  last_used_at  DATETIME(6) NULL,
  revoked_at    DATETIME(6) NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uq_tenant_api_keys_prefix (prefix),
  KEY ix_tenant_api_keys_tenant (tenant_id),
  CONSTRAINT fk_tenant_api_keys_tenant FOREIGN KEY (tenant_id) REFERENCES tenants (id)
);

CREATE TABLE tenant_config_drafts (
  tenant_id       CHAR(26)    NOT NULL,
  document        JSON        NOT NULL,
  schema_version  INT         NOT NULL,
  updated_at      DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
  updated_by      CHAR(26)    NULL,
  PRIMARY KEY (tenant_id),
  CONSTRAINT fk_tenant_config_drafts_tenant FOREIGN KEY (tenant_id) REFERENCES tenants (id)
);

CREATE TABLE tenant_config_versions (
  id              CHAR(26)    NOT NULL,
  tenant_id       CHAR(26)    NOT NULL,
  version         INT         NOT NULL,
  document        JSON        NOT NULL,
  schema_version  INT         NOT NULL,
  state           ENUM('published','active','shadow','retired') NOT NULL,
  shadow_sample_rate DECIMAL(4,3) NULL,
  created_at      DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  created_by      CHAR(26)    NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uq_tenant_config_versions (tenant_id, version),
  KEY ix_tenant_config_versions_state (tenant_id, state),
  CONSTRAINT fk_tenant_config_versions_tenant FOREIGN KEY (tenant_id) REFERENCES tenants (id)
);

CREATE TABLE tenant_secrets (
  id           CHAR(26)     NOT NULL,
  tenant_id    CHAR(26)     NOT NULL,
  kind         VARCHAR(64)  NOT NULL,
  ciphertext   VARBINARY(8192) NOT NULL,
  dek_wrapped  VARBINARY(512)  NOT NULL,
  created_at   DATETIME(6)  NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  rotated_at   DATETIME(6)  NULL,
  PRIMARY KEY (id),
  KEY ix_tenant_secrets_tenant (tenant_id),
  CONSTRAINT fk_tenant_secrets_tenant FOREIGN KEY (tenant_id) REFERENCES tenants (id)
);

CREATE TABLE tenant_models (
  id               CHAR(26)     NOT NULL,
  tenant_id        CHAR(26)     NOT NULL,
  kind             ENUM('embedder','prerank','rank','rerank') NOT NULL,
  type             ENUM('onnx','kserve_v2','openai_embeddings') NOT NULL,
  name             VARCHAR(255) NOT NULL,
  version          VARCHAR(64)  NOT NULL,
  artifact_uri     VARCHAR(1024) NULL,
  artifact_sha256  CHAR(64)     NULL,
  endpoint         VARCHAR(1024) NULL,
  secret_ref       VARCHAR(64)  NULL,
  input_schema     JSON         NOT NULL,
  output_schema    JSON         NOT NULL,
  status           VARCHAR(32)  NOT NULL,
  validated_at     DATETIME(6)  NULL,
  created_at       DATETIME(6)  NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  PRIMARY KEY (id),
  KEY ix_tenant_models_tenant (tenant_id, kind),
  CONSTRAINT fk_tenant_models_tenant FOREIGN KEY (tenant_id) REFERENCES tenants (id)
);

CREATE TABLE tenant_connections (
  id              CHAR(26)     NOT NULL,
  tenant_id       CHAR(26)     NOT NULL,
  kind            ENUM('vector_store','lexical') NOT NULL,
  type            VARCHAR(32)  NOT NULL,
  endpoint        VARCHAR(1024) NOT NULL,
  secret_ref      VARCHAR(64)  NULL,
  settings        JSON         NOT NULL,
  status          VARCHAR(32)  NOT NULL,
  last_tested_at  DATETIME(6)  NULL,
  last_error      TEXT         NULL,
  created_at      DATETIME(6)  NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  PRIMARY KEY (id),
  KEY ix_tenant_connections_tenant (tenant_id, kind),
  CONSTRAINT fk_tenant_connections_tenant FOREIGN KEY (tenant_id) REFERENCES tenants (id)
);

CREATE TABLE onboarding_state (
  tenant_id        CHAR(26)    NOT NULL,
  current_step     VARCHAR(32) NOT NULL,
  completed_steps  JSON        NOT NULL,
  updated_at       DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
  PRIMARY KEY (tenant_id),
  CONSTRAINT fk_onboarding_state_tenant FOREIGN KEY (tenant_id) REFERENCES tenants (id)
);

CREATE TABLE index_jobs (
  id              CHAR(26)    NOT NULL,
  tenant_id       CHAR(26)    NOT NULL,
  config_version  INT         NOT NULL,
  status          VARCHAR(32) NOT NULL,
  total           INT         NOT NULL DEFAULT 0,
  done            INT         NOT NULL DEFAULT 0,
  failed          INT         NOT NULL DEFAULT 0,
  started_at      DATETIME(6) NULL,
  finished_at     DATETIME(6) NULL,
  error           TEXT        NULL,
  PRIMARY KEY (id),
  KEY ix_index_jobs_tenant (tenant_id, status),
  CONSTRAINT fk_index_jobs_tenant FOREIGN KEY (tenant_id) REFERENCES tenants (id)
);

CREATE TABLE ad_campaigns (
  id                  CHAR(26)    NOT NULL,
  tenant_id           CHAR(26)    NOT NULL,
  vendor_id           VARCHAR(64) NOT NULL,
  status              VARCHAR(32) NOT NULL,
  daily_budget_minor  BIGINT      NOT NULL,
  currency            CHAR(3)     NOT NULL,
  bid_minor           BIGINT      NOT NULL,
  targeting           JSON        NOT NULL,
  start_at            DATETIME(6) NOT NULL,
  end_at              DATETIME(6) NULL,
  PRIMARY KEY (id),
  KEY ix_ad_campaigns_tenant (tenant_id, status),
  CONSTRAINT fk_ad_campaigns_tenant FOREIGN KEY (tenant_id) REFERENCES tenants (id)
);

CREATE TABLE ad_creatives (
  id           CHAR(26)     NOT NULL,
  campaign_id  CHAR(26)     NOT NULL,
  product_id   VARCHAR(128) NOT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uq_ad_creatives (campaign_id, product_id),
  CONSTRAINT fk_ad_creatives_campaign FOREIGN KEY (campaign_id) REFERENCES ad_campaigns (id) ON DELETE CASCADE
);
