-- Run in the database configured for quota statistics. No automatic migrations.
CREATE TABLE IF NOT EXISTS quota_samples
(
    provider LowCardinality(String),
    account String,
    at Int64 COMMENT 'UTC Unix epoch milliseconds at observation',
    remaining Float64 COMMENT 'Percentage points, 0 to 100 per account',
    resetAt Nullable(Int64),
    window String COMMENT 'Selected quota group/window identity',
    periodHours Nullable(Float64),
    received_at DateTime64(3, 'UTC') DEFAULT now64(3),
    CONSTRAINT valid_remaining CHECK isFinite(remaining) AND remaining >= 0 AND remaining <= 100,
    CONSTRAINT valid_at CHECK at > 0,
    CONSTRAINT valid_identity CHECK notEmpty(provider) AND notEmpty(account),
    CONSTRAINT valid_reset CHECK resetAt IS NULL OR resetAt > 0,
    CONSTRAINT valid_period CHECK periodHours IS NULL OR (isFinite(periodHours) AND periodHours >= 1 AND periodHours <= 8784)
)
ENGINE = ReplacingMergeTree(received_at)
PARTITION BY toYYYYMM(fromUnixTimestamp64Milli(at))
ORDER BY (provider, account, at)
TTL toDateTime(fromUnixTimestamp64Milli(at)) + INTERVAL 90 DAY DELETE;
