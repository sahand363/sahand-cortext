package orchestration

import (
	"context"
	"database/sql"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

type mockProcessor struct {
	processCount int64
	delay        time.Duration
	fail         bool
}

func (m *mockProcessor) ProcessJob(ctx context.Context, job *Job, h ExecutionHelper) (string, error) {
	atomic.AddInt64(&m.processCount, 1)
	if m.delay > 0 {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(m.delay):
		}
	}
	if m.fail {
		return "", errors.New("transient upstream failure")
	}
	return "mock complete", nil
}

func TestWorkerPoolExecution(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("sqlite open: %v", err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()

	storage := NewStorage(db)
	_ = storage.InitSchema()

	mock := &mockProcessor{delay: 20 * time.Millisecond}
	pool := NewWorkerPool(storage, mock, 2)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pool.Start(ctx)
	defer pool.Stop()

	job1, _ := storage.CreateJob(ctx, "Job 1", 5, 12, "{}")
	job2, _ := storage.CreateJob(ctx, "Job 2", 5, 12, "{}")

	// Wait for processing with active polling loop
	deadline := time.Now().Add(3 * time.Second)
	var j1, j2 *Job
	for time.Now().Before(deadline) {
		j1, _ = storage.GetJob(ctx, job1.ID)
		j2, _ = storage.GetJob(ctx, job2.ID)
		if j1 != nil && j1.Status == StatusCompleted && j2 != nil && j2.Status == StatusCompleted {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}

	if j1 == nil || j1.Status != StatusCompleted {
		t.Errorf("job 1 expected completed, got %v", j1)
	}
	if j2 == nil || j2.Status != StatusCompleted {
		t.Errorf("job 2 expected completed, got %v", j2)
	}
	if atomic.LoadInt64(&mock.processCount) != 2 {
		t.Errorf("expected 2 processed jobs, got %d", atomic.LoadInt64(&mock.processCount))
	}
}

func TestWorkerCancellationDuringExecution(t *testing.T) {
	db, _ := sql.Open("sqlite", ":memory:")
	db.SetMaxOpenConns(1)
	defer db.Close()

	storage := NewStorage(db)
	_ = storage.InitSchema()

	mock := &mockProcessor{delay: 300 * time.Millisecond}
	pool := NewWorkerPool(storage, mock, 1)
	pool.renewInterval = 20 * time.Millisecond // fast renewal check

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pool.Start(ctx)
	defer pool.Stop()

	job, _ := storage.CreateJob(ctx, "Cancel Me", 5, 12, "{}")

	// Wait until worker claims the job and is running
	deadlineClaim := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadlineClaim) {
		j, _ := storage.GetJob(ctx, job.ID)
		if j != nil && j.Status == StatusRunning {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}

	_ = storage.CancelJob(ctx, job.ID)

	// Wait until worker detects cancellation
	deadlineCancel := time.Now().Add(2 * time.Second)
	var updated *Job
	for time.Now().Before(deadlineCancel) {
		updated, _ = storage.GetJob(ctx, job.ID)
		if updated != nil && updated.Status == StatusCancelled {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if updated == nil || updated.Status != StatusCancelled {
		t.Errorf("expected status 'cancelled', got '%v'", updated)
	}
}
