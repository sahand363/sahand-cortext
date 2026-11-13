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

type KeyResolverFunc func(providerID string) string
type DefaultCollaboratorFunc func() (providerID, modelID string)
type FallbackCandidatesFunc func() []Participant

type CollaborationConfig struct {
	DefaultMaxRounds int
	DefaultMaxCalls  int
	MaxContextChars  int
	CallTimeout      time.Duration
}

func DefaultCollaborationConfig() CollaborationConfig {
	return CollaborationConfig{
		DefaultMaxRounds: 5,
		DefaultMaxCalls:  15,
		MaxContextChars:  45000,
		CallTimeout:      120 * time.Second,
	}
}

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
		strings.Contains(msg, "unexpected end of json") ||
		strings.Contains(msg, "invalid json response") ||
		strings.Contains(msg, "empty content") ||
		strings.Contains(msg, "empty response") ||
		strings.Contains(msg, "temporary") ||
		strings.Contains(msg, "eof") ||
		strings.Contains(msg, "network error") {
		return true
	}
	return false
}

type CollaborationEngine struct {
	storage            *Storage
	router             *provider.Router
	keyResolver        KeyResolverFunc
	defaultProvider    DefaultCollaboratorFunc
	fallbackCandidates FallbackCandidatesFunc
	config             CollaborationConfig
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
		cfg.DefaultMaxCalls = 15
	}
	if cfg.MaxContextChars <= 0 {
		cfg.MaxContextChars = 45000
	}
	if cfg.CallTimeout <= 0 {
		cfg.CallTimeout = 120 * time.Second
	}
	return &CollaborationEngine{
		storage:         storage,
		router:          router,
		keyResolver:     keyResolver,
		defaultProvider: defaultProvider,
		config:          cfg,
	}
}

func (e *CollaborationEngine) SetFallbackCandidatesProvider(fn FallbackCandidatesFunc) {
	e.fallbackCandidates = fn
}

func (e *CollaborationEngine) checkCancelledOrPaused(ctx context.Context, jobID string, helper ExecutionHelper) (bool, bool) {
	if ctx.Err() != nil || helper.IsCancelled() {
		return true, false
	}
	if e.storage.IsJobCancelled(context.Background(), jobID) {
		return true, false
	}
	if e.storage.IsJobPaused(context.Background(), jobID) {
		return false, true
	}
	return false, false
}

func boolOpt(ptr *bool, defaultVal bool) bool {
	if ptr == nil {
		return defaultVal
	}
	return *ptr
}

