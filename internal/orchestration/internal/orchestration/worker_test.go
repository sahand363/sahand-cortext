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

	// Wait for processing
	time.Sleep(200 * time.Millisecond)

	j1, _ := storage.GetJob(ctx, job1.ID)
	j2, _ := storage.GetJob(ctx, job2.ID)

	if j1.Status != StatusCompleted {
		t.Errorf("job 1 expected completed, got %s", j1.Status)
	}
	if j2.Status != StatusCompleted {
		t.Errorf("job 2 expected completed, got %s", j2.Status)
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

	mock := &mockProcessor{delay: 400 * time.Millisecond}
	pool := NewWorkerPool(storage, mock, 1)
	pool.renewInterval = 25 * time.Millisecond // fast renewal check

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pool.Start(ctx)
	defer pool.Stop()

	job, _ := storage.CreateJob(ctx, "Cancel Me", 5, 12, "{}")

	time.Sleep(50 * time.Millisecond) // Worker claims
	_ = storage.CancelJob(ctx, job.ID)

	time.Sleep(200 * time.Millisecond) // Wait for renew detection

	updated, _ := storage.GetJob(ctx, job.ID)
	if updated.Status != StatusCancelled {
		t.Errorf("expected status 'cancelled', got '%s'", updated.Status)
	}
}
