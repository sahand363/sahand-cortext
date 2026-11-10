package orchestration

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

var (
	ErrJobNotFound  = errors.New("orchestration job not found")
	ErrJobCancelled = errors.New("orchestration job has been cancelled")
)

type Storage struct {
	db *sql.DB
	mu sync.Mutex // guarantees serialized writes on connection
}

func NewStorage(db *sql.DB) *Storage {
	return &Storage{db: db}
}

func randomID(prefix string) string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%s-%s", prefix, hex.EncodeToString(b))
}

// InitSchema ensures tables and indexes exist idempotently
func (s *Storage) InitSchema() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	queries := []string{
		`CREATE TABLE IF NOT EXISTS orchestration_jobs (
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
		);`,
		`CREATE INDEX IF NOT EXISTS idx_orch_jobs_status_lease ON orchestration_jobs(status, lease_expires_at);`,
		`CREATE TABLE IF NOT EXISTS orchestration_events (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			job_id TEXT NOT NULL REFERENCES orchestration_jobs(id) ON DELETE CASCADE,
			event_type TEXT NOT NULL,
			phase TEXT NOT NULL DEFAULT '',
			worker_id TEXT NOT NULL DEFAULT '',
			message TEXT NOT NULL DEFAULT '',
			payload TEXT NOT NULL DEFAULT '{}',
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		);`,
		`CREATE INDEX IF NOT EXISTS idx_orch_events_job ON orchestration_events(job_id, id ASC);`,
		`CREATE TABLE IF NOT EXISTS orchestration_worker_leases (
			worker_id TEXT PRIMARY KEY,
			last_heartbeat DATETIME NOT NULL,
			active_job_id TEXT,
			status TEXT NOT NULL DEFAULT 'active'
		);`,
	}

	for _, q := range queries {
		if _, err := s.db.Exec(q); err != nil {
			return fmt.Errorf("failed executing schema query (%s): %w", q, err)
		}
	}
	return nil
}

// CreateJob enqueues a new job and records the job_created event atomically
func (s *Storage) CreateJob(ctx context.Context, task string, maxRounds, maxCalls int, metadata string) (*Job, error) {
	if strings.TrimSpace(task) == "" {
		return nil, errors.New("job task description cannot be empty")
	}
	if maxRounds <= 0 {
		maxRounds = 5
	}
	if maxCalls <= 0 {
		maxCalls = 12
	}
	if strings.TrimSpace(metadata) == "" {
		metadata = "{}"
	}

	id := randomID("orch")
	now := time.Now().UTC()

	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `
		INSERT INTO orchestration_jobs (id, task, status, max_rounds, max_calls, metadata, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		id, task, string(StatusQueued), maxRounds, maxCalls, metadata, now, now)
	if err != nil {
		return nil, err
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO orchestration_events (job_id, event_type, phase, message, payload, created_at)
		VALUES (?, ?, 'queued', 'Job created and entered execution queue', ?, ?)`,
		id, string(EventJobCreated), metadata, now)
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}

	return &Job{
		ID:        id,
		Task:      task,
		Status:    StatusQueued,
		MaxRounds: maxRounds,
		MaxCalls:  maxCalls,
		Metadata:  metadata,
		CreatedAt: now,
		UpdatedAt: now,
	}, nil
}