func (e *CollaborationEngine) ProcessJob(ctx context.Context, job *Job, helper ExecutionHelper) (string, error) {
	if cancel, _ := e.checkCancelledOrPaused(ctx, job.ID, helper); cancel {
		return "", ErrJobCancelled
	}
	if e.router == nil {
		return fmt.Sprintf("Orchestration infrastructure processed task: %s", job.Task), nil
	}

	participants, metaCfg, err := e.resolveParticipants(ctx, job)
	if err != nil {
		return "", err
	}

	if len(participants) == 0 {
		helper.RecordProgress("working", "Processing orchestration baseline task", "{}")
		return fmt.Sprintf("Orchestration infrastructure processed task: %s", job.Task), nil
	}

	var coordinators []Participant
	var workers []Participant
	var auditors []Participant

	for idx, p := range participants {
		p.SortOrder = idx
		switch strings.ToLower(strings.TrimSpace(p.Role)) {
		case RoleCoordinator:
			coordinators = append(coordinators, p)
		case RoleAuditor:
			auditors = append(auditors, p)
		default:
			p.Role = RoleWorker
			workers = append(workers, p)
		}
	}
	if len(coordinators) == 0 {
		c := participants[0]
		c.Role = RoleCoordinator
		coordinators = append(coordinators, c)
	}
	if len(workers) == 0 {
		w := coordinators[0]
		w.Role = RoleWorker
		if w.Name == "" || w.Name == "Coordinator" {
			w.Name = "Worker"
		}
		workers = append(workers, w)
	}

	allCheck := append(append([]Participant{}, coordinators...), workers...)
	allCheck = append(allCheck, auditors...)
	for _, p := range allCheck {
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

	history, err := e.storage.GetCollaborationMessages(ctx, job.ID)
	if err != nil {
		return "", fmt.Errorf("failed loading collaboration history: %w", err)
	}
	totalCalls := len(history)
	turnIndex := totalCalls

	var currentPlan string
	var latestWorkerResult string
	var completedFinalResult string
	isCurrentlyComplete := false

	for i, m := range history {
		if i == 0 && m.SenderRole == RoleCoordinator {
			currentPlan = m.Content
			if m.Action == ActionComplete {
				finalOut := m.CompletionSummary
				if strings.TrimSpace(finalOut) == "" {
					finalOut = m.Content
				}
				completedFinalResult = finalOut
				isCurrentlyComplete = true
			}
		}
		if m.SenderRole == RoleWorker {
			latestWorkerResult = m.Content
			isCurrentlyComplete = false
		}
		if m.SenderRole == RoleUser {
			isCurrentlyComplete = false
		}
		if i > 0 && m.SenderRole == RoleCoordinator {
			if m.Action == ActionComplete {
				finalResult := latestWorkerResult
				if strings.TrimSpace(m.CompletionSummary) != "" {
					finalResult = fmt.Sprintf("%s\n\n---\n### Coordinator Completion Summary\n%s", latestWorkerResult, strings.TrimSpace(m.CompletionSummary))
				}
				completedFinalResult = finalResult
				isCurrentlyComplete = true
			} else {
				isCurrentlyComplete = false
			}
		}
	}

	baselineDeliverable := strings.TrimSpace(job.ExecutionResult)
	if baselineDeliverable != "" {
		if sumIdx := strings.Index(baselineDeliverable, "\n---\n### Coordinator Completion Summary"); sumIdx != -1 {
			baselineDeliverable = strings.TrimSpace(baselineDeliverable[:sumIdx])
		}
		if latestWorkerResult == "" {
			latestWorkerResult = baselineDeliverable
		}
	}

	if isCurrentlyComplete && completedFinalResult != "" {
		return completedFinalResult, nil
	}

	// Re-read participants at the start of every turn so mid-job roster swaps take effect immediately
	refreshParticipants := func() {
		if freshParts, _, err := e.resolveParticipants(ctx, job); err == nil && len(freshParts) > 0 {
			participants = freshParts
			coordinators = nil
			workers = nil
			auditors = nil
			for idx, p := range participants {
				p.SortOrder = idx
				switch strings.ToLower(strings.TrimSpace(p.Role)) {
				case RoleCoordinator:
					coordinators = append(coordinators, p)
				case RoleAuditor:
					auditors = append(auditors, p)
				default:
					p.Role = RoleWorker
					workers = append(workers, p)
				}
			}
			if len(coordinators) == 0 {
				coordinators = append(coordinators, participants[0])
			}
			if len(workers) == 0 {
				w := coordinators[0]
				w.Role = RoleWorker
				workers = append(workers, w)
			}
		}
	}

	// Step 1: Coordinator creates the implementation plan
	if currentPlan == "" {
		if cancel, pause := e.checkCancelledOrPaused(ctx, job.ID, helper); cancel {
			return "", ErrJobCancelled
		} else if pause {
			return latestWorkerResult, nil
		}
		if totalCalls >= maxCalls {
			return "", errors.New("maximum model calls reached before planning")
		}

		refreshParticipants()
		primaryCoord := coordinators[0]
		_ = e.storage.UpdateJobStage(ctx, job.ID, StagePlanning, 1, totalCalls)
		_ = e.storage.RecordEvent(ctx, job.ID, EventCoordinatorStarted, string(StagePlanning), helper.WorkerID(),
			fmt.Sprintf("Coordinator #%d (%s/%s) is analyzing task and creating the implementation plan…", primaryCoord.SortOrder+1, primaryCoord.Provider, primaryCoord.Model),
			marshalPayload(map[string]any{"round": 1, "role": RoleCoordinator, "provider": primaryCoord.Provider, "model": primaryCoord.Model, "sort_order": primaryCoord.SortOrder}))

		coordCandidates := e.buildCandidateRoster(primaryCoord, coordinators, participants, RoleCoordinator, metaCfg)
		parsed, raw, latency, usedCoord, err := e.invokeWithFailover(ctx, job.ID, 1, string(StagePlanning), coordCandidates, func(cand Participant) []provider.Message {
			return BuildCoordinatorPlanContext(job.Task, cand, maxChars)
		}, callTimeout, metaCfg, helper)
		if err != nil {
			return "", fmt.Errorf("coordinator planning failed: %w", err)
		}

		if cancel, _ := e.checkCancelledOrPaused(ctx, job.ID, helper); cancel {
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
			SenderName:        usedCoord.Name,
			Provider:          usedCoord.Provider,
			Model:             usedCoord.Model,
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

		nextWorkerNum := 2
		if len(workers) > 0 && workers[0].SortOrder >= 0 {
			nextWorkerNum = workers[0].SortOrder + 1
		}
		_ = e.storage.RecordEvent(ctx, job.ID, EventPlanGenerated, string(StagePlanning), helper.WorkerID(),
			fmt.Sprintf("Coordinator #%d (%s/%s) finished the implementation plan in %.1fs — passing job to Worker #%d.", usedCoord.SortOrder+1, usedCoord.Provider, usedCoord.Model, float64(latency)/1000.0, nextWorkerNum),
			marshalPayload(map[string]any{"round": 1, "action": parsed.Action, "latency_ms": latency, "provider": usedCoord.Provider, "model": usedCoord.Model, "sort_order": usedCoord.SortOrder}))

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

	// Step 2: Multi-round collaboration loop
	hasConfiguredAuditors := len(auditors) > 0

	for round := 1; round <= maxRounds; round++ {
		if cancel, pause := e.checkCancelledOrPaused(ctx, job.ID, helper); cancel {
			return "", ErrJobCancelled
		} else if pause {
			return latestWorkerResult, nil
		}

		refreshParticipants()

		hasWorkerForRound := false
		hasAuditorForRound := false
		hasReviewForRound := false
		var latestAuditorReportForRound string
		var latestAuditorActionForRound CollaborationAction

		for idx, m := range history {
			if idx == 0 {
				continue
			}
			if m.Round == round && m.SenderRole == RoleWorker {
				hasWorkerForRound = true
				latestWorkerResult = m.Content
			}
			if m.Round == round && m.SenderRole == RoleAuditor {
				hasAuditorForRound = true
				latestAuditorReportForRound = m.Content
				if m.CompletionSummary != "" {
					latestAuditorReportForRound = fmt.Sprintf("Summary: %s\n\nDetails:\n%s", m.CompletionSummary, m.Content)
				}
				latestAuditorActionForRound = m.Action
			}
			if m.Round == round && m.SenderRole == RoleCoordinator {
				hasReviewForRound = true
			}
		}

		if hasWorkerForRound && (!hasConfiguredAuditors || hasAuditorForRound) && hasReviewForRound {
			continue
		}

		primaryWorker := workers[(round-1)%len(workers)]

		// Phase A: Worker executes or revises
		if !hasWorkerForRound {
			if totalCalls >= maxCalls {
				return e.finalizeAtLimit(ctx, job.ID, round, totalCalls, "maximum model calls limit reached", latestWorkerResult, currentPlan, helper), nil
			}

			_ = e.storage.UpdateJobStage(ctx, job.ID, StageWorking, round, totalCalls)
			_ = e.storage.RecordEvent(ctx, job.ID, EventWorkerStarted, string(StageWorking), helper.WorkerID(),
				fmt.Sprintf("Worker #%d (%s/%s) started round %d execution…", primaryWorker.SortOrder+1, primaryWorker.Provider, primaryWorker.Model, round),
				marshalPayload(map[string]any{"round": round, "role": RoleWorker, "provider": primaryWorker.Provider, "model": primaryWorker.Model, "sort_order": primaryWorker.SortOrder}))

			workerCandidates := e.buildCandidateRoster(primaryWorker, workers, participants, RoleWorker, metaCfg)
			currentCodeSnap := latestWorkerResult
			if currentCodeSnap == "" {
				currentCodeSnap = baselineDeliverable
			}
			workerParsed, workerRaw, workerLat, usedWorker, err := e.invokeWithFailover(ctx, job.ID, round, string(StageWorking), workerCandidates, func(cand Participant) []provider.Message {
				return BuildWorkerExecutionContext(job.Task, currentPlan, currentCodeSnap, history, cand, round, maxChars)
			}, callTimeout, metaCfg, helper)
			if err != nil {
				return "", fmt.Errorf("worker execution failed in round %d: %w", round, err)
			}

			if cancel, pause := e.checkCancelledOrPaused(ctx, job.ID, helper); cancel {
				return "", ErrJobCancelled
			} else if pause {
				return workerParsed.Message, nil
			}

			turnIndex++
			totalCalls++
			latestWorkerResult = workerParsed.Message

			savedWork, err := e.storage.SaveCollaborationMessage(ctx, CollaborationMessage{
				JobID:             job.ID,
				Round:             round,
				TurnIndex:         turnIndex,
				SenderRole:        RoleWorker,
				SenderName:        usedWorker.Name,
				Provider:          usedWorker.Provider,
				Model:             usedWorker.Model,
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

			nextPhaseDesc := "QA Auditor"
			if !hasConfiguredAuditors {
				nextPhaseDesc = "Coordinator Review"
			}
			_ = e.storage.RecordEvent(ctx, job.ID, EventWorkerResponded, string(StageWorking), helper.WorkerID(),
				fmt.Sprintf("Worker #%d (%s/%s) completed round %d deliverable in %.1fs — passing to %s.", usedWorker.SortOrder+1, usedWorker.Provider, usedWorker.Model, float64(workerLat)/1000.0, nextPhaseDesc),
				marshalPayload(map[string]any{"round": round, "latency_ms": workerLat, "action": workerParsed.Action, "provider": usedWorker.Provider, "model": usedWorker.Model, "sort_order": usedWorker.SortOrder}))
		}

		if cancel, pause := e.checkCancelledOrPaused(ctx, job.ID, helper); cancel {
			return "", ErrJobCancelled
		} else if pause {
			return latestWorkerResult, nil
		}

		// Phase B: QA Auditor (if configured)
		if hasConfiguredAuditors && !hasAuditorForRound {
			if totalCalls >= maxCalls {
				return e.finalizeAtLimit(ctx, job.ID, round, totalCalls, "maximum model calls limit reached before QA audit", latestWorkerResult, currentPlan, helper), nil
			}

			refreshParticipants()
			primaryAuditor := auditors[(round-1)%len(auditors)]
			_ = e.storage.UpdateJobStage(ctx, job.ID, StageAuditing, round, totalCalls)
			_ = e.storage.RecordEvent(ctx, job.ID, EventAuditStarted, string(StageAuditing), helper.WorkerID(),
				fmt.Sprintf("QA Auditor (%s/%s) started inspecting round %d deliverable…", primaryAuditor.Provider, primaryAuditor.Model, round),
				marshalPayload(map[string]any{"round": round, "role": RoleAuditor, "provider": primaryAuditor.Provider, "model": primaryAuditor.Model, "sort_order": primaryAuditor.SortOrder}))

			auditorCandidates := e.buildCandidateRoster(primaryAuditor, auditors, participants, RoleAuditor, metaCfg)
			auditParsed, auditRaw, auditLat, usedAuditor, err := e.invokeWithFailover(ctx, job.ID, round, string(StageAuditing), auditorCandidates, func(cand Participant) []provider.Message {
				return BuildPipelineAuditorContext(job.Task, currentPlan, latestWorkerResult, history, cand, round, maxRounds, maxChars)
			}, callTimeout, metaCfg, helper)
			if err != nil {
				return "", fmt.Errorf("QA auditor failed in round %d: %w", round, err)
			}

			if cancel, pause := e.checkCancelledOrPaused(ctx, job.ID, helper); cancel {
				return "", ErrJobCancelled
			} else if pause {
				return latestWorkerResult, nil
			}

			turnIndex++
			totalCalls++
			latestAuditorActionForRound = auditParsed.Action
			latestAuditorReportForRound = auditParsed.Message
			if strings.TrimSpace(auditParsed.CompletionSummary) != "" {
				latestAuditorReportForRound = fmt.Sprintf("Summary: %s\n\nDetails:\n%s", auditParsed.CompletionSummary, auditParsed.Message)
			}

			savedAudit, err := e.storage.SaveCollaborationMessage(ctx, CollaborationMessage{
				JobID:             job.ID,
				Round:             round,
				TurnIndex:         turnIndex,
				SenderRole:        RoleAuditor,
				SenderName:        usedAuditor.Name,
				Provider:          usedAuditor.Provider,
				Model:             usedAuditor.Model,
				Action:            auditParsed.Action,
				Content:           auditParsed.Message,
				CompletionSummary: auditParsed.CompletionSummary,
				RawResponse:       auditRaw,
				LatencyMs:         auditLat,
			})
			if err != nil {
				return "", err
			}
			history = append(history, *savedAudit)

			verdict := "APPROVED"
			if auditParsed.Action == ActionRevise {
				verdict = "ISSUES FLAGGED"
			}
			_ = e.storage.RecordEvent(ctx, job.ID, EventAuditCompleted, string(StageAuditing), helper.WorkerID(),
				fmt.Sprintf("QA Auditor (%s/%s) completed round %d inspection in %.1fs (Verdict: %s) — passing to Coordinator Review.", usedAuditor.Provider, usedAuditor.Model, round, float64(auditLat)/1000.0, verdict),
				marshalPayload(map[string]any{
					"round":              round,
					"action":             auditParsed.Action,
					"completion_summary": auditParsed.CompletionSummary,
					"latency_ms":         auditLat,
					"provider":           usedAuditor.Provider,
					"model":              usedAuditor.Model,
					"sort_order":         usedAuditor.SortOrder,
				}))
		}

		if cancel, pause := e.checkCancelledOrPaused(ctx, job.ID, helper); cancel {
			return "", ErrJobCancelled
		} else if pause {
			return latestWorkerResult, nil
		}

		if totalCalls >= maxCalls {
			return e.finalizeAtLimit(ctx, job.ID, round, totalCalls, "maximum model calls limit reached before coordinator review", latestWorkerResult, currentPlan, helper), nil
		}

		// Phase C: Coordinator reviews round output
		refreshParticipants()
		primaryCoord := coordinators[0]
		_ = e.storage.UpdateJobStage(ctx, job.ID, StageReviewing, round, totalCalls)
		_ = e.storage.RecordEvent(ctx, job.ID, EventReviewStarted, string(StageReviewing), helper.WorkerID(),
			fmt.Sprintf("Coordinator (%s/%s) started reviewing round %d output…", primaryCoord.Provider, primaryCoord.Model, round),
			marshalPayload(map[string]any{"round": round, "role": RoleCoordinator, "provider": primaryCoord.Provider, "model": primaryCoord.Model, "sort_order": primaryCoord.SortOrder}))

		coordCandidates := e.buildCandidateRoster(primaryCoord, coordinators, participants, RoleCoordinator, metaCfg)
		reviewParsed, reviewRaw, reviewLat, usedCoord, err := e.invokeWithFailover(ctx, job.ID, round, string(StageReviewing), coordCandidates, func(cand Participant) []provider.Message {
			return BuildCoordinatorReviewContext(job.Task, currentPlan, latestWorkerResult, latestAuditorReportForRound, latestAuditorActionForRound, history, cand, round, maxRounds, maxChars)
		}, callTimeout, metaCfg, helper)
		if err != nil {
			return "", fmt.Errorf("coordinator review failed in round %d: %w", round, err)
		}

		if cancel, pause := e.checkCancelledOrPaused(ctx, job.ID, helper); cancel {
			return "", ErrJobCancelled
		} else if pause {
			return latestWorkerResult, nil
		}

		turnIndex++
		totalCalls++

		savedReview, err := e.storage.SaveCollaborationMessage(ctx, CollaborationMessage{
			JobID:             job.ID,
			Round:             round,
			TurnIndex:         turnIndex,
			SenderRole:        RoleCoordinator,
			SenderName:        usedCoord.Name,
			Provider:          usedCoord.Provider,
			Model:             usedCoord.Model,
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
			"provider":           usedCoord.Provider,
			"model":              usedCoord.Model,
			"sort_order":         usedCoord.SortOrder,
		})
		_ = e.storage.RecordEvent(ctx, job.ID, EventReviewDecision, string(StageReviewing), helper.WorkerID(),
			fmt.Sprintf("Coordinator (%s/%s) decision in round %d: %s (%.1fs)", usedCoord.Provider, usedCoord.Model, round, reviewParsed.Action, float64(reviewLat)/1000.0), decisionPayload)

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

		_ = e.storage.RecordEvent(ctx, job.ID, EventRevisionRequested, string(StageReviewing), helper.WorkerID(),
			fmt.Sprintf("Coordinator requested revision in round %d — passing back to Worker.", round), decisionPayload)
	}

	return e.finalizeAtLimit(ctx, job.ID, maxRounds, totalCalls, "maximum collaboration rounds reached", latestWorkerResult, currentPlan, helper), nil
}

func (e *CollaborationEngine) buildCandidateRoster(
	primary Participant,
	sameRolePool []Participant,
	allAssignedPool []Participant,
	targetRole string,
	metaCfg JobMetadata,
) []Participant {
	seen := map[string]bool{}
	var roster []Participant

	addCandidate := func(p Participant) {
		key := strings.ToLower(p.Provider) + "::" + strings.TrimSpace(p.Model)
		if p.Model == "" || seen[key] {
			return
		}
		if strings.TrimSpace(e.keyResolver(p.Provider)) == "" {
			return
		}
		seen[key] = true
		p.Role = targetRole
		if p.Name == "" {
			switch targetRole {
			case RoleCoordinator:
				p.Name = "Coordinator / Architect"
			case RoleAuditor:
				p.Name = "Quality Assurance / Auditor"
			default:
				p.Name = "Worker / Developer"
			}
		}
		roster = append(roster, p)
	}

	addCandidate(primary)

	if boolOpt(metaCfg.SwitchParticipant, true) {
		for _, p := range sameRolePool {
			addCandidate(p)
		}
		for _, p := range allAssignedPool {
			addCandidate(p)
		}
	}

	if boolOpt(metaCfg.AllowGlobalFallback, true) && e.fallbackCandidates != nil {
		for _, fb := range e.fallbackCandidates() {
			fb.SortOrder = -1
			addCandidate(fb)
		}
	}

	return roster
}

func (e *CollaborationEngine) invokeWithFailover(
	ctx context.Context,
	jobID string,
	round int,
	phase string,
	candidates []Participant,
	promptBuilder func(Participant) []provider.Message,
	timeout time.Duration,
	metaCfg JobMetadata,
	helper ExecutionHelper,
) (StructuredModelResponse, string, int64, Participant, error) {
	if len(candidates) == 0 {
		return StructuredModelResponse{}, "", 0, Participant{}, errors.New("no valid model candidates available")
	}

	autoRetry := boolOpt(metaCfg.AutoRetry, true)
	var errs []string
	var prevErr string

	for idx, cand := range candidates {
		if cancel, _ := e.checkCancelledOrPaused(ctx, jobID, helper); cancel {
			return StructuredModelResponse{}, "", 0, cand, ErrJobCancelled
		}

		if idx > 0 {
			prev := candidates[idx-1]
			_ = e.storage.RecordEvent(ctx, jobID, EventParticipantFallback, phase, helper.WorkerID(),
				fmt.Sprintf("✗ Model %s/%s failed (%s) — automatically passing job to backup %s/%s.", prev.Provider, prev.Model, prevErr, cand.Provider, cand.Model),
				marshalPayload(map[string]any{
					"round":         round,
					"from_provider": prev.Provider,
					"from_model":    prev.Model,
					"from_order":    prev.SortOrder,
					"error":         prevErr,
					"to_provider":   cand.Provider,
					"to_model":      cand.Model,
					"to_order":      cand.SortOrder,
				}))
		}

		msgs := promptBuilder(cand)
		parsed, raw, lat, err := e.invokeParticipant(ctx, jobID, round, phase, cand, msgs, timeout, !autoRetry, helper)
		if err == nil {
			return parsed, raw, lat, cand, nil
		}

		prevErr = err.Error()
		_ = e.storage.RecordEvent(ctx, jobID, "participant_failed", phase, helper.WorkerID(),
			fmt.Sprintf("✗ Model %s/%s failed (%s)", cand.Provider, cand.Model, prevErr),
			marshalPayload(map[string]any{
				"round":      round,
				"role":       cand.Role,
				"provider":   cand.Provider,
				"model":      cand.Model,
				"sort_order": cand.SortOrder,
				"error":      prevErr,
			}))

		errs = append(errs, fmt.Sprintf("%s/%s: %v", cand.Provider, cand.Model, err))
	}

	return StructuredModelResponse{}, "", 0, candidates[0], fmt.Errorf("all candidate models failed (%s)", strings.Join(errs, " | "))
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
	disableRetry bool,
	helper ExecutionHelper,
) (StructuredModelResponse, string, int64, error) {
	if cancel, _ := e.checkCancelledOrPaused(ctx, jobID, helper); cancel {
		return StructuredModelResponse{}, "", 0, ErrJobCancelled
	}

	key := e.keyResolver(participant.Provider)
	if strings.TrimSpace(key) == "" {
		return StructuredModelResponse{}, "", 0, fmt.Errorf("missing API key for provider %s", participant.Provider)
	}

	invokePayload := marshalPayload(map[string]any{
		"round":      round,
		"role":       participant.Role,
		"name":       participant.Name,
		"provider":   participant.Provider,
		"model":      participant.Model,
		"sort_order": participant.SortOrder,
	})
	_ = e.storage.RecordEvent(ctx, jobID, EventModelInvoked, phase, helper.WorkerID(),
		fmt.Sprintf("Invoking %s #%d (%s/%s)…", participant.Role, participant.SortOrder+1, participant.Provider, participant.Model), invokePayload)

	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	rawReply, latency, err := e.router.Call(callCtx, participant.Provider, key, participant.Model, messages, provider.CallOpts{
		DisableRetry: disableRetry,
	})
	if err != nil {
		return StructuredModelResponse{}, "", latency, err
	}

	defaultAction := ActionContinue
	if phase == string(StageReviewing) {
		defaultAction = ActionRevise
	} else if phase == string(StageAuditing) {
		defaultAction = ActionComplete
	}
	parsed := ParseStructuredResponse(rawReply, defaultAction)

	respPayload := marshalPayload(map[string]any{
		"round":      round,
		"role":       participant.Role,
		"provider":   participant.Provider,
		"model":      participant.Model,
		"sort_order": participant.SortOrder,
		"action":     parsed.Action,
		"latency_ms": latency,
	})
	_ = e.storage.RecordEvent(ctx, jobID, EventModelResponded, phase, helper.WorkerID(),
		fmt.Sprintf("Received response from %s (%s/%s in %.1fs, action=%s)", participant.Role, participant.Provider, participant.Model, float64(latency)/1000.0, parsed.Action), respPayload)

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

func parseRevisionFlags(raw string) (cleanInstruction string, includeFullFiles bool, includeSummary bool) {
	includeFullFiles = true
	includeSummary = true
	s := strings.TrimSpace(raw)

	if strings.HasPrefix(s, "[REV_OPTS:") {
		if endIdx := strings.Index(s, "]"); endIdx != -1 {
			header := strings.ToLower(s[:endIdx])
			includeFullFiles = !strings.Contains(header, "full_files=false")
			includeSummary = !strings.Contains(header, "summary=false")
			s = strings.TrimSpace(s[endIdx+1:])
		}
	}
	return s, includeFullFiles, includeSummary
}

func latestUserRevisionFromHistory(history []CollaborationMessage) (string, bool, bool) {
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].SenderRole == RoleUser {
			return parseRevisionFlags(history[i].Content)
		}
	}
	return "", true, true
}

func buildCondensedDiscussionSummary(history []CollaborationMessage, maxChars int) string {
	if len(history) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("### Condensed Discussion & Decisions Summary:\n")
	for _, m := range history {
		switch m.SenderRole {
		case RoleCoordinator:
			sum := strings.TrimSpace(m.CompletionSummary)
			if sum == "" {
				sum = truncateText(m.Content, 700)
			}
			sb.WriteString(fmt.Sprintf("- [Round %d · Coordinator (%s)]: %s\n", m.Round, m.Model, sum))
		case RoleAuditor:
			sum := strings.TrimSpace(m.CompletionSummary)
			if sum == "" {
				sum = truncateText(m.Content, 500)
			}
			sb.WriteString(fmt.Sprintf("- [Round %d · QA Auditor (%s)]: %s\n", m.Round, m.Model, sum))
		case RoleUser:
			clean, _, _ := parseRevisionFlags(m.Content)
			sb.WriteString(fmt.Sprintf("- [Round %d · User Revision Request]: %s\n", m.Round, truncateText(clean, 800)))
		case RoleWorker:
			sum := strings.TrimSpace(m.CompletionSummary)
			if sum != "" {
				sb.WriteString(fmt.Sprintf("- [Round %d · Worker (%s) Summary]: %s\n", m.Round, m.Model, sum))
			}
		}
	}
	return truncateText(sb.String(), maxChars)
}

func BuildCoordinatorPlanContext(task string, coordinator Participant, maxChars int) []provider.Message {
	baseSys := strings.TrimSpace(coordinator.SystemPrompt)
	if baseSys == "" {
		baseSys = "You are the Coordinator / Architect in a multi-model AI collaboration managed by Sahand Cortex."
	}

	sysPrompt := baseSys + `

Your responsibilities:
1. Analyze the original user task.
2. Formulate a clear, structured implementation plan and explicit technical instructions for the Worker / Developer.
3. If the task requires a software project or deployment, instruct the Worker to output every required file using explicit file headers:
   === FILE: filename.ext ===
   (complete file content)
4. Respond using ONLY valid JSON matching this schema:
{
  "action": "continue",
  "message": "Your detailed implementation plan, file manifest, and instructions for the Worker",
  "completion_summary": ""
}`

	userPrompt := fmt.Sprintf("### Original User Task:\n%s\n\nCreate the implementation plan and instructions for the Worker in the required JSON format.", truncateText(task, maxChars/2))

	return []provider.Message{
		{Role: "system", Content: sysPrompt},
		{Role: "user", Content: userPrompt},
	}
}

func BuildWorkerExecutionContext(task, plan, latestCodebase string, history []CollaborationMessage, worker Participant, round int, maxChars int) []provider.Message {
	if maxChars <= 0 {
		maxChars = 45000
	}
	latestUserRev, incFullFiles, incSummary := latestUserRevisionFromHistory(history)
	if latestUserRev != "" && incFullFiles {
		if maxChars < 120000 {
			maxChars = 120000
		}
	}

	baseSys := strings.TrimSpace(worker.SystemPrompt)
	if baseSys == "" {
		baseSys = "You are the Worker / Developer in a multi-model AI collaboration managed by Sahand Cortex."
	}

	sysPrompt := baseSys + `

Your responsibilities:
1. Follow the Coordinator's implementation plan, QA Auditor findings, review instructions, and any direct User Revision Requests carefully.
2. Produce complete, working, self-contained deliverables with zero placeholders or "// TODO" comments. Never omit unchanged files—always output the complete set of project files so the user's ZIP download has everything needed.
3. CRITICAL REVISION RULE: When existing project files are provided under "CURRENT PROJECT FILES TO MODIFY", you MUST NOT build a different program from scratch. Keep the existing architecture, styling, and working features intact, and apply the requested modifications or new features directly to those files.
4. MULTI-FILE PACKAGING RULE (CRITICAL): Do NOT wrap your code in a JSON object where quotes can break. Instead, output plain text starting with a 1-line summary, followed by EACH project file under its own exact header:
SUMMARY: Brief summary of the files and features you produced
=== FILE: main.go ===
(complete Go code here)
=== FILE: index.html ===
(complete HTML code here)
=== FILE: config.json ===
(complete JSON config here)
=== FILE: install.sh ===
(complete Bash installer script here)`

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("### Original User Task:\n%s\n\n", truncateText(task, 12000)))
	sb.WriteString(fmt.Sprintf("### Coordinator Implementation Plan:\n%s\n\n", truncateText(plan, 10000)))

	if latestUserRev != "" {
		if incSummary {
			sb.WriteString(buildCondensedDiscussionSummary(history, 10000) + "\n\n")
		}
		if incFullFiles && strings.TrimSpace(latestCodebase) != "" {
			sb.WriteString(fmt.Sprintf("### CURRENT PROJECT FILES TO MODIFY (DO NOT REWRITE FROM SCRATCH — EDIT THESE FILES):\n%s\n\n", truncateText(latestCodebase, 85000)))
		}
		sb.WriteString(fmt.Sprintf("### IMPORTANT — LATEST USER REVISION REQUEST:\n%s\n\nModify the existing project above to fulfill this revision request while preserving all existing working functionality. Output all project files in full using === FILE: filename === headers.", latestUserRev))
	} else {
		if round > 1 && strings.TrimSpace(latestCodebase) != "" {
			sb.WriteString(fmt.Sprintf("### Current Draft Files (Round %d):\n%s\n\n", round-1, truncateText(latestCodebase, 65000)))
		}
		if len(history) > 1 {
			sb.WriteString(formatBoundedHistory(history, 16000))
		}
		if round > 1 {
			sb.WriteString("\nAddress all revision requests from the QA Auditor and Coordinator's latest review, and provide the complete updated deliverable using === FILE: filename === headers for every file.")
		} else {
			sb.WriteString("\nExecute the Coordinator's plan now and provide every required project file using === FILE: filename === headers.")
		}
	}

	return []provider.Message{
		{Role: "system", Content: sysPrompt},
		{Role: "user", Content: sb.String()},
	}
}

func BuildPipelineAuditorContext(task, plan, latestWorkerResult string, history []CollaborationMessage, auditor Participant, round, maxRounds, maxChars int) []provider.Message {
	if maxChars <= 0 {
		maxChars = 60000
	}
	baseSys := strings.TrimSpace(auditor.SystemPrompt)
	if baseSys == "" {
		baseSys = "You are the Quality Assurance (QA) & Security Auditor in a 3-tier AI collaboration pipeline managed by Sahand Cortex."
	}

	sysPrompt := baseSys + `

Your responsibilities:
1. Independently test and audit the Worker's deliverable before the Coordinator gives final approval.
2. Check for runtime bugs, missing files, broken logic, or unfulfilled user revision requests.
3. Set "action": "complete" ONLY if the Worker's deliverable is 100% working, complete, and ready for delivery.
4. Set "action": "revise" if you find ANY bugs, missing files, or broken logic, and provide a numbered list of exact fixes required.
5. Respond using ONLY valid JSON matching this schema:
{
  "action": "complete | revise",
  "message": "Your detailed QA inspection report and numbered fixes (if any)",
  "completion_summary": "1-sentence QA verdict summary"
}`

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("### Original User Task:\n%s\n\n", truncateText(task, 10000)))
	sb.WriteString(fmt.Sprintf("### Coordinator Plan:\n%s\n\n", truncateText(plan, 8000)))
	latestUserRev, _, _ := latestUserRevisionFromHistory(history)
	if latestUserRev != "" {
		sb.WriteString(fmt.Sprintf("### Latest User Revision Request:\n%s\n\n", latestUserRev))
	}
	sb.WriteString(fmt.Sprintf("### Worker Deliverable to Audit (Round %d of %d):\n%s\n\n", round, maxRounds, truncateText(latestWorkerResult, 70000)))
	sb.WriteString("Perform your QA audit now and respond in the required JSON format.")

	return []provider.Message{
		{Role: "system", Content: sysPrompt},
		{Role: "user", Content: sb.String()},
	}
}

func BuildCoordinatorReviewContext(
	task, plan, latestWorkerResult, auditorReport string,
	auditorAction CollaborationAction,
	history []CollaborationMessage,
	coordinator Participant,
	round, maxRounds, maxChars int,
) []provider.Message {
	if maxChars <= 0 {
		maxChars = 60000
	}
	baseSys := strings.TrimSpace(coordinator.SystemPrompt)
	if baseSys == "" {
		baseSys = "You are the Coordinator / Architect reviewing the Worker's deliverable in Sahand Cortex."
	}

	sysPrompt := baseSys + `

Your responsibilities:
1. Strictly evaluate the Worker's deliverable (and the QA Auditor's inspection report, if present) against the original user task, your implementation plan, and any User Revision Requests.
2. If the QA Auditor reported bugs ("action": "revise") or if you find ANY bug, unfulfilled user revision request, missing file, or broken logic, you MUST set "action": "revise" and instruct the Worker on the exact fixes required.
3. Only set "action": "complete" when all required files, user revisions, and features are verified working and complete.
4. Respond using ONLY valid JSON matching this schema:
{
  "action": "complete | revise | continue",
  "message": "Your detailed technical review, bug report, or approval",
  "completion_summary": "Brief final summary if action is complete, otherwise empty"
}`

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("### Original User Task:\n%s\n\n", truncateText(task, 10000)))
	sb.WriteString(fmt.Sprintf("### Implementation Plan:\n%s\n\n", truncateText(plan, 8000)))
	latestUserRev, _, incSummary := latestUserRevisionFromHistory(history)
	if latestUserRev != "" {
		if incSummary {
			sb.WriteString(buildCondensedDiscussionSummary(history, 8000) + "\n\n")
		}
		sb.WriteString(fmt.Sprintf("### Latest User Revision Request:\n%s\n\n", latestUserRev))
	}
	sb.WriteString(fmt.Sprintf("### Latest Worker Result (Round %d of %d):\n%s\n\n", round, maxRounds, truncateText(latestWorkerResult, 70000)))
	if strings.TrimSpace(auditorReport) != "" {
		sb.WriteString(fmt.Sprintf("### Quality Assurance / Auditor Report (Verdict: %s):\n%s\n\n", strings.ToUpper(string(auditorAction)), truncateText(auditorReport, 10000)))
	}
	sb.WriteString("Review the Worker's deliverable and the QA Auditor report carefully, and respond with your decision in the required JSON format.")

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
		if m.SenderRole == RoleWorker && i < len(history)-2 {
			continue
		}
		content := m.Content
		if m.SenderRole == RoleUser {
			content, _, _ = parseRevisionFlags(content)
		}
		entry := fmt.Sprintf("[Round %d · %s (%s/%s) · Action: %s]\n%s\n",
			m.Round, strings.ToUpper(m.SenderRole), m.Provider, m.Model, m.Action, truncateText(content, budget/2))
		if used+len(entry) > budget && len(chunks) > 0 {
			break
		}
		chunks = append([]string{entry}, chunks...)
		used += len(entry)
	}
	return "### Recent Collaboration Reviews & Notes:\n" + strings.Join(chunks, "\n")
}

