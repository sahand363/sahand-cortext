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
	ErrJobNotFound     = errors.New("orchestration job not found")
	ErrJobCancelled    = errors.New("orchestration job has been cancelled")
	ErrMessageNotFound = errors.New("collaboration message not found")
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

// InitSchema ensures tables, columns, and indexes exist idempotently
func (s *Storage) InitSchema() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	queries := []string{
		`CREATE TABLE IF NOT EXISTS orchestration_jobs (
			id TEXT PRIMARY KEY,
			task TEXT NOT NULL,
			status TEXT NOT NULL,
			current_stage TEXT NOT NULL DEFAULT 'queued',
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
		`CREATE TABLE IF NOT EXISTS orchestration_participants (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			job_id TEXT NOT NULL REFERENCES orchestration_jobs(id) ON DELETE CASCADE,
			role TEXT NOT NULL,
			name TEXT NOT NULL DEFAULT '',
			provider TEXT NOT NULL,
			model TEXT NOT NULL,
			system_prompt TEXT NOT NULL DEFAULT '',
			sort_order INTEGER NOT NULL DEFAULT 0,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		);`,
		`CREATE INDEX IF NOT EXISTS idx_orch_participants_job ON orchestration_participants(job_id, sort_order ASC);`,
		`CREATE TABLE IF NOT EXISTS orchestration_messages (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			job_id TEXT NOT NULL REFERENCES orchestration_jobs(id) ON DELETE CASCADE,
			round INTEGER NOT NULL DEFAULT 1,
			turn_index INTEGER NOT NULL DEFAULT 1,
			sender_role TEXT NOT NULL,
			sender_name TEXT NOT NULL DEFAULT '',
			provider TEXT NOT NULL,
			model TEXT NOT NULL,
			action TEXT NOT NULL DEFAULT 'continue',
			content TEXT NOT NULL,
			completion_summary TEXT NOT NULL DEFAULT '',
			raw_response TEXT NOT NULL DEFAULT '',
			latency_ms INTEGER NOT NULL DEFAULT 0,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		);`,
		`CREATE INDEX IF NOT EXISTS idx_orch_messages_job ON orchestration_messages(job_id, id ASC);`,
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

	_, _ = s.db.Exec(`ALTER TABLE orchestration_jobs ADD COLUMN current_stage TEXT NOT NULL DEFAULT 'queued';`)
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
		INSERT INTO orchestration_jobs (id, task, status, current_stage, max_rounds, max_calls, metadata, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, task, string(StatusQueued), string(StageQueued), maxRounds, maxCalls, metadata, now, now)
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
		ID:           id,
		Task:         task,
		Status:       StatusQueued,
		CurrentStage: StageQueued,
		MaxRounds:    maxRounds,
		MaxCalls:     maxCalls,
		MaxRetries:   3,
		Metadata:     metadata,
		CreatedAt:    now,
		UpdatedAt:    now,
	}, nil
}

// SaveParticipants stores the participant roster for a job
func (s *Storage) SaveParticipants(ctx context.Context, jobID string, participants []Participant) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `DELETE FROM orchestration_participants WHERE job_id = ?`, jobID)
	if err != nil {
		return err
	}

	now := time.Now().UTC()
	for i, p := range participants {
		role := strings.ToLower(strings.TrimSpace(p.Role))
		if role == "" {
			if i == 0 {
				role = RoleCoordinator
			} else {
				role = RoleWorker
			}
		}
		name := strings.TrimSpace(p.Name)
		if name == "" {
			if role == RoleCoordinator {
				name = "Coordinator"
			} else {
				name = "Worker"
			}
		}
		_, err = tx.ExecContext(ctx, `
			INSERT INTO orchestration_participants (job_id, role, name, provider, model, system_prompt, sort_order, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			jobID, role, name, strings.ToLower(strings.TrimSpace(p.Provider)), strings.TrimSpace(p.Model), p.SystemPrompt, i, now)
		if err != nil {
			return err
		}
	}

	return tx.Commit()
}

