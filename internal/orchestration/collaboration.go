package orchestration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sahand363/sahand-cortext/internal/provider"
)

// KeyResolverFunc looks up the backend API key for a given provider ID
type KeyResolverFunc func(providerID string) string

// DefaultCollaboratorFunc returns default (provider, model) when none are specified in the job
type DefaultCollaboratorFunc func() (providerID, modelID string)

// CollaborationConfig holds configurable safety limits for the collaboration loop
type CollaborationConfig struct {
	DefaultMaxRounds int
	DefaultMaxCalls  int
	MaxContextChars  int
	CallTimeout      time.Duration
}

func DefaultCollaborationConfig() CollaborationConfig {
	return CollaborationConfig{
		DefaultMaxRounds: 5,
		DefaultMaxCalls:  12,
		MaxContextChars:  28000,
		CallTimeout:      90 * time.Second,
	}
}

// IsRetryableProviderError classifies whether an error should trigger a background job retry
// (e.g., HTTP 429, 5xx, timeouts, network drops) vs fail permanently (e.g., HTTP 401/403/404, missing API key).
func IsRetryableProviderError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, ErrJobCancelled) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "missing api key") ||
		strings.Contains(msg, "http 400") ||
		strings.Contains(msg, "http 401") ||
		strings.Contains(msg, "http 403") ||
		strings.Contains(msg, "http 404") ||
		strings.Contains(msg, "invalid api key") ||
		strings.Contains(msg, "maximum model calls") {
		return false
	}
	if strings.Contains(msg, "http 429") ||
		strings.Contains(msg, "http 500") ||
		strings.Contains(msg, "http 502") ||
		strings.Contains(msg, "http 503") ||
		strings.Contains(msg, "http 504") ||
		strings.Contains(msg, "timeout") ||
		strings.Contains(msg, "deadline exceeded") ||
		strings.Contains(msg, "connection reset") ||
		strings.Contains(msg, "temporary") ||
		strings.Contains(msg, "eof") ||
		strings.Contains(msg, "network error") {
		return true
	}
	return false
}

// CollaborationEngine implements JobProcessor to run multi-model collaboration
type CollaborationEngine struct {
	storage         *Storage
	router          *provider.Router
	keyResolver     KeyResolverFunc
	defaultProvider DefaultCollaboratorFunc
	config          CollaborationConfig
}

func NewCollaborationEngine(
	storage *Storage,
	router *provider.Router,
	keyResolver KeyResolverFunc,
	defaultProvider DefaultCollaboratorFunc,
	cfg CollaborationConfig,
) *CollaborationEngine {
	if cfg.DefaultMaxRounds <= 0 {
		cfg.DefaultMaxRounds = 5
	}
	if cfg.DefaultMaxCalls <= 0 {
		cfg.DefaultMaxCalls = 12
	}
	if cfg.MaxContextChars <= 0 {
		cfg.MaxContextChars = 28000
	}
	if cfg.CallTimeout <= 0 {
		cfg.CallTimeout = 90 * time.Second
	}
	return &CollaborationEngine{
		storage:         storage,
		router:          router,
		keyResolver:     keyResolver,
		defaultProvider: defaultProvider,
		config:          cfg,
	}
}

func (e *CollaborationEngine) checkCancelled(ctx context.Context, jobID string, helper ExecutionHelper) bool {
	if ctx.Err() != nil || helper.IsCancelled() {
		return true
	}
	return e.storage.IsJobCancelled(context.Background(), jobID)
}