func truncateText(s string, maxChars int) string {
	s = strings.TrimSpace(s)
	if maxChars <= 0 || len(s) <= maxChars {
		return s
	}
	return s[:maxChars] + "\n...[truncated]..."
}

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

	if !strings.HasPrefix(trimmed, "{") && strings.Contains(trimmed, "=== FILE:") {
		var summary string
		msg := trimmed
		lines := strings.SplitN(trimmed, "\n", 3)
		if len(lines) > 0 && strings.HasPrefix(strings.ToUpper(strings.TrimSpace(lines[0])), "SUMMARY:") {
			summary = strings.TrimSpace(lines[0][len("SUMMARY:"):])
			if idx := strings.Index(trimmed, "=== FILE:"); idx != -1 {
				msg = strings.TrimSpace(trimmed[idx:])
			}
		}
		return StructuredModelResponse{
			Action:            defaultAction,
			Message:           msg,
			CompletionSummary: summary,
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
		if err := json.Unmarshal([]byte(c), &parsed); err == nil && (parsed.Action != "" || parsed.Decision != "" || parsed.Message != "" || parsed.Content != "") {
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

	if strings.HasPrefix(trimmed, "{") && strings.Contains(trimmed, `"message"`) {
		msgIdx := strings.Index(trimmed, `"message"`)
		if msgIdx != -1 {
			rest := trimmed[msgIdx+len(`"message"`):]
			if colonIdx := strings.Index(rest, `"`); colonIdx != -1 {
				body := rest[colonIdx+1:]
				if endIdx := strings.LastIndex(body, `"completion_summary"`); endIdx != -1 {
					body = strings.TrimRight(strings.TrimSpace(body[:endIdx]), `",`)
				} else if lastQuote := strings.LastIndex(body, `"`); lastQuote != -1 {
					body = body[:lastQuote]
				}
				body = strings.ReplaceAll(body, `\n`, "\n")
				body = strings.ReplaceAll(body, `\r`, "\r")
				body = strings.ReplaceAll(body, `\t`, "\t")
				body = strings.ReplaceAll(body, `\"`, `"`)
				body = strings.ReplaceAll(body, `\\`, `\`)
				trimmed = strings.TrimSpace(body)
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

func (e *CollaborationEngine) AuditDeliverable(ctx context.Context, jobID, providerID, modelID string) (*CollaborationMessage, error) {
	if e.router == nil {
		return nil, errors.New("provider router is not initialized")
	}
	normProv := provider.NormalizeProvider(providerID)
	modelID = strings.TrimSpace(modelID)
	if modelID == "" {
		return nil, errors.New("auditor model ID is required")
	}
	key := strings.TrimSpace(e.keyResolver(normProv))
	if key == "" {
		return nil, fmt.Errorf("missing API key for auditor provider %s", provider.ProviderLabel(normProv))
	}

	job, err := e.storage.GetJob(ctx, jobID)
	if err != nil {
		return nil, err
	}
	history, err := e.storage.GetCollaborationMessages(ctx, jobID)
	if err != nil {
		return nil, err
	}

	var plan string
	var deliverable string
	for i, m := range history {
		if i == 0 && m.SenderRole == RoleCoordinator {
			plan = m.Content
		}
		if m.SenderRole == RoleWorker {
			deliverable = m.Content
		}
	}
	if strings.TrimSpace(job.ExecutionResult) != "" {
		deliverable = job.ExecutionResult
		if idx := strings.Index(deliverable, "\n---\n### Coordinator Completion Summary"); idx != -1 {
			deliverable = strings.TrimSpace(deliverable[:idx])
		}
	}
	if strings.TrimSpace(deliverable) == "" {
		return nil, errors.New("no deliverable found to audit yet")
	}

	_ = e.storage.RecordEvent(ctx, jobID, EventAuditStarted, "auditing", "auditor",
		fmt.Sprintf("Independent Second-Opinion Audit started with %s / %s", normProv, modelID),
		marshalPayload(map[string]any{"provider": normProv, "model": modelID}))

	promptMsgs := BuildIndependentAuditContext(job.Task, plan, deliverable, 90000)

	callCtx, cancel := context.WithTimeout(ctx, e.config.CallTimeout)
	defer cancel()

	rawReply, latency, err := e.router.Call(callCtx, normProv, key, modelID, promptMsgs, provider.CallOpts{})
	if err != nil {
		_ = e.storage.RecordEvent(ctx, jobID, EventStepProgress, "auditing", "auditor",
			fmt.Sprintf("Independent Audit failed on %s/%s: %v", normProv, modelID, err), "{}")
		return nil, err
	}

	parsed := ParseStructuredResponse(rawReply, ActionComplete)
	nextTurn := len(history) + 1
	round := job.CurrentRound
	if round <= 0 {
		round = 1
	}

	savedMsg, err := e.storage.SaveCollaborationMessage(ctx, CollaborationMessage{
		JobID:             jobID,
		Round:             round,
		TurnIndex:         nextTurn,
		SenderRole:        RoleAuditor,
		SenderName:        "Independent AI Auditor",
		Provider:          normProv,
		Model:             modelID,
		Action:            parsed.Action,
		Content:           parsed.Message,
		CompletionSummary: parsed.CompletionSummary,
		RawResponse:       rawReply,
		LatencyMs:         latency,
	})
	if err != nil {
		return nil, err
	}

	verdictLabel := "APPROVED (No critical bugs found)"
	if parsed.Action == ActionRevise {
		verdictLabel = "ISSUES FOUND (Fixes provided)"
	}

	_ = e.storage.RecordEvent(ctx, jobID, EventAuditCompleted, "auditing", "auditor",
		fmt.Sprintf("Independent Auditor (%s/%s) finished in %d ms — Verdict: %s", normProv, modelID, latency, verdictLabel),
		marshalPayload(map[string]any{
			"provider":           normProv,
			"model":              modelID,
			"action":             parsed.Action,
			"latency_ms":         latency,
			"completion_summary": parsed.CompletionSummary,
		}))

	return savedMsg, nil
}

func BuildIndependentAuditContext(task, plan, deliverable string, maxChars int) []provider.Message {
	if maxChars <= 0 {
		maxChars = 90000
	}
	sysPrompt := `You are an independent Senior Code Reviewer and Security/Quality Auditor in Sahand Cortex.
Your job is to perform a rigorous second-opinion audit of the project files produced by the primary AI team.

How to Respond:
- If you find ANY bugs or improvements needed, output plain text starting with:
DECISION: REVISE
SUMMARY: Concise bulleted list of exact fixes
followed by the COMPLETE corrected project files using:
=== FILE: path/to/filename.ext ===
- If the project is solid, output:
DECISION: COMPLETE
SUMMARY: Brief confirmation.`

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("### Original User Task:\n%s\n\n", truncateText(task, 12000)))
	if strings.TrimSpace(plan) != "" {
		sb.WriteString(fmt.Sprintf("### Architectural Plan:\n%s\n\n", truncateText(plan, 10000)))
	}
	sb.WriteString(fmt.Sprintf("### Project Deliverable Files to Audit:\n%s\n\n", truncateText(deliverable, 70000)))
	sb.WriteString("Perform your independent audit now and return your report.")

	return []provider.Message{
		{Role: "system", Content: sysPrompt},
		{Role: "user", Content: sb.String()},
	}
}
