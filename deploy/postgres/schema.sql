-- S3Lite control plane schema (PostgreSQL)
-- Apply with:
--   psql -h <host> -U s3lite -d s3lite -f schema.sql

CREATE TABLE IF NOT EXISTS nodes (
    node_id          TEXT PRIMARY KEY,
    capacity_bytes   BIGINT NOT NULL DEFAULT 0,
    zone             TEXT NOT NULL DEFAULT '',
    ip               TEXT NOT NULL DEFAULT '',
    os               TEXT NOT NULL DEFAULT '',
    arch             TEXT NOT NULL DEFAULT '',
    rack             TEXT NOT NULL DEFAULT '',
    volume_port      INTEGER NOT NULL DEFAULT 0,
    friendly_name    TEXT NOT NULL DEFAULT '',
    stability        TEXT NOT NULL DEFAULT 'unstable',
    registered_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_heartbeat   TIMESTAMPTZ,
    alive            BOOLEAN NOT NULL DEFAULT FALSE,
    status           TEXT NOT NULL DEFAULT 'offline',
    disk_free        BIGINT NOT NULL DEFAULT 0,
    disk_used        BIGINT NOT NULL DEFAULT 0,
    volumes_count    INTEGER NOT NULL DEFAULT 0,
    ec_shards_count  INTEGER NOT NULL DEFAULT 0
);

-- Placement history: shard layout per (collection, volume_id, node).
CREATE TABLE IF NOT EXISTS ec_placement (
    collection TEXT NOT NULL,
    volume_id  INTEGER NOT NULL,
    data_shards INTEGER NOT NULL DEFAULT 6,
    parity_shards INTEGER NOT NULL DEFAULT 4,
    node_addr  TEXT NOT NULL,
    shard_indices INTEGER[] NOT NULL,
    observed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (collection, volume_id, node_addr)
);

CREATE INDEX IF NOT EXISTS idx_ec_placement_volume
    ON ec_placement (collection, volume_id);