// ProcessJob implements the JobProcessor interface for WorkerPool.
// It supports full turn-level state reconstruction so interrupted or retried jobs resume
// from the exact round and step where they left off without repeating completed API calls.
func (e *CollaborationEngine) ProcessJob(ctx context.Context, job *Job, helper ExecutionHelper) (string, error) {
	if e.checkCancelled(ctx, job.ID, helper) {
		return "", ErrJobCancelled
	}
	if e.router == nil {
		return fmt.Sprintf("Orchestration infrastructure processed task: %s", job.Task), nil
	}

	participants, metaCfg, err := e.resolveParticipants(ctx, job)
	if err != nil {
		return "", err
	}

	// If no participants were configured and no default API key exists, fall back to baseline processor
	if len(participants) == 0 {
		helper.RecordProgress("working", "Processing orchestration baseline task", "{}")
		return fmt.Sprintf("Orchestration infrastructure processed task: %s", job.Task), nil
	}

	var coordinator Participant
	var workers []Participant
	for _, p := range participants {
		if strings.ToLower(p.Role) == RoleCoordinator && coordinator.Model == "" {
			coordinator = p
		} else {
			workers = append(workers, p)
		}
	}
	if coordinator.Model == "" {
		coordinator = participants[0]
		coordinator.Role = RoleCoordinator
	}
	if len(workers) == 0 {
		w := coordinator
		w.Role = RoleWorker
		if w.Name == "" || w.Name == "Coordinator" {
			w.Name = "Worker"
		}
		workers = append(workers, w)
	}

	// Validate credentials up front before making network calls
	for _, p := range append([]Participant{coordinator}, workers...) {
		if strings.TrimSpace(e.keyResolver(p.Provider)) == "" {
			return "", fmt.Errorf("missing API key for participant %s (%s / %s)", p.Role, p.Provider, p.Model)
		}
	}

	maxRounds := job.MaxRounds
	if maxRounds <= 0 {
		maxRounds = e.config.DefaultMaxRounds
	}
	maxCalls := job.MaxCalls
	if maxCalls <= 0 {
		maxCalls = e.config.DefaultMaxCalls
	}
	maxChars := e.config.MaxContextChars
	if metaCfg.MaxContextChar > 0 {
		maxChars = metaCfg.MaxContextChar
	}
	callTimeout := e.config.CallTimeout
	if metaCfg.TimeoutSeconds > 0 {
		callTimeout = time.Duration(metaCfg.TimeoutSeconds) * time.Second
	}

	// Reconstruct durable state from any previously saved messages in SQLite
	history, err := e.storage.GetCollaborationMessages(ctx, job.ID)
	if err != nil {
		return "", fmt.Errorf("failed loading collaboration history: %w", err)
	}
	totalCalls := len(history)
	turnIndex := totalCalls

	var currentPlan string
	var latestWorkerResult string

	// Inspect history to determine what has already been completed
	for i, m := range history {
		if i == 0 && m.SenderRole == RoleCoordinator {
			currentPlan = m.Content
			if m.Action == ActionComplete {
				finalOut := m.CompletionSummary
				if strings.TrimSpace(finalOut) == "" {
					finalOut = m.Content
				}
				return finalOut, nil
			}
		}
		if m.SenderRole == RoleWorker {
			latestWorkerResult = m.Content
		}
		if i > 0 && m.SenderRole == RoleCoordinator && m.Action == ActionComplete {
			finalResult := latestWorkerResult
			if strings.TrimSpace(m.CompletionSummary) != "" {
				finalResult = fmt.Sprintf("%s\n\n---\n### Coordinator Completion Summary\n%s", latestWorkerResult, strings.TrimSpace(m.CompletionSummary))
			}
			return finalResult, nil
		}
	}

	// Step 1: Coordinator creates the implementation plan (if not already present from recovery)
	if currentPlan == "" {
		if e.checkCancelled(ctx, job.ID, helper) {
			return "", ErrJobCancelled
		}
		if totalCalls >= maxCalls {
			return "", errors.New("maximum model calls reached before planning")
		}

		_ = e.storage.UpdateJobStage(ctx, job.ID, StagePlanning, 1, totalCalls)
		_ = e.storage.RecordEvent(ctx, job.ID, EventCoordinatorStarted, string(StagePlanning), helper.WorkerID(),
			fmt.Sprintf("Coordinator (%s/%s) started analyzing task and building plan", coordinator.Provider, coordinator.Model),
			marshalPayload(map[string]any{"round": 1, "role": RoleCoordinator, "provider": coordinator.Provider, "model": coordinator.Model}))

		planPrompt := BuildCoordinatorPlanContext(job.Task, coordinator, maxChars)
		parsed, raw, latency, err := e.invokeParticipant(ctx, job.ID, 1, string(StagePlanning), coordinator, planPrompt, callTimeout, helper)
		if err != nil {
			return "", fmt.Errorf("coordinator planning failed: %w", err)
		}

		if e.checkCancelled(ctx, job.ID, helper) {
			return "", ErrJobCancelled
		}

		turnIndex++
		totalCalls++
		currentPlan = parsed.Message

		savedMsg, err := e.storage.SaveCollaborationMessage(ctx, CollaborationMessage{
			JobID:             job.ID,
			Round:             1,
			TurnIndex:         turnIndex,
			SenderRole:        RoleCoordinator,
			SenderName:        coordinator.Name,
			Provider:          coordinator.Provider,
			Model:             coordinator.Model,
			Action:            parsed.Action,
			Content:           parsed.Message,
			CompletionSummary: parsed.CompletionSummary,
			RawResponse:       raw,
			LatencyMs:         latency,
		})
		if err != nil {
			return "", err
		}
		history = append(history, *savedMsg)

		_ = e.storage.RecordEvent(ctx, job.ID, EventPlanGenerated, string(StagePlanning), helper.WorkerID(),
			fmt.Sprintf("Coordinator generated implementation plan (%d ms)", latency),
			marshalPayload(map[string]any{"round": 1, "action": parsed.Action, "latency_ms": latency}))

		// If the Coordinator immediately marked the task complete during planning
		if parsed.Action == ActionComplete {
			finalOut := parsed.CompletionSummary
			if strings.TrimSpace(finalOut) == "" {
				finalOut = parsed.Message
			}
			_ = e.storage.UpdateJobStage(ctx, job.ID, StageCompleted, 1, totalCalls)
			_ = e.storage.RecordEvent(ctx, job.ID, EventTaskCompleted, string(StageCompleted), helper.WorkerID(),
				"Task completed during initial coordinator planning",
				marshalPayload(map[string]any{"round": 1, "total_calls": totalCalls}))
			return finalOut, nil
		}
	}

	// Step 2: Multi-round collaboration loop (Worker executes -> Coordinator reviews)
	for round := 1; round <= maxRounds; round++ {
		if e.checkCancelled(ctx, job.ID, helper) {
			return "", ErrJobCancelled
		}

		// Check if Worker and/or Coordinator Review already ran for this round prior to a restart
		hasWorkerForRound := false
		hasReviewForRound := false
		for idx, m := range history {
			if idx == 0 {
				continue // skip initial plan
			}
			if m.Round == round && m.SenderRole == RoleWorker {
				hasWorkerForRound = true
				latestWorkerResult = m.Content
			}
			if m.Round == round && m.SenderRole == RoleCoordinator {
				hasReviewForRound = true
			}
		}

		// If both Worker and Coordinator Review were already persisted for this round, advance to next round
		if hasWorkerForRound && hasReviewForRound {
			continue
		}

		activeWorker := workers[(round-1)%len(workers)]

		// Phase A: Worker performs work or revises based on Coordinator review (unless already persisted)
		if !hasWorkerForRound {
			if totalCalls >= maxCalls {
				return e.finalizeAtLimit(ctx, job.ID, round, totalCalls, "maximum model calls limit reached", latestWorkerResult, currentPlan, helper), nil
			}

			_ = e.storage.UpdateJobStage(ctx, job.ID, StageWorking, round, totalCalls)
			_ = e.storage.RecordEvent(ctx, job.ID, EventWorkerStarted, string(StageWorking), helper.WorkerID(),
				fmt.Sprintf("Worker (%s/%s) started round %d execution", activeWorker.Provider, activeWorker.Model, round),
				marshalPayload(map[string]any{"round": round, "role": RoleWorker, "provider": activeWorker.Provider, "model": activeWorker.Model}))

			workerCtx := BuildWorkerExecutionContext(job.Task, currentPlan, history, activeWorker, round, maxChars)
			workerParsed, workerRaw, workerLat, err := e.invokeParticipant(ctx, job.ID, round, string(StageWorking), activeWorker, workerCtx, callTimeout, helper)
			if err != nil {
				return "", fmt.Errorf("worker execution failed in round %d: %w", round, err)
			}

			if e.checkCancelled(ctx, job.ID, helper) {
				return "", ErrJobCancelled
			}

			turnIndex++
			totalCalls++
			latestWorkerResult = workerParsed.Message

			savedWork, err := e.storage.SaveCollaborationMessage(ctx, CollaborationMessage{
				JobID:             job.ID,
				Round:             round,
				TurnIndex:         turnIndex,
				SenderRole:        RoleWorker,
				SenderName:        activeWorker.Name,
				Provider:          activeWorker.Provider,
				Model:             activeWorker.Model,
				Action:            workerParsed.Action,
				Content:           workerParsed.Message,
				CompletionSummary: workerParsed.CompletionSummary,
				RawResponse:       workerRaw,
				LatencyMs:         workerLat,
			})
			if err != nil {
				return "", err
			}
			history = append(history, *savedWork)

			_ = e.storage.RecordEvent(ctx, job.ID, EventWorkerResponded, string(StageWorking), helper.WorkerID(),
				fmt.Sprintf("Worker completed round %d deliverable (%d ms)", round, workerLat),
				marshalPayload(map[string]any{"round": round, "latency_ms": workerLat, "action": workerParsed.Action}))

			if round > 1 {
				_ = e.storage.RecordEvent(ctx, job.ID, EventRevisionCompleted, string(StageWorking), helper.WorkerID(),
					fmt.Sprintf("Worker completed requested revision for round %d", round),
					marshalPayload(map[string]any{"round": round, "latency_ms": workerLat}))
			}
		}

		if e.checkCancelled(ctx, job.ID, helper) {
			return "", ErrJobCancelled
		}

		// Check call budget before Coordinator review
		if totalCalls >= maxCalls {
			return e.finalizeAtLimit(ctx, job.ID, round, totalCalls, "maximum model calls limit reached after worker turn", latestWorkerResult, currentPlan, helper), nil
		}

		// Phase B: Coordinator reviews the Worker's output
		_ = e.storage.UpdateJobStage(ctx, job.ID, StageReviewing, round, totalCalls)
		_ = e.storage.RecordEvent(ctx, job.ID, EventReviewStarted, string(StageReviewing), helper.WorkerID(),
			fmt.Sprintf("Coordinator (%s/%s) started reviewing round %d output", coordinator.Provider, coordinator.Model, round),
			marshalPayload(map[string]any{"round": round, "role": RoleCoordinator, "provider": coordinator.Provider, "model": coordinator.Model}))

		reviewCtx := BuildCoordinatorReviewContext(job.Task, currentPlan, latestWorkerResult, history, coordinator, round, maxRounds, maxChars)
		reviewParsed, reviewRaw, reviewLat, err := e.invokeParticipant(ctx, job.ID, round, string(StageReviewing), coordinator, reviewCtx, callTimeout, helper)
		if err != nil {
			return "", fmt.Errorf("coordinator review failed in round %d: %w", round, err)
		}

		if e.checkCancelled(ctx, job.ID, helper) {
			return "", ErrJobCancelled
		}

		turnIndex++
		totalCalls++

		savedReview, err := e.storage.SaveCollaborationMessage(ctx, CollaborationMessage{
			JobID:             job.ID,
			Round:             round,
			TurnIndex:         turnIndex,
			SenderRole:        RoleCoordinator,
			SenderName:        coordinator.Name,
			Provider:          coordinator.Provider,
			Model:             coordinator.Model,
			Action:            reviewParsed.Action,
			Content:           reviewParsed.Message,
			CompletionSummary: reviewParsed.CompletionSummary,
			RawResponse:       reviewRaw,
			LatencyMs:         reviewLat,
		})
		if err != nil {
			return "", err
		}
		history = append(history, *savedReview)

		decisionPayload := marshalPayload(map[string]any{
			"round":              round,
			"action":             reviewParsed.Action,
			"completion_summary": reviewParsed.CompletionSummary,
			"latency_ms":         reviewLat,
		})
		_ = e.storage.RecordEvent(ctx, job.ID, EventReviewDecision, string(StageReviewing), helper.WorkerID(),
			fmt.Sprintf("Coordinator decision in round %d: %s", round, reviewParsed.Action), decisionPayload)

		// Complete if Coordinator accepted the result
		if reviewParsed.Action == ActionComplete {
			finalResult := latestWorkerResult
			if strings.TrimSpace(reviewParsed.CompletionSummary) != "" {
				finalResult = fmt.Sprintf("%s\n\n---\n### Coordinator Completion Summary\n%s", latestWorkerResult, strings.TrimSpace(reviewParsed.CompletionSummary))
			}
			_ = e.storage.UpdateJobStage(ctx, job.ID, StageCompleted, round, totalCalls)
			_ = e.storage.RecordEvent(ctx, job.ID, EventTaskCompleted, string(StageCompleted), helper.WorkerID(),
				fmt.Sprintf("Coordinator approved deliverable in round %d", round),
				marshalPayload(map[string]any{"round": round, "total_calls": totalCalls}))
			return finalResult, nil
		}

		// Revision or continuation requested
		_ = e.storage.RecordEvent(ctx, job.ID, EventRevisionRequested, string(StageReviewing), helper.WorkerID(),
			fmt.Sprintf("Coordinator requested revision in round %d", round), decisionPayload)
	}

	// Reached maximum collaboration rounds
	return e.finalizeAtLimit(ctx, job.ID, maxRounds, totalCalls, "maximum collaboration rounds reached", latestWorkerResult, currentPlan, helper), nil
}

