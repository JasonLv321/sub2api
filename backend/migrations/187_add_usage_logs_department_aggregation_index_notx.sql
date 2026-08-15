-- Build the showback aggregation index without blocking usage writes.
--
-- CREATE INDEX CONCURRENTLY takes roughly the time of two table scans. Typical
-- planning ranges are under 1 minute below 1M rows, 1-10 minutes for 1M-10M,
-- and 10-60+ minutes above 10M, depending on storage and write load. It needs
-- no downtime, but requires free disk near the final index size and retries
-- after removing any INVALID index left by an interrupted build.
--
-- This statement is for a non-partitioned usage_logs table. If production uses
-- the partition layout from migration 035, PostgreSQL cannot build the parent
-- partitioned index concurrently: build the same index concurrently on each
-- leaf partition first, then attach them to a parent index in a maintenance
-- migration. Confirm relkind before applying this file in that deployment.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_usage_logs_department_model_created
    ON usage_logs (created_at, department_code, requested_model);
