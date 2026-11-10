package orchestration

import "time"

// JobStatus represents the lifecycle state of an orchestration job
type JobStatus string

const (
	StatusQueued    JobStatus = "queued"
	StatusRunning   JobStatus = "running"
	StatusWaiting   JobStatus = "waiting"
	StatusCompleted JobStatus = "completed"
	StatusFailed    JobStatus = "failed"
	StatusCancelled JobStatus = "cancelled"
)

// WorkflowStage tracks the exact step inside the collaboration state machine
type WorkflowStage string

const (
	StageQueued    WorkflowStage = "queued"
	StagePlanning  WorkflowStage = "planning"
	StageWorking   WorkflowStage = "working"
	StageReviewing WorkflowStage = "reviewing"
	StageCompleted WorkflowStage = "completed"
	StageFailed    WorkflowStage = "failed"
	StageCancelled WorkflowStage = "cancelled"
)

// EventType represents the durable event category in the audit trail
type EventType string

const (
	EventJobCreated         EventType = "job_created"
	EventJobClaimed         EventType = "job_claimed"
	EventJobStarted         EventType = "job_started"
	EventJobWaiting         EventType = "job_waiting"
	EventRetryScheduled     EventType = "retry_scheduled"
	EventJobFailed          EventType = "job_failed"
	EventJobCompleted       EventType = "job_completed"
	EventJobCancelled       EventType = "job_cancelled"
	EventWorkerRecovery     EventType = "worker_recovery"
	EventStepProgress       EventType = "step_progress"
	EventModelInvoked       EventType = "model_invoked"
	EventModelResponded     EventType = "model_responded"
	EventReviewDecision     EventType = "review_decision"
	EventCoordinatorStarted EventType = "coordinator_started"
	EventPlanGenerated      EventType = "plan_generated"
	EventWorkerStarted      EventType = "worker_started"
	EventWorkerResponded    EventType = "worker_response_received"
	EventReviewStarted      EventType = "review_started"
	EventRevisionRequested  EventType = "revision_requested"
	EventRevisionCompleted  EventType = "revision_completed"
	EventTaskCompleted      EventType = "task_completed"
	EventTaskFailed         EventType = "task_failed"
	EventTaskCancelled      EventType = "task_cancelled"
)

// Participant Roles
const (
	RoleCoordinator = "coordinator"
	RoleWorker      = "worker"
)

// CollaborationAction represents a validated model decision
type CollaborationAction string

const (
	ActionContinue CollaborationAction = "continue"
	ActionRevise   CollaborationAction = "revise"
	ActionComplete CollaborationAction = "complete"
)

// Job represents a persistent orchestration workflow execution unit
type Job struct {
	ID              string        `json:"id"`
	Task            string        `json:"task"`
	Status          JobStatus     `json:"status"`
	CurrentStage    WorkflowStage `json:"current_stage"`
	MaxRounds       int           `json:"max_rounds"`
	MaxCalls        int           `json:"max_calls"`
	CurrentRound    int           `json:"current_round"`
	TotalCalls      int           `json:"total_calls"`
	RetryCount      int           `json:"retry_count"`
	MaxRetries      int           `json:"max_retries"`
	ClaimedBy       string        `json:"claimed_by,omitempty"`
	LeaseExpiresAt  *time.Time    `json:"lease_expires_at,omitempty"`
	Metadata        string        `json:"metadata"`
	ExecutionResult string        `json:"execution_result,omitempty"`
	ErrorMessage    string        `json:"error_message,omitempty"`
	CreatedAt       time.Time     `json:"created_at"`
	UpdatedAt       time.Time     `json:"updated_at"`
	StartedAt       *time.Time    `json:"started_at,omitempty"`
	CompletedAt     *time.Time    `json:"completed_at,omitempty"`
}

// JobDetails bundles the job, participants, messages, and events for inspection
type JobDetails struct {
	*Job
	Participants []Participant          `json:"participants"`
	Messages     []CollaborationMessage `json:"messages"`
	Events       []Event                `json:"events"`
}

// Participant represents an AI model assigned to a specific collaboration role
type Participant struct {
	ID           int64     `json:"id"`
	JobID        string    `json:"job_id"`
	Role         string    `json:"role"` // "coordinator", "worker", or additional roles
	Name         string    `json:"name"`
	Provider     string    `json:"provider"`
	Model        string    `json:"model"`
	SystemPrompt string    `json:"system_prompt,omitempty"`
	SortOrder    int       `json:"sort_order"`
	CreatedAt    time.Time `json:"created_at"`
}

// CollaborationMessage stores a single turn in the AI-to-AI collaboration transcript
type CollaborationMessage struct {
	ID                int64               `json:"id"`
	JobID             string              `json:"job_id"`
	Round             int                 `json:"round"`
	TurnIndex         int                 `json:"turn_index"`
	SenderRole        string              `json:"sender_role"`
	SenderName        string              `json:"sender_name"`
	Provider          string              `json:"provider"`
	Model             string              `json:"model"`
	Action            CollaborationAction `json:"action"`
	Content           string              `json:"content"`
	CompletionSummary string              `json:"completion_summary,omitempty"`
	RawResponse       string              `json:"raw_response,omitempty"`
	LatencyMs         int64               `json:"latency_ms"`
	CreatedAt         time.Time           `json:"created_at"`
}

// StructuredModelResponse is the expected structured output format from models
type StructuredModelResponse struct {
	Action            CollaborationAction `json:"action"`
	Message           string              `json:"message"`
	CompletionSummary string              `json:"completion_summary"`
}

// JobMetadata represents optional configuration stored in Job.Metadata
type JobMetadata struct {
	Participants   []Participant `json:"participants,omitempty"`
	TimeoutSeconds int           `json:"timeout_seconds,omitempty"`
	MaxContextChar int           `json:"max_context_chars,omitempty"`
}

// Event represents an immutable timestamped execution step
type Event struct {
	ID        int64     `json:"id"`
	JobID     string    `json:"job_id"`
	EventType EventType `json:"event_type"`
	Phase     string    `json:"phase"`
	WorkerID  string    `json:"worker_id,omitempty"`
	Message   string    `json:"message"`
	Payload   string    `json:"payload"`
	CreatedAt time.Time `json:"created_at"`
}

// WorkerLease tracks background worker health
type WorkerLease struct {
	WorkerID      string    `json:"worker_id"`
	LastHeartbeat time.Time `json:"last_heartbeat"`
	ActiveJobID   string    `json:"active_job_id,omitempty"`
	Status        string    `json:"status"`
}