func (e *CollaborationEngine) finalizeAtLimit(ctx context.Context, jobID string, round, totalCalls int, reason, latestWorkerResult, currentPlan string, helper ExecutionHelper) string {
	payload := marshalPayload(map[string]any{
		"reason":      reason,
		"round":       round,
		"total_calls": totalCalls,
	})
	_ = e.storage.UpdateJobStage(ctx, jobID, StageCompleted, round, totalCalls)
	_ = e.storage.RecordEvent(ctx, jobID, EventTaskCompleted, string(StageCompleted), helper.WorkerID(),
		fmt.Sprintf("Collaboration concluded (%s)", reason), payload)

	if strings.TrimSpace(latestWorkerResult) != "" {
		return latestWorkerResult
	}
	return currentPlan
}

func (e *CollaborationEngine) invokeParticipant(
	ctx context.Context,
	jobID string,
	round int,
	phase string,
	participant Participant,
	messages []provider.Message,
	timeout time.Duration,
	helper ExecutionHelper,
) (StructuredModelResponse, string, int64, error) {
	if e.checkCancelled(ctx, jobID, helper) {
		return StructuredModelResponse{}, "", 0, ErrJobCancelled
	}

	key := e.keyResolver(participant.Provider)
	if strings.TrimSpace(key) == "" {
		return StructuredModelResponse{}, "", 0, fmt.Errorf("missing API key for provider %s", participant.Provider)
	}

	invokePayload := marshalPayload(map[string]any{
		"round":    round,
		"role":     participant.Role,
		"name":     participant.Name,
		"provider": participant.Provider,
		"model":    participant.Model,
	})
	_ = e.storage.RecordEvent(ctx, jobID, EventModelInvoked, phase, helper.WorkerID(),
		fmt.Sprintf("Invoking %s (%s/%s)", participant.Role, participant.Provider, participant.Model), invokePayload)

	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// External network call executes strictly outside any SQLite transaction
	rawReply, latency, err := e.router.Call(callCtx, participant.Provider, key, participant.Model, messages, provider.CallOpts{})
	if err != nil {
		return StructuredModelResponse{}, "", latency, err
	}

	defaultAction := ActionContinue
	if phase == string(StageReviewing) {
		defaultAction = ActionRevise
	}
	parsed := ParseStructuredResponse(rawReply, defaultAction)

	respPayload := marshalPayload(map[string]any{
		"round":      round,
		"role":       participant.Role,
		"provider":   participant.Provider,
		"model":      participant.Model,
		"action":     parsed.Action,
		"latency_ms": latency,
	})
	_ = e.storage.RecordEvent(ctx, jobID, EventModelResponded, phase, helper.WorkerID(),
		fmt.Sprintf("Received response from %s (%d ms, action=%s)", participant.Role, latency, parsed.Action), respPayload)

	return parsed, rawReply, latency, nil
}

