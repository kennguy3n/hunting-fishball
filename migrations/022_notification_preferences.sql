-- 022_notification_preferences.sql — Round-6 Task 11.

CREATE TABLE IF NOT EXISTS notification_preferences (
    id          CHAR(26)    NOT NULL,
    tenant_id   CHAR(26)    NOT NULL,
    event_type  VARCHAR(64) NOT NULL,
    channel     VARCHAR(16) NOT NULL,
    target      TEXT        NOT NULL,
    enabled     BOOLEAN     NOT NULL DEFAULT TRUE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (id)
);

CREATE INDEX IF NOT EXISTS idx_notification_prefs_tenant_event
    ON notification_preferences (tenant_id, event_type) WHERE enabled = TRUE;