// ClaimNextJob atomically claims an available queued or expired job
func (s *Storage) ClaimNextJob(ctx context.Context, workerID string, leaseDuration time.Duration) (*Job, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()

	var jobID, prevWorker string
	var prevStatus JobStatus
	var isRecovery bool

	now := time.Now().UTC()

	// Select 1 eligible job using parameterized 'now' for sub-second precision
	err = tx.QueryRowContext(ctx, `
		SELECT id, status, coalesce(claimed_by, '')
		FROM orchestration_jobs
		WHERE status = 'queued'
		   OR (status IN ('running', 'waiting') AND lease_expires_at IS NOT NULL AND lease_expires_at < ?)
		ORDER BY created_at ASC
		LIMIT 1`, now).Scan(&jobID, &prevStatus, &prevWorker)

	if err == sql.ErrNoRows {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}

	if prevStatus != StatusQueued {
		isRecovery = true
	}

	leaseEnd := now.Add(leaseDuration)

	res, err := tx.ExecContext(ctx, `
		UPDATE orchestration_jobs
		SET status = 'running',
		    claimed_by = ?,
		    lease_expires_at = ?,
		    started_at = coalesce(started_at, ?),
		    updated_at = ?
		WHERE id = ? AND (status = 'queued' OR lease_expires_at IS NULL OR lease_expires_at <= ?)`,
		workerID, leaseEnd, now, now, jobID, now)
	if err != nil {
		return nil, false, err
	}
	if rows, _ := res.RowsAffected(); rows == 0 {
		return nil, false, nil
	}

	// Update worker lease table
	_, _ = tx.ExecContext(ctx, `
		INSERT INTO orchestration_worker_leases (worker_id, last_heartbeat, active_job_id, status)
		VALUES (?, ?, ?, 'active')
		ON CONFLICT(worker_id) DO UPDATE SET
			last_heartbeat = excluded.last_heartbeat,
			active_job_id = excluded.active_job_id,
			status = 'active'`,
		workerID, now, jobID)

	// Record audit events
	if isRecovery {
		_, _ = tx.ExecContext(ctx, `
			INSERT INTO orchestration_events (job_id, event_type, phase, worker_id, message, created_at)
			VALUES (?, ?, 'recovery', ?, ?, ?)`,
			jobID, string(EventWorkerRecovery), workerID,
			fmt.Sprintf("Job reclaimed from expired lease of worker '%s'", prevWorker), now)
	}

	_, _ = tx.ExecContext(ctx, `
		INSERT INTO orchestration_events (job_id, event_type, phase, worker_id, message, created_at)
		VALUES (?, ?, 'running', ?, 'Job claimed and execution started', ?)`,
		jobID, string(EventJobClaimed), workerID, now)

	if err := tx.Commit(); err != nil {
		return nil, false, err
	}

	job, err := s.GetJob(ctx, jobID)
	return job, isRecovery, err
}

// RenewLease extends lease expiration and verifies job is not cancelled
func (s *Storage) RenewLease(ctx context.Context, jobID, workerID string, leaseDuration time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	var status string
	err := s.db.QueryRowContext(ctx, `SELECT status FROM orchestration_jobs WHERE id = ?`, jobID).Scan(&status)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrJobNotFound
		}
		return err
	}
	if status == string(StatusCancelled) {
		return ErrJobCancelled
	}

	now := time.Now().UTC()
	leaseEnd := now.Add(leaseDuration)

	_, err = s.db.ExecContext(ctx, `
		UPDATE orchestration_jobs
		SET lease_expires_at = ?, updated_at = ?
		WHERE id = ? AND claimed_by = ? AND status = 'running'`,
		leaseEnd, now, jobID, workerID)
	if err != nil {
		return err
	}

	_, _ = s.db.ExecContext(ctx, `
		UPDATE orchestration_worker_leases
		SET last_heartbeat = ?
		WHERE worker_id = ?`, now, workerID)

	return nil
}

// RecordEvent records an execution milestone into durable SQLite storage
func (s *Storage) RecordEvent(ctx context.Context, jobID string, evtType EventType, phase, workerID, message, payload string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if payload == "" {
		payload = "{}"
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO orchestration_events (job_id, event_type, phase, worker_id, message, payload, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		jobID, string(evtType), phase, workerID, message, payload, time.Now().UTC())
	return err
}

// CompleteJob transitions status to completed with final result
func (s *Storage) CompleteJob(ctx context.Context, jobID, workerID, result string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `
		UPDATE orchestration_jobs
		SET status = 'completed',
		    execution_result = ?,
		    completed_at = ?,
		    lease_expires_at = NULL,
		    updated_at = ?
		WHERE id = ? AND status = 'running'`,
		result, now, now, jobID)
	if err != nil {
		return err
	}

	_, _ = tx.ExecContext(ctx, `
		UPDATE orchestration_worker_leases
		SET active_job_id = NULL, last_heartbeat = ?
		WHERE worker_id = ?`, now, workerID)

	_, _ = tx.ExecContext(ctx, `
		INSERT INTO orchestration_events (job_id, event_type, phase, worker_id, message, created_at)
		VALUES (?, ?, 'completed', ?, 'Job execution successfully completed', ?)`,
		jobID, string(EventJobCompleted), workerID, now)

	return tx.Commit()
}

