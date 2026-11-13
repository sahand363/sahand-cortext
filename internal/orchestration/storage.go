package orchestration

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	ErrJobNotFound  = errors.New("orchestration job not found")
	ErrJobCancelled = errors.New("orchestration job was cancelled")
	ErrJobCompleted = errors.New("orchestration job is already completed")
)

type Storage struct {
	db *sql.DB
}

func NewStorage(db *sql.DB) *Storage {
	return &Storage{db: db}
}

func (s *Storage) InitSchema() error {
	queries := []string{
		`CREATE TABLE IF NOT EXISTS orch_jobs(
			id TEXT PRIMARY KEY,
			task TEXT NOT NULL,
			status TEXT NOT NULL DEFAULT 'queued',
			current_stage TEXT NOT NULL DEFAULT 'queued',
			current_round INTEGER NOT NULL DEFAULT 0,
			max_rounds INTEGER NOT NULL DEFAULT 5,
			total_calls INTEGER NOT NULL DEFAULT 0,
			max_calls INTEGER NOT NULL DEFAULT 15,
			retry_count INTEGER NOT NULL DEFAULT 0,
			max_retries INTEGER NOT NULL DEFAULT 3,
			error_message TEXT NOT NULL DEFAULT '',
			execution_result TEXT NOT NULL DEFAULT '',
			metadata TEXT NOT NULL DEFAULT '{}',
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		);`,
		`CREATE TABLE IF NOT EXISTS orch_participants(
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			job_id TEXT NOT NULL REFERENCES orch_jobs(id) ON DELETE CASCADE,
			role TEXT NOT NULL,
			name TEXT NOT NULL,
			provider TEXT NOT NULL,
			model TEXT NOT NULL,
			sort_order INTEGER NOT NULL DEFAULT 0
		);`,
		`CREATE TABLE IF NOT EXISTS orch_messages(
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			job_id TEXT NOT NULL REFERENCES orch_jobs(id) ON DELETE CASCADE,
			round INTEGER NOT NULL,
			turn_index INTEGER NOT NULL,
			sender_role TEXT NOT NULL,
			sender_name TEXT NOT NULL,
			provider TEXT NOT NULL,
			model TEXT NOT NULL,
			action TEXT NOT NULL,
			content TEXT NOT NULL,
			completion_summary TEXT NOT NULL DEFAULT '',
			raw_response TEXT NOT NULL DEFAULT '',
			latency_ms INTEGER NOT NULL DEFAULT 0,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		);`,
		`CREATE TABLE IF NOT EXISTS orch_events(
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			job_id TEXT NOT NULL REFERENCES orch_jobs(id) ON DELETE CASCADE,
			event_type TEXT NOT NULL,
			phase TEXT NOT NULL,
			worker_id TEXT NOT NULL,
			message TEXT NOT NULL,
			payload_json TEXT NOT NULL DEFAULT '{}',
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		);`,
		`PRAGMA journal_mode=WAL;`,
		`PRAGMA foreign_keys=ON;`,
	}
	for _, q := range queries {
		if _, err := s.db.Exec(q); err != nil {
			return err
		}
	}
	return nil
}

