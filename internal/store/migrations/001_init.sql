CREATE TABLE monitors (
    id                    BIGSERIAL PRIMARY KEY,
    name                  TEXT        NOT NULL CHECK (length(btrim(name)) BETWEEN 1 AND 100),
    url                   TEXT        NOT NULL CHECK (url ~ '^https?://'),
    method                TEXT        NOT NULL DEFAULT 'GET' CHECK (method IN ('GET', 'HEAD')),
    interval_seconds      INTEGER     NOT NULL CHECK (interval_seconds BETWEEN 30 AND 86400),
    timeout_seconds       INTEGER     NOT NULL CHECK (timeout_seconds BETWEEN 1 AND 30),
    expected_status_min   INTEGER     NOT NULL DEFAULT 200,
    expected_status_max   INTEGER     NOT NULL DEFAULT 399,
    failure_threshold     INTEGER     NOT NULL DEFAULT 2 CHECK (failure_threshold BETWEEN 1 AND 10),
    paused                BOOLEAN     NOT NULL DEFAULT FALSE,
    status                TEXT        NOT NULL DEFAULT 'UNKNOWN' CHECK (status IN ('UNKNOWN', 'UP', 'DOWN')),
    consecutive_failures  INTEGER     NOT NULL DEFAULT 0 CHECK (consecutive_failures >= 0),
    last_checked_at       TIMESTAMPTZ,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT monitors_timeout_below_interval CHECK (timeout_seconds < interval_seconds),
    CONSTRAINT monitors_status_range CHECK (
        expected_status_min BETWEEN 100 AND 599
        AND expected_status_max BETWEEN 100 AND 599
        AND expected_status_min <= expected_status_max)
);

-- One row per check. The primary key doubles as the index for "results of a monitor
-- in a time range", which is what every stats query asks.
CREATE TABLE check_results (
    monitor_id   BIGINT      NOT NULL REFERENCES monitors (id) ON DELETE CASCADE,
    checked_at   TIMESTAMPTZ NOT NULL,
    ok           BOOLEAN     NOT NULL,
    status_code  INTEGER,
    latency_ms   INTEGER     NOT NULL CHECK (latency_ms >= 0),
    error        TEXT,
    PRIMARY KEY (monitor_id, checked_at)
);
-- Retention deletes by age across all monitors
CREATE INDEX check_results_checked_at_idx ON check_results (checked_at);

CREATE TABLE incidents (
    id           BIGSERIAL PRIMARY KEY,
    monitor_id   BIGINT      NOT NULL REFERENCES monitors (id) ON DELETE CASCADE,
    opened_at    TIMESTAMPTZ NOT NULL,
    resolved_at  TIMESTAMPTZ,
    cause        TEXT        NOT NULL,
    CONSTRAINT incidents_resolved_after_opened CHECK (resolved_at IS NULL OR resolved_at >= opened_at)
);
-- The database, not the code, guarantees a monitor never has two open incidents
CREATE UNIQUE INDEX incidents_one_open_per_monitor ON incidents (monitor_id) WHERE resolved_at IS NULL;
CREATE INDEX incidents_opened_at_idx ON incidents (opened_at DESC);
