-- Daily account-cost calibration against upstream ledgers.
--
-- The dashboard account cost is computed from usage_logs, which only records
-- requests we completed. Upstreams also bill attempts we abandoned (response
-- header timeouts before failover, client disconnects), so the dashboard
-- under-reports real cost. For upstreams that expose a per-key daily ledger
-- (Sub2API-compatible GET /v1/usage), a background job stores one row per
-- (day, upstream key): adjustment = upstream actual cost - our account cost.
-- The dashboard adds the adjustment on top of its usage_logs-based cost.
--
-- key_hash is SHA-256 of the upstream API key; the key itself is never stored.
-- status: 'ok' (adjustment applied) or 'misaligned' (upstream reported fewer
-- requests than we logged, so the day is not comparable; adjustment = 0).
CREATE TABLE IF NOT EXISTS usage_cost_calibrations (
    bucket_date          DATE           NOT NULL,
    key_hash             VARCHAR(64)    NOT NULL,
    account_ids          TEXT           NOT NULL,
    our_requests         BIGINT         NOT NULL DEFAULT 0,
    upstream_requests    BIGINT         NOT NULL DEFAULT 0,
    our_account_cost     NUMERIC(20,10) NOT NULL DEFAULT 0,
    upstream_actual_cost NUMERIC(20,10) NOT NULL DEFAULT 0,
    adjustment           NUMERIC(20,10) NOT NULL DEFAULT 0,
    status               VARCHAR(16)    NOT NULL,
    computed_at          TIMESTAMPTZ    NOT NULL DEFAULT NOW(),
    PRIMARY KEY (bucket_date, key_hash)
);