// FailOrScheduleRetry handles failures, incrementing retries or terminating as failed
func (s *Storage) FailOrScheduleRetry(ctx context.Context, jobID, workerID, errMsg string, retryable bool, backoff time.Duration) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var retryCount, maxRetries int
	err := s.db.QueryRowContext(ctx, `SELECT retry_count, max_retries FROM orchestration_jobs WHERE id = ?`, jobID).Scan(&retryCount, &maxRetries)
	if err != nil {
		return false, err
	}

	now := time.Now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	if retryable && retryCount < maxRetries {
		newRetries := retryCount + 1
		leaseEnd := now.Add(backoff)

		_, err = tx.ExecContext(ctx, `
			UPDATE orchestration_jobs
			SET status = 'waiting',
			    retry_count = ?,
			    error_message = ?,
			    lease_expires_at = ?,
			    updated_at = ?
			WHERE id = ?`,
			newRetries, errMsg, leaseEnd, now, jobID)
		if err != nil {
			return false, err
		}

		_, _ = tx.ExecContext(ctx, `
			INSERT INTO orchestration_events (job_id, event_type, phase, worker_id, message, created_at)
			VALUES (?, ?, 'retry', ?, ?, ?)`,
			jobID, string(EventRetryScheduled), workerID,
			fmt.Sprintf("Retry %d/%d scheduled in %v: %s", newRetries, maxRetries, backoff, errMsg), now)

		return true, tx.Commit()
	}

	// Permanent failure
	_, err = tx.ExecContext(ctx, `
		UPDATE orchestration_jobs
		SET status = 'failed',
		    error_message = ?,
		    completed_at = ?,
		    lease_expires_at = NULL,
		    updated_at = ?
		WHERE id = ?`,
		errMsg, now, now, jobID)
	if err != nil {
		return false, err
	}

	_, _ = tx.ExecContext(ctx, `
		UPDATE orchestration_worker_leases
		SET active_job_id = NULL, last_heartbeat = ?
		WHERE worker_id = ?`, now, workerID)

	_, _ = tx.ExecContext(ctx, `
		INSERT INTO orchestration_events (job_id, event_type, phase, worker_id, message, created_at)
		VALUES (?, ?, 'failed', ?, ?, ?)`,
		jobID, string(EventJobFailed), workerID,
		fmt.Sprintf("Job failed: %s", errMsg), now)

	return false, tx.Commit()
}

// CancelJob marks a job cancelled and informs workers
func (s *Storage) CancelJob(ctx context.Context, jobID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC()
	res, err := s.db.ExecContext(ctx, `
		UPDATE orchestration_jobs
		SET status = 'cancelled',
		    completed_at = ?,
		    lease_expires_at = NULL,
		    updated_at = ?
		WHERE id = ? AND status IN ('queued', 'running', 'waiting')`,
		now, now, jobID)
	if err != nil {
		return err
	}
	if rows, _ := res.RowsAffected(); rows == 0 {
		return ErrJobNotFound
	}

	_, _ = s.db.ExecContext(ctx, `
		INSERT INTO orchestration_events (job_id, event_type, phase, message, created_at)
		VALUES (?, ?, 'cancelled', 'Job cancelled by user request', ?)`,
		jobID, string(EventJobCancelled), now)

	return nil
}

