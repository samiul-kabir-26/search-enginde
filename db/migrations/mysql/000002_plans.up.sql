-- Plan definitions (PLAN.md R3). Editable from the platform admin panel (M4); tenants.plan
-- references a row here.
CREATE TABLE plans (
  id                 VARCHAR(32) NOT NULL,
  name               VARCHAR(64) NOT NULL,
  max_total_ms       INT         NOT NULL,
  search_rps         INT         NOT NULL,
  search_burst       INT         NOT NULL,
  events_rps         INT         NOT NULL,
  byo_allowed        BOOLEAN     NOT NULL,
  egress_allowlist   JSON        NOT NULL,
  max_custom_fields  INT         NOT NULL,
  max_page_depth     INT         NOT NULL,
  created_at         DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  updated_at         DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
  PRIMARY KEY (id)
);

INSERT INTO plans (id, name, max_total_ms, search_rps, search_burst, events_rps, byo_allowed, egress_allowlist, max_custom_fields, max_page_depth) VALUES
  ('starter',    'Starter',    300,  20,   40,  200, FALSE, JSON_ARRAY(), 20,  500),
  ('growth',     'Growth',     600, 100,  200, 1000, TRUE,  JSON_ARRAY(), 50, 1000),
  ('enterprise', 'Enterprise', 1000, 500, 1000, 5000, TRUE,  JSON_ARRAY(), 100, 1000);

ALTER TABLE tenants ADD CONSTRAINT fk_tenants_plan FOREIGN KEY (plan) REFERENCES plans (id);