func (e *CollaborationEngine) resolveParticipants(ctx context.Context, job *Job) ([]Participant, JobMetadata, error) {
	var meta JobMetadata
	if strings.TrimSpace(job.Metadata) != "" && job.Metadata != "{}" {
		_ = json.Unmarshal([]byte(job.Metadata), &meta)
	}

	parts, err := e.storage.GetParticipants(ctx, job.ID)
	if err != nil {
		return nil, meta, err
	}

	if len(parts) == 0 && len(meta.Participants) > 0 {
		if err := e.storage.SaveParticipants(ctx, job.ID, meta.Participants); err == nil {
			parts, _ = e.storage.GetParticipants(ctx, job.ID)
		} else {
			parts = meta.Participants
		}
	}

	if len(parts) == 0 && e.defaultProvider != nil {
		prov, mod := e.defaultProvider()
		if prov != "" && mod != "" && strings.TrimSpace(e.keyResolver(prov)) != "" {
			defaultParts := []Participant{
				{JobID: job.ID, Role: RoleCoordinator, Name: "Coordinator", Provider: prov, Model: mod, SortOrder: 0},
				{JobID: job.ID, Role: RoleWorker, Name: "Worker", Provider: prov, Model: mod, SortOrder: 1},
			}
			_ = e.storage.SaveParticipants(ctx, job.ID, defaultParts)
			parts, _ = e.storage.GetParticipants(ctx, job.ID)
		}
	}

	return parts, meta, nil
}

