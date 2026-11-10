package orchestration

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"sync"
	"time"
)

// JobProcessor defines the interface for task execution handlers
type JobProcessor interface {
	ProcessJob(ctx context.Context, job *Job, helper ExecutionHelper) (string, error)
}

// ExecutionHelper provides workers with safe logging, progress updates, and cancel checks
type ExecutionHelper interface {
	WorkerID() string
	IsCancelled() bool
	RecordProgress(phase, message string, payload string)
}

type executionHelperImpl struct {
	workerID string
	jobID    string
	storage  *Storage
	ctx      context.Context
}

func (h *executionHelperImpl) WorkerID() string { return h.workerID }
func (h *executionHelperImpl) IsCancelled() bool {
	select {
	case <-h.ctx.Done():
		return true
	default:
		return h.storage.IsJobCancelled(context.Background(), h.jobID)
	}
}
func (h *executionHelperImpl) RecordProgress(phase, message string, payload string) {
	_ = h.storage.RecordEvent(context.Background(), h.jobID, EventStepProgress, phase, h.workerID, message, payload)
}

// DefaultJobProcessor processes task baseline when no AI collaboration processor is attached
type DefaultJobProcessor struct{}

func (p *DefaultJobProcessor) ProcessJob(ctx context.Context, job *Job, h ExecutionHelper) (string, error) {
	h.RecordProgress("working", "Processing orchestration baseline task", "{}")
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-time.After(10 * time.Millisecond):
	}
	return fmt.Sprintf("Orchestration infrastructure processed task: %s", job.Task), nil
}

// WorkerPool manages the background worker goroutines
type WorkerPool struct {
	storage       *Storage
	processor     JobProcessor
	workerCount   int
	leaseDuration time.Duration
	renewInterval time.Duration
	retryBackoff  time.Duration
	stopCh        chan struct{}
	wg            sync.WaitGroup
	running       bool
	mu            sync.Mutex
}

func NewWorkerPool(storage *Storage, processor JobProcessor, count int) *WorkerPool {
	if count <= 0 {
		count = 2
	}
	if processor == nil {
		processor = &DefaultJobProcessor{}
	}
	return &WorkerPool{
		storage:       storage,
		processor:     processor,
		workerCount:   count,
		leaseDuration: 60 * time.Second,
		renewInterval: 15 * time.Second,
		retryBackoff:  2 * time.Second,
		stopCh:        make(chan struct{}),
	}
}

// SetProcessor allows attaching a custom job engine
func (p *WorkerPool) SetProcessor(proc JobProcessor) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.processor = proc
}

// Start launches background workers
func (p *WorkerPool) Start(ctx context.Context) {
	p.mu.Lock()
	if p.running {
		p.mu.Unlock()
		return
	}
	p.running = true
	p.mu.Unlock()

	for i := 1; i <= p.workerCount; i++ {
		workerID := fmt.Sprintf("worker-%d", i)
		p.wg.Add(1)
		go p.workerLoop(ctx, workerID)
	}
	log.Printf("[Orchestration] Background worker pool started with %d workers", p.workerCount)
}

// Stop executes graceful shutdown
func (p *WorkerPool) Stop() {
	p.mu.Lock()
	if !p.running {
		p.mu.Unlock()
		return
	}
	p.running = false
	close(p.stopCh)
	p.mu.Unlock()

	p.wg.Wait()
	log.Println("[Orchestration] Background worker pool shut down cleanly")
}

func (p *WorkerPool) workerLoop(rootCtx context.Context, workerID string) {
	defer p.wg.Done()

	for {
		select {
		case <-rootCtx.Done():
			return
		case <-p.stopCh:
			return
		default:
		}

		job, _, err := p.storage.ClaimNextJob(rootCtx, workerID, p.leaseDuration)
		if err != nil {
			time.Sleep(50 * time.Millisecond)
			continue
		}
		if job == nil {
			select {
			case <-time.After(50 * time.Millisecond):
			case <-rootCtx.Done():
				return
			case <-p.stopCh:
				return
			}
			continue
		}

		p.executeWithProtection(rootCtx, workerID, job)
	}
}

func (p *WorkerPool) executeWithProtection(rootCtx context.Context, workerID string, job *Job) {
	jobCtx, cancel := context.WithCancel(rootCtx)
	defer cancel()

	helper := &executionHelperImpl{
		workerID: workerID,
		jobID:    job.ID,
		storage:  p.storage,
		ctx:      jobCtx,
	}

	// Active lease renewal ticker
	renewTicker := time.NewTicker(p.renewInterval)
	defer renewTicker.Stop()

	go func() {
		for {
			select {
			case <-jobCtx.Done():
				return
			case <-renewTicker.C:
				err := p.storage.RenewLease(context.Background(), job.ID, workerID, p.leaseDuration)
				if errors.Is(err, ErrJobCancelled) {
					cancel()
					return
				}
			}
		}
	}()

	// Panic shield protects the main Go server from crashing on unexpected runtime panics
	defer func() {
		if r := recover(); r != nil {
			errStr := fmt.Sprintf("panic recovered in worker: %v", r)
			log.Printf("[Orchestration] %s (job: %s)", errStr, job.ID)
			_, _ = p.storage.FailOrScheduleRetry(context.Background(), job.ID, workerID, errStr, false, 0)
		}
	}()

	_ = p.storage.RecordEvent(jobCtx, job.ID, EventJobStarted, string(job.CurrentStage), workerID, "Worker execution started", "{}")

	result, err := p.processor.ProcessJob(jobCtx, job, helper)

	if errors.Is(err, context.Canceled) || errors.Is(err, ErrJobCancelled) || helper.IsCancelled() {
		_ = p.storage.CancelJob(context.Background(), job.ID)
		return
	}

	if err != nil {
		isRetryable := IsRetryableProviderError(err)
		backoff := time.Duration(math.Pow(2, float64(job.RetryCount))) * p.retryBackoff
		if backoff <= 0 {
			backoff = 2 * time.Second
		}
		_, _ = p.storage.FailOrScheduleRetry(context.Background(), job.ID, workerID, err.Error(), isRetryable, backoff)
		return
	}

	_ = p.storage.CompleteJob(context.Background(), job.ID, workerID, result)
}
