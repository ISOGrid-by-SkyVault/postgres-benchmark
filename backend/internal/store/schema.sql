-- Results database schema. Applied at every startup, so every statement must
-- be safe to run again.

CREATE TABLE IF NOT EXISTS benchmark_runs (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    status            text        NOT NULL,
    target_label      text        NOT NULL,
    topology          text        NOT NULL,
    node_count        integer     NOT NULL,
    server_version    text        NOT NULL,
    scale             integer     NOT NULL,
    concurrency       integer     NOT NULL,
    duration_seconds  integer     NOT NULL,
    current_action    text        NOT NULL DEFAULT '',
    completed_actions integer     NOT NULL DEFAULT 0,
    total_actions     integer     NOT NULL DEFAULT 0,
    error             text        NOT NULL DEFAULT '',
    started_at        timestamptz NOT NULL DEFAULT now(),
    finished_at       timestamptz
);

CREATE INDEX IF NOT EXISTS benchmark_runs_started_at_idx ON benchmark_runs (started_at DESC);

CREATE TABLE IF NOT EXISTS benchmark_results (
    id             bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    run_id         uuid             NOT NULL REFERENCES benchmark_runs (id) ON DELETE CASCADE,
    position       integer          NOT NULL,
    action         text             NOT NULL,
    title          text             NOT NULL,
    category       text             NOT NULL,
    node           text             NOT NULL,
    kind           text             NOT NULL,
    operations     bigint           NOT NULL,
    errors         bigint           NOT NULL,
    rows_affected  bigint           NOT NULL,
    duration_ms    double precision NOT NULL,
    ops_per_second double precision NOT NULL,
    latency_avg_ms double precision NOT NULL,
    latency_p50_ms double precision NOT NULL,
    latency_p95_ms double precision NOT NULL,
    latency_p99_ms double precision NOT NULL,
    latency_min_ms double precision NOT NULL,
    latency_max_ms double precision NOT NULL,
    error_sample   text             NOT NULL DEFAULT '',
    UNIQUE (run_id, position)
);
