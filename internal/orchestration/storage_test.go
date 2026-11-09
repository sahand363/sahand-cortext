package orchestration

import (
	"context"
	"database/sql"
	"sync"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func setupTestStorage(t *testing.T) (*Storage, func()) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("failed to open in-memory sqlite: %v", err)
	}
	db.SetMaxOpenConns(1)

	s := NewStorage(db)
	if err := s.InitSchema(); err != nil {
		t.Fatalf("schema init failed: %v", err)
	}

	return s, func() { db.Close() }
}

func TestStorageCreateAndGetJob(t *testing.T) {
	s, cleanup := setupTestStorage(t)
	defer cleanup()

	ctx := context.Background()
	job, err := s.CreateJob(ctx, "Analyze data logs", 4, 10, `{"type":"analytics"}`)
	if err != nil {
		t.Fatalf("failed creating job: %v", err)
	}

	if job.Status != StatusQueued {
		t.Errorf("expected status 'queued', got '%s'", job.Status)
	}

	retrieved, err := s.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("failed getting job: %v", err)
	}

	if retrieved.Task != "Analyze data logs" {
		t.Errorf("task mismatch: %s", retrieved.Task)
	}
	if retrieved.MaxRounds != 4 || retrieved.MaxCalls != 10 {
		t.Errorf("limits mismatch: rounds=%d, calls=%d", retrieved.MaxRounds, retrieved.MaxCalls)
	}
}

func TestAtomicClaimsSingleWinner(t *testing.T) {
	s, cleanup := setupTestStorage(t)
	defer cleanup()

	ctx := context.Background()
	job, err := s.CreateJob(ctx, "Concurrent Claim Task", 5, 12, "{}")
	if err != nil {
		t.Fatalf("create job err: %v", err)
	}

	const workerCount = 10
	var wg sync.WaitGroup
	claims := make([]string, 0)
	var mu sync.Mutex

	for i := 0; i < workerCount; i++ {
		wg.Add(1)
		wID := fmt.Sprintf("worker-%d", i)
		go func(id string) {
			defer wg.Done()
			claimed, _, err := s.ClaimNextJob(ctx, id, 30*time.Second)
			if err != nil {
				t.Errorf("claim error: %v", err)
				return
			}
			if claimed != nil {
				mu.Lock()
				claims = append(claims, id)
				mu.Unlock()
			}
		}(wID)
	}
	wg.Wait()

	if len(claims) != 1 {
		t.Fatalf("expected exactly 1 claim on job %s, got %d (%v)", job.ID, len(claims), claims)
	}
}

func TestLeaseExpirationAndRecovery(t *testing.T) {
	s, cleanup := setupTestStorage(t)
	defer cleanup()

	ctx := context.Background()
	job, _ := s.CreateJob(ctx, "Expiring Task", 5, 12, "{}")

	// Worker 1 claims with expired 1 millisecond lease
	claimed, _, err := s.ClaimNextJob(ctx, "worker-crashed", 1*time.Millisecond)
	if err != nil || claimed == nil {
		t.Fatalf("claim failed: %v", err)
	}

	time.Sleep(10 * time.Millisecond) // Ensure lease expired

	// Worker 2 attempts claim; must recover the abandoned job
	recovered, isRec, err := s.ClaimNextJob(ctx, "worker-alive", 60*time.Second)
	if err != nil || recovered == nil {
		t.Fatalf("recovery claim failed: %v", err)
	}
	if !isRec {
		t.Errorf("expected isRecovery to be true")
	}
	if recovered.ID != job.ID {
		t.Errorf("expected to recover %s, got %s", job.ID, recovered.ID)
	}
	if recovered.ClaimedBy != "worker-alive" {
		t.Errorf("expected claimed_by 'worker-alive', got '%s'", recovered.ClaimedBy)
	}
}

func TestJobCancellation(t *testing.T) {
	s, cleanup := setupTestStorage(t)
	defer cleanup()

	ctx := context.Background()
	job, _ := s.CreateJob(ctx, "Cancellable Task", 5, 12, "{}")

	claimed, _, _ := s.ClaimNextJob(ctx, "w1", 60*time.Second)

	if err := s.CancelJob(ctx, claimed.ID); err != nil {
		t.Fatalf("cancel job err: %v", err)
	}

	// Worker heartbeat must immediately detect cancellation
	err := s.RenewLease(ctx, claimed.ID, "w1", 60*time.Second)
	if !errors.Is(err, ErrJobCancelled) {
		t.Errorf("expected ErrJobCancelled on renewal, got: %v", err)
	}

	updated, _ := s.GetJob(ctx, job.ID)
	if updated.Status != StatusCancelled {
		t.Errorf("expected status 'cancelled', got '%s'", updated.Status)
	}
}

func TestDurableEventsRecord(t *testing.T) {
	s, cleanup := setupTestStorage(t)
	defer cleanup()

	ctx := context.Background()
	job, _ := s.CreateJob(ctx, "Event Logging Task", 5, 12, "{}")

	_ = s.RecordEvent(ctx, job.ID, EventStepProgress, "planning", "w1", "Step 1 complete", `{"step":1}`)
	_ = s.RecordEvent(ctx, job.ID, EventStepProgress, "execution", "w1", "Step 2 complete", `{"step":2}`)

	events, err := s.GetJobEvents(ctx, job.ID)
	if err != nil {
		t.Fatalf("get events err: %v", err)
	}

	if len(events) != 3 { // job_created + 2 progress events
		t.Fatalf("expected 3 events, got %d", len(events))
	}
	if events[0].EventType != EventJobCreated {
		t.Errorf("first event not job_created: %s", events[0].EventType)
	}
}