func marshalPayload(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// BuildCoordinatorPlanContext constructs the bounded context for the Coordinator's initial plan
func BuildCoordinatorPlanContext(task string, coordinator Participant, maxChars int) []provider.Message {
	baseSys := strings.TrimSpace(coordinator.SystemPrompt)
	if baseSys == "" {
		baseSys = "You are the Coordinator / Architect in a multi-model AI collaboration managed by Sahand Cortex."
	}

	sysPrompt := baseSys + `

Your responsibilities:
1. Analyze the original user task.
2. Formulate a clear, structured implementation plan and explicit instructions for the Worker / Developer.
3. Respond using ONLY valid JSON matching this schema:
{
  "action": "continue",
  "message": "Your detailed implementation plan and instructions for the Worker",
  "completion_summary": ""
}
Allowed values for "action": "continue" (send plan to Worker) or "complete" (only if the task is trivial and already answered in message).`

	userPrompt := fmt.Sprintf("### Original User Task:\n%s\n\nCreate the implementation plan and instructions for the Worker in the required JSON format.", truncateText(task, maxChars/2))

	return []provider.Message{
		{Role: "system", Content: sysPrompt},
		{Role: "user", Content: userPrompt},
	}
}

// BuildWorkerExecutionContext constructs bounded context for the Worker's execution or revision turn
func BuildWorkerExecutionContext(task, plan string, history []CollaborationMessage, worker Participant, round int, maxChars int) []provider.Message {
	if maxChars <= 0 {
		maxChars = 28000
	}
	baseSys := strings.TrimSpace(worker.SystemPrompt)
	if baseSys == "" {
		baseSys = "You are the Worker / Developer in a multi-model AI collaboration managed by Sahand Cortex."
	}

	sysPrompt := baseSys + `

Your responsibilities:
1. Follow the Coordinator's implementation plan and review instructions carefully.
2. Produce complete, accurate, high-quality work.
3. Respond using ONLY valid JSON matching this schema:
{
  "action": "continue",
  "message": "Your complete implementation, code, or analysis deliverable",
  "completion_summary": "Brief summary of what you produced or revised"
}`

	taskBudget := maxChars / 4
	planBudget := maxChars / 4
	historyBudget := maxChars - taskBudget - planBudget

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("### Original User Task:\n%s\n\n", truncateText(task, taskBudget)))
	sb.WriteString(fmt.Sprintf("### Coordinator Implementation Plan:\n%s\n\n", truncateText(plan, planBudget)))

	if len(history) > 1 {
		sb.WriteString(formatBoundedHistory(history, historyBudget))
	}

	if round > 1 {
		sb.WriteString("\nAddress all revision requests from the Coordinator's latest review and provide the complete updated deliverable in the required JSON format.")
	} else {
		sb.WriteString("\nExecute the Coordinator's plan now and provide your complete result in the required JSON format.")
	}

	return []provider.Message{
		{Role: "system", Content: sysPrompt},
		{Role: "user", Content: sb.String()},
	}
}