// GetParticipants returns the ordered list of participants for a job
func (s *Storage) GetParticipants(ctx context.Context, jobID string) ([]Participant, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, job_id, role, name, provider, model, system_prompt, sort_order, created_at
		FROM orchestration_participants
		WHERE job_id = ?
		ORDER BY sort_order ASC, id ASC`, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Participant
	for rows.Next() {
		var p Participant
		if err := rows.Scan(&p.ID, &p.JobID, &p.Role, &p.Name, &p.Provider, &p.Model, &p.SystemPrompt, &p.SortOrder, &p.CreatedAt); err == nil {
			out = append(out, p)
		}
	}
	return out, nil
}

// SaveCollaborationMessage atomically persists a single model turn and updates job progress in one transaction
func (s *Storage) SaveCollaborationMessage(ctx context.Context, msg CollaborationMessage) (*CollaborationMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

var currentStatus string
if err := tx.QueryRowContext(ctx, `SELECT status FROM orchestration_jobs WHERE id = ?`, msg.JobID).Scan(&currentStatus); err != nil {
    return nil, err
}
	if currentStatus == string(StatusCancelled) {
		return nil, ErrJobCancelled
	}

	now := time.Now().UTC()
	if msg.Action == "" {
		msg.Action = ActionContinue
	}

	res, err := tx.ExecContext(ctx, `
		INSERT INTO orchestration_messages
		(job_id, round, turn_index, sender_role, sender_name, provider, model, action, content, completion_summary, raw_response, latency_ms, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		msg.JobID, msg.Round, msg.TurnIndex, msg.SenderRole, msg.SenderName, msg.Provider, msg.Model,
		string(msg.Action), msg.Content, msg.CompletionSummary, msg.RawResponse, msg.LatencyMs, now)
	if err != nil {
		return nil, err
	}

	id, _ := res.LastInsertId()
	msg.ID = id
	msg.CreatedAt = now
	msg.ContentLength = len(msg.Content)
	msg.Preview = buildMessagePreview(msg.SenderRole, msg.Content, msg.CompletionSummary, 50)
	msg.HasFullContent = true

	_, err = tx.ExecContext(ctx, `
		UPDATE orchestration_jobs
		SET current_round = ?, total_calls = ?, error_message = NULL, updated_at = ?
		WHERE id = ?`,
		msg.Round, msg.TurnIndex, now, msg.JobID)
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &msg, nil
}