func (s *Storage) CreateJob(ctx context.Context, task string, maxRounds, maxCalls int, metadata string) (*Job, error) {
	id := fmt.Sprintf("orch-%x", time.Now().UnixNano()&0xffffffff)

	if maxRounds <= 0 {
		maxRounds = 5
	}
	if maxCalls <= 0 {
		maxCalls = 15
	}
	if strings.TrimSpace(metadata) == "" {
		metadata = "{}"
	}

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO orch_jobs(id, task, status, current_stage, max_rounds, max_calls, metadata) VALUES(?, ?, 'queued', 'queued', ?, ?, ?)`,
		id, task, maxRounds, maxCalls, metadata,
	)
	if err != nil {
		return nil, err
	}

	_ = s.RecordEvent(ctx, id, "job_created", "queued", "system", "Collaboration job created and queued", "{}")
	return s.GetJob(ctx, id)
}

func (s *Storage) GetJob(ctx context.Context, id string) (*Job, error) {
	var j Job
	err := s.db.QueryRowContext(ctx,
		`SELECT id, task, status, current_stage, current_round, max_rounds, total_calls, max_calls, retry_count, max_retries, error_message, execution_result, metadata, created_at, updated_at 
		 FROM orch_jobs WHERE id=?`, id,
	).Scan(&j.ID, &j.Task, &j.Status, &j.CurrentStage, &j.CurrentRound, &j.MaxRounds, &j.TotalCalls, &j.MaxCalls, &j.RetryCount, &j.MaxRetries, &j.ErrorMessage, &j.ExecutionResult, &j.Metadata, &j.CreatedAt, &j.UpdatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrJobNotFound
		}
		return nil, err
	}
	return &j, nil
}

func (s *Storage) ListJobs(ctx context.Context, limit, offset int, statusFilter string) ([]Job, error) {
	var rows *sql.Rows
	var err error
	if statusFilter != "" {
		rows, err = s.db.QueryContext(ctx,
			`SELECT id, task, status, current_stage, current_round, max_rounds, total_calls, max_calls, retry_count, max_retries, error_message, execution_result, metadata, created_at, updated_at 
			 FROM orch_jobs WHERE status=? ORDER BY created_at DESC LIMIT ? OFFSET ?`, statusFilter, limit, offset)
	} else {
		rows, err = s.db.QueryContext(ctx,
			`SELECT id, task, status, current_stage, current_round, max_rounds, total_calls, max_calls, retry_count, max_retries, error_message, execution_result, metadata, created_at, updated_at 
			 FROM orch_jobs ORDER BY created_at DESC LIMIT ? OFFSET ?`, limit, offset)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var jobs []Job
	for rows.Next() {
		var j Job
		if rows.Scan(&j.ID, &j.Task, &j.Status, &j.CurrentStage, &j.CurrentRound, &j.MaxRounds, &j.TotalCalls, &j.MaxCalls, &j.RetryCount, &j.MaxRetries, &j.ErrorMessage, &j.ExecutionResult, &j.Metadata, &j.CreatedAt, &j.UpdatedAt) == nil {
			jobs = append(jobs, j)
		}
	}
	return jobs, nil
}

func (s *Storage) GetJobDetails(ctx context.Context, id string) (*JobDetails, error) {
	job, err := s.GetJob(ctx, id)
	if err != nil {
		return nil, err
	}
	participants, _ := s.GetParticipants(ctx, id)
	messages, _ := s.GetCollaborationMessages(ctx, id)
	events, _ := s.GetJobEvents(ctx, id)

	return &JobDetails{
		Job:          *job,
		Participants: participants,
		Messages:     messages,
		Events:       events,
	}, nil
}

func (s *Storage) SaveParticipants(ctx context.Context, jobID string, participants []Participant) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `DELETE FROM orch_participants WHERE job_id=?`, jobID); err != nil {
		return err
	}

	stmt, err := tx.PrepareContext(ctx, `INSERT INTO orch_participants(job_id, role, name, provider, model, sort_order) VALUES(?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for idx, p := range participants {
		sortOrder := p.SortOrder
		if sortOrder == 0 && idx > 0 {
			sortOrder = idx
		}
		name := p.Name
		if name == "" {
			switch p.Role {
			case RoleCoordinator:
				name = "Coordinator / Architect"
			case RoleAuditor:
				name = "Quality Assurance / Auditor"
			default:
				name = "Worker / Developer"
			}
		}
		_, err := stmt.ExecContext(ctx, jobID, p.Role, name, p.Provider, p.Model, sortOrder)
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Storage) GetParticipants(ctx context.Context, jobID string) ([]Participant, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, job_id, role, name, provider, model, sort_order FROM orch_participants WHERE job_id=? ORDER BY sort_order ASC`, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Participant
	for rows.Next() {
		var p Participant
		if rows.Scan(&p.ID, &p.JobID, &p.Role, &p.Name, &p.Provider, &p.Model, &p.SortOrder) == nil {
			out = append(out, p)
		}
	}
	return out, nil
}

func (s *Storage) SaveCollaborationMessage(ctx context.Context, m CollaborationMessage) (*CollaborationMessage, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO orch_messages(job_id, round, turn_index, sender_role, sender_name, provider, model, action, content, completion_summary, raw_response, latency_ms)
		 VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.JobID, m.Round, m.TurnIndex, m.SenderRole, m.SenderName, m.Provider, m.Model, m.Action, m.Content, m.CompletionSummary, m.RawResponse, m.LatencyMs,
	)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	m.ID = id
	return &m, nil
}

func (s *Storage) GetCollaborationMessages(ctx context.Context, jobID string) ([]CollaborationMessage, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, job_id, round, turn_index, sender_role, sender_name, provider, model, action, content, completion_summary, raw_response, latency_ms, created_at
		 FROM orch_messages WHERE job_id=? ORDER BY turn_index ASC, id ASC`, jobID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []CollaborationMessage
	for rows.Next() {
		var m CollaborationMessage
		if rows.Scan(&m.ID, &m.JobID, &m.Round, &m.TurnIndex, &m.SenderRole, &m.SenderName, &m.Provider, &m.Model, &m.Action, &m.Content, &m.CompletionSummary, &m.RawResponse, &m.LatencyMs, &m.CreatedAt) == nil {
			out = append(out, m)
		}
	}
	return out, nil
}

func (s *Storage) RecordEvent(ctx context.Context, jobID string, eventType EventType, phase, workerID, message, payloadJSON string) error {
	if strings.TrimSpace(payloadJSON) == "" {
		payloadJSON = "{}"
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO orch_events(job_id, event_type, phase, worker_id, message, payload_json) VALUES(?, ?, ?, ?, ?, ?)`,
		jobID, string(eventType), phase, workerID, message, payloadJSON,
	)
	return err
}

func (s *Storage) GetJobEvents(ctx context.Context, jobID string) ([]Event, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, job_id, event_type, phase, worker_id, message, payload_json, created_at FROM orch_events WHERE job_id=? ORDER BY id ASC`, jobID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Event
	for rows.Next() {
		var ev Event
		if rows.Scan(&ev.ID, &ev.JobID, &ev.EventType, &ev.Phase, &ev.WorkerID, &ev.Message, &ev.PayloadJSON, &ev.CreatedAt) == nil {
			out = append(out, ev)
		}
	}
	return out, nil
}

func (s *Storage) UpdateJobStage(ctx context.Context, jobID string, stage WorkflowStage, round, totalCalls int) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE orch_jobs SET status='running', current_stage=?, current_round=?, total_calls=?, updated_at=CURRENT_TIMESTAMP WHERE id=?`,
		string(stage), round, totalCalls, jobID,
	)
	return err
}

func (s *Storage) CompleteJob(ctx context.Context, jobID, result string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE orch_jobs SET status='completed', current_stage='completed', execution_result=?, updated_at=CURRENT_TIMESTAMP WHERE id=?`,
		result, jobID,
	)
	return err
}

func (s *Storage) FailJob(ctx context.Context, jobID, errMsg string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE orch_jobs SET status='failed', current_stage='failed', error_message=?, updated_at=CURRENT_TIMESTAMP WHERE id=?`,
		errMsg, jobID,
	)
	return err
}

func (s *Storage) CancelJob(ctx context.Context, jobID string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE orch_jobs SET status='cancelled', current_stage='cancelled', updated_at=CURRENT_TIMESTAMP WHERE id=? AND status IN ('queued', 'running', 'waiting', 'paused')`,
		jobID,
	)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrJobNotFound
	}
	_ = s.RecordEvent(ctx, jobID, "job_cancelled", "cancelled", "user", "Job cancelled by user", "{}")
	return nil
}