// BuildCoordinatorReviewContext constructs bounded context for the Coordinator's review turn
func BuildCoordinatorReviewContext(task, plan, latestWorkerResult string, history []CollaborationMessage, coordinator Participant, round, maxRounds, maxChars int) []provider.Message {
	if maxChars <= 0 {
		maxChars = 28000
	}
	baseSys := strings.TrimSpace(coordinator.SystemPrompt)
	if baseSys == "" {
		baseSys = "You are the Coordinator / Architect reviewing the Worker's deliverable in Sahand Cortex."
	}

	sysPrompt := baseSys + `

Your responsibilities:
1. Evaluate the Worker's deliverable against the original user task and your implementation plan.
2. Decide whether to:
   - Accept the result and complete the task ("action": "complete")
   - Request specific revisions ("action": "revise")
   - Ask for additional work ("action": "continue")
3. Respond using ONLY valid JSON matching this schema:
{
  "action": "complete | revise | continue",
  "message": "Your detailed review, critique, or specific revision instructions",
  "completion_summary": "Final summary if action is complete, otherwise empty"
}`

	taskBudget := maxChars / 5
	planBudget := maxChars / 5
	workerBudget := maxChars / 2
	historyBudget := maxChars - taskBudget - planBudget - workerBudget

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("### Original User Task:\n%s\n\n", truncateText(task, taskBudget)))
	sb.WriteString(fmt.Sprintf("### Implementation Plan:\n%s\n\n", truncateText(plan, planBudget)))
	if len(history) > 2 && historyBudget > 500 {
		sb.WriteString(formatBoundedHistory(history[:len(history)-1], historyBudget))
		sb.WriteString("\n")
	}
	sb.WriteString(fmt.Sprintf("### Latest Worker Result (Round %d of %d):\n%s\n\n", round, maxRounds, truncateText(latestWorkerResult, workerBudget)))
	sb.WriteString("Review the Worker's deliverable and respond with your decision in the required JSON format.")

	return []provider.Message{
		{Role: "system", Content: sysPrompt},
		{Role: "user", Content: sb.String()},
	}
}

