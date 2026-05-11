-- 025_notification_delivery_log.sql — Round-7 Task 5.
-- Per-attempt log of webhook deliveries, used by the retry worker
-- and the GET /v1/admin/notifications/delivery-log endpoint.

CREATE TABLE IF NOT EXISTS notification_delivery_log (
    id                 CHAR(26)     NOT NULL,
    tenant_id          CHAR(26)     NOT NULL,
    preference_id      CHAR(26)     NOT NULL,
    event_type         VARCHAR(64)  NOT NULL,
    channel            VARCHAR(16)  NOT NULL,
    target             TEXT         NOT NULL,
    payload            JSONB        NOT NULL DEFAULT '{}',
    status             VARCHAR(16)  NOT NULL DEFAULT 'pending',
    attempt            INTEGER      NOT NULL DEFAULT 0,
    response_code      INTEGER      NOT NULL DEFAULT 0,
    error_message      TEXT         NOT NULL DEFAULT '',
    next_retry_at      TIMESTAMPTZ,
    created_at         TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ  NOT NULL DEFAULT now(),
    PRIMARY KEY (id)
);

CREATE INDEX IF NOT EXISTS idx_notif_delivery_tenant_time
    ON notification_delivery_log (tenant_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_notif_delivery_status
    ON notification_delivery_log (status, next_retry_at);