// GetCollaborationMessages retrieves all collaboration turns for a job in chronological order
func (s *Storage) GetCollaborationMessages(ctx context.Context, jobID string) ([]CollaborationMessage, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, job_id, round, turn_index, sender_role, sender_name, provider, model,
		       action, content, completion_summary, raw_response, latency_ms, created_at
		FROM orchestration_messages
		WHERE job_id = ?
		ORDER BY id ASC`, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var msgs []CollaborationMessage
	for rows.Next() {
		var m CollaborationMessage
		var act string
		if err := rows.Scan(&m.ID, &m.JobID, &m.Round, &m.TurnIndex, &m.SenderRole, &m.SenderName,
			&m.Provider, &m.Model, &act, &m.Content, &m.CompletionSummary, &m.RawResponse, &m.LatencyMs, &m.CreatedAt); err == nil {
			m.Action = CollaborationAction(act)
			m.ContentLength = len(m.Content)
			m.Preview = buildMessagePreview(m.SenderRole, m.Content, m.CompletionSummary, 50)
			m.HasFullContent = true
			msgs = append(msgs, m)
		}
	}
	return msgs, nil
}

func buildMessagePreview(senderRole, content, completionSummary string, maxRunes int) string {
	if maxRunes <= 0 {
		maxRunes = 50
	}
	src := strings.TrimSpace(content)
	if senderRole == RoleUser && strings.HasPrefix(src, "[REV_OPTS:") {
		if endIdx := strings.Index(src, "]"); endIdx != -1 {
			src = strings.TrimSpace(src[endIdx+1:])
		}
	}
	if len(src) >= 8 && strings.EqualFold(src[:8], "SUMMARY:") {
		rest := strings.TrimSpace(src[8:])
		if nl := strings.Index(rest, "\n"); nl != -1 {
			firstLine := strings.TrimSpace(rest[:nl])
			if firstLine != "" {
				src = firstLine
			} else {
				src = strings.TrimSpace(rest[nl+1:])
			}
		} else if rest != "" {
			src = rest
		}
	}
	if strings.HasPrefix(src, "===") && strings.TrimSpace(completionSummary) != "" {
		src = strings.TrimSpace(completionSummary)
	}
	if src == "" {
		src = strings.TrimSpace(completionSummary)
	}
	clean := strings.Join(strings.Fields(src), " ")
	runes := []rune(clean)
	if len(runes) <= maxRunes {
		return clean
	}
	return string(runes[:maxRunes])
}

// GetCollaborationMessagePreviews retrieves lightweight 50-character previews for transcript rendering
// without transferring full multi-file code payloads on every polling cycle.
func (s *Storage) GetCollaborationMessagePreviews(ctx context.Context, jobID string) ([]CollaborationMessage, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, job_id, round, turn_index, sender_role, sender_name, provider, model,
		       action, content, completion_summary, latency_ms, created_at
		FROM orchestration_messages
		WHERE job_id = ?
		ORDER BY id ASC`, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var msgs []CollaborationMessage
	for rows.Next() {
		var m CollaborationMessage
		var act, fullContent string
		if err := rows.Scan(&m.ID, &m.JobID, &m.Round, &m.TurnIndex, &m.SenderRole, &m.SenderName,
			&m.Provider, &m.Model, &act, &fullContent, &m.CompletionSummary, &m.LatencyMs, &m.CreatedAt); err == nil {
			m.Action = CollaborationAction(act)
			m.ContentLength = len(fullContent)
			m.Preview = buildMessagePreview(m.SenderRole, fullContent, m.CompletionSummary, 50)
			m.HasFullContent = false
			msgs = append(msgs, m)
		}
	}
	return msgs, nil
}

// GetCollaborationMessageByID retrieves a single full collaboration message on demand.
func (s *Storage) GetCollaborationMessageByID(ctx context.Context, jobID string, messageID int64) (*CollaborationMessage, error) {
	var m CollaborationMessage
	var act string
	err := s.db.QueryRowContext(ctx, `
		SELECT id, job_id, round, turn_index, sender_role, sender_name, provider, model,
		       action, content, completion_summary, latency_ms, created_at
		FROM orchestration_messages
		WHERE job_id = ? AND id = ?`, jobID, messageID).Scan(
		&m.ID, &m.JobID, &m.Round, &m.TurnIndex, &m.SenderRole, &m.SenderName,
		&m.Provider, &m.Model, &act, &m.Content, &m.CompletionSummary, &m.LatencyMs, &m.CreatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrMessageNotFound
		}
		return nil, err
	}
	m.Action = CollaborationAction(act)
	m.ContentLength = len(m.Content)
	m.Preview = buildMessagePreview(m.SenderRole, m.Content, m.CompletionSummary, 50)
	m.HasFullContent = true
	return &m, nil
}

// UpdateJobProgress updates current_round and total_calls on an active job
func (s *Storage) UpdateJobProgress(ctx context.Context, jobID string, currentRound, totalCalls int) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.ExecContext(ctx, `
		UPDATE orchestration_jobs
		SET current_round = ?, total_calls = ?, updated_at = ?
		WHERE id = ? AND status NOT IN ('cancelled', 'completed')`,
		currentRound, totalCalls, time.Now().UTC(), jobID)
	return err
}

// UpdateJobStage updates the durable workflow stage and current round
func (s *Storage) UpdateJobStage(ctx context.Context, jobID string, stage WorkflowStage, currentRound, totalCalls int) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.ExecContext(ctx, `
		UPDATE orchestration_jobs
		SET status = CASE WHEN status = 'pausing' THEN 'pausing' ELSE 'running' END,
		    current_stage = ?, current_round = ?, total_calls = ?, updated_at = ?
		WHERE id = ? AND status NOT IN ('cancelled', 'completed')`,
		string(stage), currentRound, totalCalls, time.Now().UTC(), jobID)
	return err
}