func formatBoundedHistory(history []CollaborationMessage, budget int) string {
	if len(history) == 0 || budget <= 0 {
		return ""
	}
	var chunks []string
	used := 0
	for i := len(history) - 1; i >= 0; i-- {
		m := history[i]
		entry := fmt.Sprintf("[Round %d · %s (%s/%s) · Action: %s]\n%s\n",
			m.Round, strings.ToUpper(m.SenderRole), m.Provider, m.Model, m.Action, truncateText(m.Content, budget/2))
		if used+len(entry) > budget && len(chunks) > 0 {
			break
		}
		chunks = append([]string{entry}, chunks...)
		used += len(entry)
	}
	return "### Recent Collaboration History:\n" + strings.Join(chunks, "\n")
}

func truncateText(s string, maxChars int) string {
	s = strings.TrimSpace(s)
	if maxChars <= 0 || len(s) <= maxChars {
		return s
	}
	return s[:maxChars] + "\n...[truncated]..."
}

// ParseStructuredResponse safely parses and validates structured JSON from any model.
func ParseStructuredResponse(raw string, defaultAction CollaborationAction) StructuredModelResponse {
	if defaultAction == "" {
		defaultAction = ActionContinue
	}
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return StructuredModelResponse{
			Action:  defaultAction,
			Message: "(empty response)",
		}
	}

	candidates := extractJSONCandidates(trimmed)
	for _, c := range candidates {
		var parsed struct {
			Action            string `json:"action"`
			Decision          string `json:"decision"`
			Message           string `json:"message"`
			Content           string `json:"content"`
			CompletionSummary string `json:"completion_summary"`
			Summary           string `json:"summary"`
		}
		if err := json.Unmarshal([]byte(c), &parsed); err == nil {
			actRaw := strings.ToLower(strings.TrimSpace(parsed.Action))
			if actRaw == "" {
				actRaw = strings.ToLower(strings.TrimSpace(parsed.Decision))
			}
			msg := strings.TrimSpace(parsed.Message)
			if msg == "" {
				msg = strings.TrimSpace(parsed.Content)
			}
			summary := strings.TrimSpace(parsed.CompletionSummary)
			if summary == "" {
				summary = strings.TrimSpace(parsed.Summary)
			}

			action := normalizeAction(actRaw, defaultAction)
			if msg == "" && summary != "" {
				msg = summary
			}
			if msg == "" {
				msg = trimmed
			}
			return StructuredModelResponse{
				Action:            action,
				Message:           msg,
				CompletionSummary: summary,
			}
		}
	}

	inferredAction := defaultAction
	lower := strings.ToLower(trimmed)
	if strings.Contains(lower, "decision: complete") || strings.Contains(lower, `"action": "complete"`) || strings.Contains(lower, `"action":"complete"`) {
		inferredAction = ActionComplete
	} else if strings.Contains(lower, "decision: revise") || strings.Contains(lower, `"action": "revise"`) || strings.Contains(lower, `"action":"revise"`) {
		inferredAction = ActionRevise
	} else if strings.Contains(lower, "decision: continue") || strings.Contains(lower, `"action": "continue"`) || strings.Contains(lower, `"action":"continue"`) {
		inferredAction = ActionContinue
	}

	var summary string
	if inferredAction == ActionComplete {
		summary = truncateText(trimmed, 600)
	}

	return StructuredModelResponse{
		Action:            inferredAction,
		Message:           trimmed,
		CompletionSummary: summary,
	}
}

func normalizeAction(act string, fallback CollaborationAction) CollaborationAction {
	switch strings.ToLower(strings.TrimSpace(act)) {
	case "complete", "completed", "done", "accept", "accepted", "finish":
		return ActionComplete
	case "revise", "revision", "reject", "changes_requested", "retry":
		return ActionRevise
	case "continue", "proceed", "next", "work":
		return ActionContinue
	default:
		return fallback
	}
}

func extractJSONCandidates(s string) []string {
	var out []string
	out = append(out, s)

	if idx := strings.Index(s, "```"); idx != -1 {
		rest := s[idx+3:]
		if nl := strings.Index(rest, "\n"); nl != -1 {
			rest = rest[nl+1:]
		}
		if end := strings.Index(rest, "```"); end != -1 {
			out = append(out, strings.TrimSpace(rest[:end]))
		}
	}

	firstBrace := strings.Index(s, "{")
	lastBrace := strings.LastIndex(s, "}")
	if firstBrace != -1 && lastBrace > firstBrace {
		out = append(out, strings.TrimSpace(s[firstBrace:lastBrace+1]))
	}

	return out
}