func (s *Storage) PauseJob(ctx context.Context, jobID string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE orch_jobs SET status='paused', current_stage='paused', updated_at=CURRENT_TIMESTAMP WHERE id=? AND status='running'`,
		jobID,
	)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrJobNotFound
	}
	_ = s.RecordEvent(ctx, jobID, "job_paused", "paused", "user", "Job paused by user", "{}")
	return nil
}

func (s *Storage) RetryJob(ctx context.Context, jobID string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE orch_jobs SET status='queued', current_stage='queued', error_message='', updated_at=CURRENT_TIMESTAMP WHERE id=? AND status IN ('failed', 'cancelled', 'waiting', 'paused')`,
		jobID,
	)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrJobNotFound
	}
	_ = s.RecordEvent(ctx, jobID, "retry_scheduled", "queued", "user", "Job re-queued and resumed by user", "{}")
	return nil
}

func (s *Storage) IsJobCancelled(ctx context.Context, jobID string) bool {
	var st string
	err := s.db.QueryRowContext(ctx, `SELECT status FROM orch_jobs WHERE id=?`, jobID).Scan(&st)
	return err == nil && (st == "cancelled" || st == "failed")
}

func (s *Storage) IsJobPaused(ctx context.Context, jobID string) bool {
	var st string
	err := s.db.QueryRowContext(ctx, `SELECT status FROM orch_jobs WHERE id=?`, jobID).Scan(&st)
	return err == nil && st == "paused"
}