// IsJobCancelled checks SQLite directly to verify whether a job has been cancelled
func (s *Storage) IsJobCancelled(ctx context.Context, jobID string) bool {
	var status string
	err := s.db.QueryRowContext(ctx, `SELECT status FROM orchestration_jobs WHERE id = ?`, jobID).Scan(&status)
	if err != nil {
		return false
	}
	return status == string(StatusCancelled)
}

// ClaimNextJob atomically claims an available queued or expired job.
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
		WHERE id = ? AND status IN ('queued', 'running', 'waiting')
		  AND (status = 'queued' OR lease_expires_at IS NULL OR lease_expires_at <= ?)`,
		workerID, leaseEnd, now, now, jobID, now)
	if err != nil {
		return nil, false, err
	}
	if rows, _ := res.RowsAffected(); rows == 0 {
		return nil, false, nil
	}

	_, _ = tx.ExecContext(ctx, `
		INSERT INTO orchestration_worker_leases (worker_id, last_heartbeat, active_job_id, status)
		VALUES (?, ?, ?, 'active')
		ON CONFLICT(worker_id) DO UPDATE SET
			last_heartbeat = excluded.last_heartbeat,
			active_job_id = excluded.active_job_id,
			status = 'active'`,
		workerID, now, jobID)

	if isRecovery {
		_, _ = tx.ExecContext(ctx, `
			INSERT INTO orchestration_events (job_id, event_type, phase, worker_id, message, created_at)
			VALUES (?, ?, 'recovery', ?, ?, ?)`,
			jobID, string(EventWorkerRecovery), workerID,
			fmt.Sprintf("Job recovered and resumed by '%s' (previous state: %s, worker: '%s')", workerID, prevStatus, prevWorker), now)
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
	if s.FinalizePauseIfRequested(ctx, jobID) {
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx, `
		UPDATE orchestration_jobs
		SET status = 'completed',
		    current_stage = 'completed',
		    execution_result = ?,
		    error_message = NULL,
		    completed_at = ?,
		    lease_expires_at = NULL,
		    updated_at = ?
		WHERE id = ? AND status = 'running' AND status != 'paused'`,
		result, now, now, jobID)
	if err != nil {
		return err
	}
	if rows, _ := res.RowsAffected(); rows == 0 {
		return nil
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

	var status string
	var retryCount, maxRetries int
	err := s.db.QueryRowContext(ctx, `SELECT status, retry_count, max_retries FROM orchestration_jobs WHERE id = ?`, jobID).Scan(&status, &retryCount, &maxRetries)
	if err != nil {
		return false, err
	}
	if status == string(StatusCancelled) || status == string(StatusCompleted) {
		return false, nil
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
			WHERE id = ? AND status NOT IN ('cancelled', 'completed')`,
			newRetries, errMsg, leaseEnd, now, jobID)
		if err != nil {
			return false, err
		}

		_, _ = tx.ExecContext(ctx, `
			UPDATE orchestration_worker_leases
			SET active_job_id = NULL, last_heartbeat = ?
			WHERE worker_id = ?`, now, workerID)

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
		    current_stage = 'failed',
		    error_message = ?,
		    completed_at = ?,
		    lease_expires_at = NULL,
		    updated_at = ?
		WHERE id = ? AND status NOT IN ('cancelled', 'completed')`,
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

// RetryJob resets a failed, cancelled, or waiting job back to queued so it resumes from its saved messages
func (s *Storage) RetryJob(ctx context.Context, jobID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC()
	res, err := s.db.ExecContext(ctx, `
		UPDATE orchestration_jobs
		SET status = 'queued',
		    current_stage = 'queued',
		    retry_count = 0,
		    error_message = NULL,
		    claimed_by = NULL,
		    lease_expires_at = NULL,
		    completed_at = NULL,
		    updated_at = ?
		WHERE id = ? AND status IN ('failed', 'cancelled', 'waiting', 'paused')`,
		now, jobID)
	if err != nil {
		return err
	}
	if rows, _ := res.RowsAffected(); rows == 0 {
		return ErrJobNotFound
	}

	_, _ = s.db.ExecContext(ctx, `
		INSERT INTO orchestration_events (job_id, event_type, phase, message, created_at)
		VALUES (?, ?, 'queued', 'Job manually re-queued for retry/resume from last saved state', ?)`,
		jobID, string(EventRetryScheduled), now)

	return nil
}

// IterateJob appends a human-in-the-loop revision request to an existing job, expands its round/call budgets,
// and re-queues it so the AI team modifies the existing deliverable without losing prior work.
func (s *Storage) IterateJob(ctx context.Context, jobID, instructions string, extraRounds, extraCalls int) (*Job, error) {
	instructions = strings.TrimSpace(instructions)
	if instructions == "" {
		return nil, errors.New("revision instructions cannot be empty")
	}
	if extraRounds <= 0 {
		extraRounds = 3
	}
	if extraCalls <= 0 {
		extraCalls = 8
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var status string
	var curRound, maxRounds, totalCalls, maxCalls int
	err = tx.QueryRowContext(ctx, `
		SELECT status, current_round, max_rounds, total_calls, max_calls
		FROM orchestration_jobs
		WHERE id = ?`, jobID).Scan(&status, &curRound, &maxRounds, &totalCalls, &maxCalls)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrJobNotFound
		}
		return nil, err
	}
	if status == string(StatusRunning) || status == string(StatusQueued) {
		return nil, errors.New("job is already running or queued; stop it first or wait for the current turn to finish")
	}

	nextRound := curRound + 1
	if nextRound < 1 {
		nextRound = 1
	}
	newTurnIndex := totalCalls + 1

	newMaxRounds := maxRounds
	if nextRound+extraRounds-1 > newMaxRounds {
		newMaxRounds = nextRound + extraRounds - 1
	}
	newMaxCalls := maxCalls
	if newTurnIndex+extraCalls > newMaxCalls {
		newMaxCalls = newTurnIndex + extraCalls
	}

	now := time.Now().UTC()

	// 1. Insert the user's revision instructions into the collaboration transcript
	_, err = tx.ExecContext(ctx, `
		INSERT INTO orchestration_messages
		(job_id, round, turn_index, sender_role, sender_name, provider, model, action, content, completion_summary, raw_response, latency_ms, created_at)
		VALUES (?, ?, ?, ?, 'Workspace Owner (User)', 'user', 'human-feedback', ?, ?, '', '', 0, ?)`,
		jobID, nextRound, newTurnIndex, RoleUser, string(ActionRevise), instructions, now)
	if err != nil {
		return nil, err
	}

	// 2. Re-queue the job with expanded limits
	_, err = tx.ExecContext(ctx, `
		UPDATE orchestration_jobs
		SET status = 'queued',
		    current_stage = 'queued',
		    current_round = ?,
		    max_rounds = ?,
		    total_calls = ?,
		    max_calls = ?,
		    retry_count = 0,
		    error_message = NULL,
		    claimed_by = NULL,
		    lease_expires_at = NULL,
		    completed_at = NULL,
		    updated_at = ?
		WHERE id = ?`,
		nextRound, newMaxRounds, newTurnIndex, newMaxCalls, now, jobID)
	if err != nil {
		return nil, err
	}

	// 3. Record durable audit event
	shortPreview := instructions
	if len([]rune(shortPreview)) > 120 {
		shortPreview = string([]rune(shortPreview)[:120]) + "…"
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO orchestration_events (job_id, event_type, phase, message, created_at)
		VALUES (?, ?, 'queued', ?, ?)`,
		jobID, string(EventUserRevision),
		fmt.Sprintf("User requested revision for Round %d: %s", nextRound, shortPreview), now)
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}

	return s.GetJob(ctx, jobID)
}

