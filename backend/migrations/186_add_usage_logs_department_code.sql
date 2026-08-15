-- Add the immutable department snapshot used by financial showback.
--
-- PostgreSQL 11+ stores a constant DEFAULT in table metadata, so this does not
-- rewrite existing rows. ADD COLUMN still takes a brief ACCESS EXCLUSIVE lock;
-- expected execution is under one second once concurrent long transactions are
-- drained. No downtime is expected, but production should use a short lock
-- timeout and retry during a quiet window.
--
-- On a non-partitioned usage_logs table this changes only the table metadata.
-- On a partitioned usage_logs parent PostgreSQL propagates the column to every
-- partition; lock acquisition can therefore take longer with many partitions.
-- Existing rows read as 'unknown', preserving an explicit historical boundary.
ALTER TABLE usage_logs
    ADD COLUMN IF NOT EXISTS department_code VARCHAR(100)
    NOT NULL DEFAULT 'unknown';