func (s *Storage) IterateJob(ctx context.Context, jobID, instruction string, extraRounds, extraCalls int) (*Job, error) {
	job, err := s.GetJob(ctx, jobID)
	if err != nil {
		return nil, err
	}

	maxRounds := job.MaxRounds + extraRounds
	maxCalls := job.MaxCalls + extraCalls

	history, err := s.GetCollaborationMessages(ctx, jobID)
	if err != nil {
		return nil, err
	}
	nextTurn := len(history) + 1

	_, err = s.SaveCollaborationMessage(ctx, CollaborationMessage{
		JobID:      jobID,
		Round:      job.CurrentRound + 1,
		TurnIndex:  nextTurn,
		SenderRole: RoleUser,
		SenderName: "Workspace Owner",
		Provider:   "human",
		Model:      "revision",
		Action:     ActionRevise,
		Content:    instruction,
	})
	if err != nil {
		return nil, err
	}

	_, err = s.db.ExecContext(ctx,
		`UPDATE orch_jobs SET status='queued', current_stage='queued', max_rounds=?, max_calls=?, error_message='', updated_at=CURRENT_TIMESTAMP WHERE id=?`,
		maxRounds, maxCalls, jobID,
	)
	if err != nil {
		return nil, err
	}

	_ = s.RecordEvent(ctx, jobID, "user_revision_requested", "queued", "user",
		fmt.Sprintf("User submitted revision instruction: %s", truncateString(instruction, 140)),
		marshalPayload(map[string]any{"instruction": instruction, "extra_rounds": extraRounds}))

	return s.GetJob(ctx, jobID)
}

func (s *Storage) ApplyAuditResult(ctx context.Context, jobID string, messageID int64) (*Job, error) {
	var auditContent string
	err := s.db.QueryRowContext(ctx, `SELECT content FROM orch_messages WHERE id=? AND job_id=? AND sender_role='auditor'`, messageID, jobID).Scan(&auditContent)
	if err != nil {
		return nil, errors.New("auditor report message not found")
	}

	history, err := s.GetCollaborationMessages(ctx, jobID)
	if err != nil {
		return nil, err
	}
	nextTurn := len(history) + 1
	job, err := s.GetJob(ctx, jobID)
	if err != nil {
		return nil, err
	}

	_, _ = s.SaveCollaborationMessage(ctx, CollaborationMessage{
		JobID:      jobID,
		Round:      job.CurrentRound,
		TurnIndex:  nextTurn,
		SenderRole: RoleWorker,
		SenderName: "Worker / Developer (Applied Auditor Fixes)",
		Provider:   "auditor-apply",
		Model:      "fix-applied",
		Action:     ActionContinue,
		Content:    auditContent,
	})

	_, _ = s.db.ExecContext(ctx, `UPDATE orch_jobs SET execution_result=?, updated_at=CURRENT_TIMESTAMP WHERE id=?`, auditContent, jobID)
	_ = s.RecordEvent(ctx, jobID, "audit_applied", "completed", "user", "Independent auditor corrected files applied to project deliverable", "{}")

	return s.GetJob(ctx, jobID)
}

func truncateString(s string, max int) string {
	s = strings.TrimSpace(s)
	if len([]rune(s)) <= max {
		return s
	}
	return string([]rune(s)[:max]) + "…"
}