// GetJob returns a job by ID
func (s *Storage) GetJob(ctx context.Context, jobID string) (*Job, error) {
	var j Job
	var claimedBy, execResult, errMsg sql.NullString
	var leaseExpires, startedAt, completedAt sql.NullTime

	err := s.db.QueryRowContext(ctx, `
		SELECT id, task, status, max_rounds, max_calls, current_round, total_calls,
		       retry_count, max_retries, claimed_by, lease_expires_at, metadata,
		       execution_result, error_message, created_at, updated_at, started_at, completed_at
		FROM orchestration_jobs
		WHERE id = ?`, jobID).Scan(
		&j.ID, &j.Task, &j.Status, &j.MaxRounds, &j.MaxCalls, &j.CurrentRound, &j.TotalCalls,
		&j.RetryCount, &j.MaxRetries, &claimedBy, &leaseExpires, &j.Metadata,
		&execResult, &errMsg, &j.CreatedAt, &j.UpdatedAt, &startedAt, &completedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrJobNotFound
		}
		return nil, err
	}

	if claimedBy.Valid {
		j.ClaimedBy = claimedBy.String
	}
	if execResult.Valid {
		j.ExecutionResult = execResult.String
	}
	if errMsg.Valid {
		j.ErrorMessage = errMsg.String
	}
	if leaseExpires.Valid {
		t := leaseExpires.Time
		j.LeaseExpiresAt = &t
	}
	if startedAt.Valid {
		t := startedAt.Time
		j.StartedAt = &t
	}
	if completedAt.Valid {
		t := completedAt.Time
		j.CompletedAt = &t
	}

	return &j, nil
}

// ListJobs retrieves paginated orchestration jobs
func (s *Storage) ListJobs(ctx context.Context, limit, offset int, statusFilter string) ([]Job, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 100 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}

	var rows *sql.Rows
	var err error

	if statusFilter != "" {
		rows, err = s.db.QueryContext(ctx, `
			SELECT id, task, status, max_rounds, max_calls, current_round, total_calls,
			       retry_count, max_retries, claimed_by, lease_expires_at, metadata,
			       execution_result, error_message, created_at, updated_at, started_at, completed_at
			FROM orchestration_jobs
			WHERE status = ?
			ORDER BY created_at DESC
			LIMIT ? OFFSET ?`, statusFilter, limit, offset)
	} else {
		rows, err = s.db.QueryContext(ctx, `
			SELECT id, task, status, max_rounds, max_calls, current_round, total_calls,
			       retry_count, max_retries, claimed_by, lease_expires_at, metadata,
			       execution_result, error_message, created_at, updated_at, started_at, completed_at
			FROM orchestration_jobs
			ORDER BY created_at DESC
			LIMIT ? OFFSET ?`, limit, offset)
	}

	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var jobs []Job
	for rows.Next() {
		var j Job
		var claimedBy, execResult, errMsg sql.NullString
		var leaseExpires, startedAt, completedAt sql.NullTime

		if err := rows.Scan(
			&j.ID, &j.Task, &j.Status, &j.MaxRounds, &j.MaxCalls, &j.CurrentRound, &j.TotalCalls,
			&j.RetryCount, &j.MaxRetries, &claimedBy, &leaseExpires, &j.Metadata,
			&execResult, &errMsg, &j.CreatedAt, &j.UpdatedAt, &startedAt, &completedAt); err == nil {
			if claimedBy.Valid {
				j.ClaimedBy = claimedBy.String
			}
			if execResult.Valid {
				j.ExecutionResult = execResult.String
			}
			if errMsg.Valid {
				j.ErrorMessage = errMsg.String
			}
			if leaseExpires.Valid {
				t := leaseExpires.Time
				j.LeaseExpiresAt = &t
			}
			if startedAt.Valid {
				t := startedAt.Time
				j.StartedAt = &t
			}
			if completedAt.Valid {
				t := completedAt.Time
				j.CompletedAt = &t
			}
			jobs = append(jobs, j)
		}
	}
	return jobs, nil
}

// GetJobEvents returns full chronological events for audit or stream catchup
func (s *Storage) GetJobEvents(ctx context.Context, jobID string) ([]Event, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, job_id, event_type, phase, worker_id, message, payload, created_at
		FROM orchestration_events
		WHERE job_id = ?
		ORDER BY id ASC`, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var events []Event
	for rows.Next() {
		var e Event
		var wID, msg, payload sql.NullString
		if err := rows.Scan(&e.ID, &e.JobID, &e.EventType, &e.Phase, &wID, &msg, &payload, &e.CreatedAt); err == nil {
			if wID.Valid {
				e.WorkerID = wID.String
			}
			if msg.Valid {
				e.Message = msg.String
			}
			if payload.Valid {
				e.Payload = payload.String
			}
			events = append(events, e)
		}
	}
	return events, nil
}
