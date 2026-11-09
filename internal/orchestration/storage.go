-- 1. Persistent Orchestration Jobs
CREATE TABLE IF NOT EXISTS orchestration_jobs (
    id TEXT PRIMARY KEY,
    task TEXT NOT NULL,
    status TEXT NOT NULL,
    max_rounds INTEGER NOT NULL DEFAULT 5,
    max_calls INTEGER NOT NULL DEFAULT 12,
    current_round INTEGER NOT NULL DEFAULT 0,
    total_calls INTEGER NOT NULL DEFAULT 0,
    retry_count INTEGER NOT NULL DEFAULT 0,
    max_retries INTEGER NOT NULL DEFAULT 3,
    claimed_by TEXT,
    lease_expires_at DATETIME,
    metadata TEXT NOT NULL DEFAULT '{}',
    execution_result TEXT,
    error_message TEXT,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    started_at DATETIME,
    completed_at DATETIME
);
CREATE INDEX IF NOT EXISTS idx_orch_jobs_status_lease ON orchestration_jobs(status, lease_expires_at);

-- 2. Durable Orchestration Events History
CREATE TABLE IF NOT EXISTS orchestration_events (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    job_id TEXT NOT NULL REFERENCES orchestration_jobs(id) ON DELETE CASCADE,
    event_type TEXT NOT NULL,
    phase TEXT NOT NULL DEFAULT '',
    worker_id TEXT NOT NULL DEFAULT '',
    message TEXT NOT NULL DEFAULT '',
    payload TEXT NOT NULL DEFAULT '{}',
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX IF NOT EXISTS idx_orch_events_job ON orchestration_events(job_id, id ASC);

-- 3. Worker Leases and Heartbeats
CREATE TABLE IF NOT EXISTS orchestration_worker_leases (
    worker_id TEXT PRIMARY KEY,
    last_heartbeat DATETIME NOT NULL,
    active_job_id TEXT,
    status TEXT NOT NULL DEFAULT 'active'
);