// ApplyAuditResult promotes an Auditor message's corrected files to become the job's active execution_result
func (s *Storage) ApplyAuditResult(ctx context.Context, jobID string, messageID int64) (*Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var content, summary, providerName, modelName string
	if messageID > 0 {
		err = tx.QueryRowContext(ctx, `
			SELECT content, completion_summary, provider, model
			FROM orchestration_messages
			WHERE id = ? AND job_id = ? AND sender_role = ?`,
			messageID, jobID, RoleAuditor).Scan(&content, &summary, &providerName, &modelName)
	} else {
		err = tx.QueryRowContext(ctx, `
			SELECT content, completion_summary, provider, model
			FROM orchestration_messages
			WHERE job_id = ? AND sender_role = ?
			ORDER BY id DESC LIMIT 1`,
			jobID, RoleAuditor).Scan(&content, &summary, &providerName, &modelName)
	}
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errors.New("auditor report not found for this job")
		}
		return nil, err
	}

	finalResult := strings.TrimSpace(content)
	if strings.TrimSpace(summary) != "" {
		finalResult = fmt.Sprintf("%s\n\n---\n### Coordinator Completion Summary\n[Verified & Corrected by Independent Auditor %s/%s]\n%s",
			finalResult, providerName, modelName, strings.TrimSpace(summary))
	}

	now := time.Now().UTC()
	_, err = tx.ExecContext(ctx, `
		UPDATE orchestration_jobs
		SET execution_result = ?,
		    status = 'completed',
		    current_stage = 'completed',
		    updated_at = ?
		WHERE id = ?`,
		finalResult, now, jobID)
	if err != nil {
		return nil, err
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO orchestration_events (job_id, event_type, phase, message, created_at)
		VALUES (?, ?, 'completed', ?, ?)`,
		jobID, string(EventAuditApplied),
		fmt.Sprintf("Applied corrected deliverable from Independent Auditor (%s/%s)", providerName, modelName), now)
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}

	return s.GetJob(ctx, jobID)
}

// CancelJob marks a job cancelled and informs workers
func (s *Storage) CancelJob(ctx context.Context, jobID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC()
	res, err := s.db.ExecContext(ctx, `
		UPDATE orchestration_jobs
		SET status = 'cancelled',
		    current_stage = 'cancelled',
		    completed_at = ?,
		    lease_expires_at = NULL,
		    updated_at = ?
		WHERE id = ? AND status IN ('queued', 'running', 'waiting', 'pausing', 'paused')`,
		now, now, jobID)
	if err != nil {
		return err
	}
	if rows, _ := res.RowsAffected(); rows == 0 {
		var curStatus string
		if e := s.db.QueryRowContext(ctx, `SELECT status FROM orchestration_jobs WHERE id = ?`, jobID).Scan(&curStatus); e == nil && curStatus == string(StatusCancelled) {
			return nil
		}
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
	var stage, claimedBy, execResult, errMsg sql.NullString
	var leaseExpires, startedAt, completedAt sql.NullTime

	err := s.db.QueryRowContext(ctx, `
		SELECT id, task, status, current_stage, max_rounds, max_calls, current_round, total_calls,
		       retry_count, max_retries, claimed_by, lease_expires_at, metadata,
		       execution_result, error_message, created_at, updated_at, started_at, completed_at
		FROM orchestration_jobs
		WHERE id = ?`, jobID).Scan(
		&j.ID, &j.Task, &j.Status, &stage, &j.MaxRounds, &j.MaxCalls, &j.CurrentRound, &j.TotalCalls,
		&j.RetryCount, &j.MaxRetries, &claimedBy, &leaseExpires, &j.Metadata,
		&execResult, &errMsg, &j.CreatedAt, &j.UpdatedAt, &startedAt, &completedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrJobNotFound
		}
		return nil, err
	}

	if stage.Valid && stage.String != "" {
		j.CurrentStage = WorkflowStage(stage.String)
	} else {
		j.CurrentStage = WorkflowStage(j.Status)
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

// GetJobDetails returns the job alongside its participants, lightweight message previews, and audit events
func (s *Storage) GetJobDetails(ctx context.Context, jobID string) (*JobDetails, error) {
	job, err := s.GetJob(ctx, jobID)
	if err != nil {
		return nil, err
	}
	parts, err := s.GetParticipants(ctx, jobID)
	if err != nil {
		return nil, err
	}
	msgs, err := s.GetCollaborationMessagePreviews(ctx, jobID)
	if err != nil {
		return nil, err
	}
	evts, err := s.GetJobEvents(ctx, jobID)
	if err != nil {
		return nil, err
	}
	return &JobDetails{
		Job:          job,
		Participants: parts,
		Messages:     msgs,
		Events:       evts,
	}, nil
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
			SELECT id, task, status, current_stage, max_rounds, max_calls, current_round, total_calls,
			       retry_count, max_retries, claimed_by, lease_expires_at, metadata,
			       execution_result, error_message, created_at, updated_at, started_at, completed_at
			FROM orchestration_jobs
			WHERE status = ?
			ORDER BY created_at DESC
			LIMIT ? OFFSET ?`, statusFilter, limit, offset)
	} else {
		rows, err = s.db.QueryContext(ctx, `
			SELECT id, task, status, current_stage, max_rounds, max_calls, current_round, total_calls,
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
		var stage, claimedBy, execResult, errMsg sql.NullString
		var leaseExpires, startedAt, completedAt sql.NullTime

		if err := rows.Scan(
			&j.ID, &j.Task, &j.Status, &stage, &j.MaxRounds, &j.MaxCalls, &j.CurrentRound, &j.TotalCalls,
			&j.RetryCount, &j.MaxRetries, &claimedBy, &leaseExpires, &j.Metadata,
			&execResult, &errMsg, &j.CreatedAt, &j.UpdatedAt, &startedAt, &completedAt); err == nil {
			if stage.Valid && stage.String != "" {
				j.CurrentStage = WorkflowStage(stage.String)
			} else {
				j.CurrentStage = WorkflowStage(j.Status)
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

// PauseJob marks a running job as 'pausing' — the worker finishes its current
// turn, and FinalizePauseIfRequested flips it to 'paused' once the worker exits.
func (s *Storage) PauseJob(ctx context.Context, jobID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC()
	res, err := s.db.ExecContext(ctx, `
		UPDATE orchestration_jobs
		SET status = 'pausing',
		    updated_at = ?
		WHERE id = ? AND status = 'running'`,
		now, jobID)
	if err != nil {
		return err
	}
	if rows, _ := res.RowsAffected(); rows == 0 {
		return ErrJobNotFound
	}

	_, _ = s.db.ExecContext(ctx, `
		INSERT INTO orchestration_events (job_id, event_type, phase, worker_id, message, created_at)
		VALUES (?, ?, 'pausing', 'user', 'Pause requested — waiting for current AI turn to finish before pausing', ?)`,
		jobID, "job_paused", now)

	return nil
}

// FinalizePauseIfRequested flips a 'pausing' job to 'paused' (releasing its
// worker lease) once the worker goroutine has exited. Returns true when the
// job is pausing or paused. It takes no lock — CompleteJob calls it while
// holding s.mu — so the event is written with a direct INSERT, not
// RecordEvent (which locks s.mu and would deadlock).
func (s *Storage) FinalizePauseIfRequested(ctx context.Context, jobID string) bool {
	var st string
	err := s.db.QueryRowContext(ctx, `SELECT status FROM orchestration_jobs WHERE id = ?`, jobID).Scan(&st)
	if err != nil || (st != "pausing" && st != "paused") {
		return false
	}
	if st == "pausing" {
		now := time.Now().UTC()
		res, err := s.db.ExecContext(ctx, `
			UPDATE orchestration_jobs
			SET status = 'paused',
			    current_stage = 'paused',
			    claimed_by = NULL,
			    lease_expires_at = NULL,
			    updated_at = ?
			WHERE id = ? AND status = 'pausing'`,
			now, jobID)
		if err == nil {
			if n, _ := res.RowsAffected(); n > 0 {
				_, _ = s.db.ExecContext(ctx, `
					INSERT INTO orchestration_events (job_id, event_type, phase, worker_id, message, created_at)
					VALUES (?, ?, 'paused', 'system', 'Current AI turn finished — job is now safely paused', ?)`,
					jobID, "job_paused", now)
			}
		}
	}
	return true
}

func (s *Storage) IsJobPaused(ctx context.Context, jobID string) bool {
	return s.FinalizePauseIfRequested(ctx, jobID)
}
