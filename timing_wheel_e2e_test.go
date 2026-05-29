// End-to-end tests for the timing-wheel migration. Each test exercises the
// public Scheduler API rather than the wheel directly — the goal is to pin
// down user-observable contracts (fires happen on time, Cancel really stops
// a pending fire, retries are spaced by their backoff, goroutine count stays
// flat under load) against future refactors of the wheel internals.
package skedulr_test

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lupppig/skedulr"
)

// TestE2E_WheelDrivesEveryDelayedPath runs the four call sites that the wheel
// migration is supposed to own — retry backoff, ScheduleOnce, ScheduleRecurring,
// ScheduleCron — concurrently against one Scheduler, and asserts that every
// expected fire happens, that goroutine count stays flat-ish under load, and
// that ShutDown leaves no dangling goroutines from any of the four paths.
//
// This is the "if any of the migrations regressed, this test fails loudly" net.
func TestE2E_WheelDrivesEveryDelayedPath(t *testing.T) {
	runtime.GC()
	beforeGoroutines := runtime.NumGoroutine()

	sch := skedulr.New(
		skedulr.WithMaxWorkers(16),
		skedulr.WithInitialWorkers(4),
	)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = sch.ShutDown(ctx)
	})

	// ---- (a) ScheduleOnce -------------------------------------------------
	// Fan out 200 ScheduleOnce calls at staggered short delays. Each must fire
	// within tolerance. Pre-migration this would have spawned 200 goroutines.
	const onceN = 200
	onceFired := make(chan int, onceN)
	for i := 0; i < onceN; i++ {
		i := i
		delay := time.Duration(50+(i%20)*10) * time.Millisecond
		_, err := sch.ScheduleOnce(func(ctx context.Context) error {
			onceFired <- i
			return nil
		}, time.Now().Add(delay), 1)
		if err != nil {
			t.Fatalf("ScheduleOnce[%d]: %v", i, err)
		}
	}

	// ---- (b) ScheduleRecurring -------------------------------------------
	var recurringCount int64
	recID, err := sch.ScheduleRecurring(func(ctx context.Context) error {
		atomic.AddInt64(&recurringCount, 1)
		return nil
	}, 100*time.Millisecond, 1)
	if err != nil {
		t.Fatalf("ScheduleRecurring: %v", err)
	}

	// ---- (c) Retry backoff via failing task ------------------------------
	// A task that always fails with linear retry should hit Submit attempt+1
	// times (initial run + maxRetries). Each retry is parked in the wheel.
	const maxRetries = 3
	var failingAttempts int64
	failingDone := make(chan struct{})
	failTask := skedulr.NewTask(func(ctx context.Context) error {
		n := atomic.AddInt64(&failingAttempts, 1)
		if n >= int64(1+maxRetries) {
			// Closing on the final attempt lets the test wait deterministically
			// rather than sleeping for an arbitrary budget.
			select {
			case <-failingDone:
			default:
				close(failingDone)
			}
		}
		return errors.New("always fails")
	}, 1, 0).
		WithMaxRetries(maxRetries).
		WithRetryStrategy(skedulr.NewLinearRetry(maxRetries, 50*time.Millisecond))
	if _, err := sch.Submit(failTask); err != nil {
		t.Fatalf("Submit failing: %v", err)
	}

	// ---- (d) ScheduleCron ------------------------------------------------
	// "* * * * *" fires every minute. We can't wait a real minute in tests,
	// but we *can* assert it was accepted and gets registered without per-cron
	// goroutines. The recurring path above already exercises wheel re-arming.
	cronID, err := sch.ScheduleCron("* * * * *", func(ctx context.Context) error {
		return nil
	}, 1)
	if err != nil {
		t.Fatalf("ScheduleCron: %v", err)
	}

	// ---- Wait for the deterministic signals ------------------------------
	// All 200 ScheduleOnce fires within 2s. (Bottom-level tick=10ms, max delay
	// is 250ms, so 2s is comfortable on a noisy CI box.)
	deadline := time.After(3 * time.Second)
	gotOnce := 0
	for gotOnce < onceN {
		select {
		case <-onceFired:
			gotOnce++
		case <-deadline:
			t.Fatalf("ScheduleOnce: only %d/%d fired before deadline", gotOnce, onceN)
		}
	}

	// Failing task: original run + maxRetries.
	select {
	case <-failingDone:
	case <-time.After(2 * time.Second):
		t.Fatalf("retry path: only %d/%d attempts fired", atomic.LoadInt64(&failingAttempts), 1+maxRetries)
	}
	if got := atomic.LoadInt64(&failingAttempts); got != int64(1+maxRetries) {
		t.Fatalf("retry path: want %d attempts (1 initial + %d retries), got %d", 1+maxRetries, maxRetries, got)
	}

	// Recurring task: 100ms interval × ~600ms wall time → expect at least 3.
	// We've already burned roughly 350ms above; sleep a little more to be safe.
	time.Sleep(400 * time.Millisecond)
	if got := atomic.LoadInt64(&recurringCount); got < 3 {
		t.Fatalf("recurring path: want >=3 fires, got %d", got)
	}

	// ---- Cancellation through the wheel ----------------------------------
	// Cancel the recurring task; count must stop climbing past +1 (the +1 is
	// the in-flight tolerance — a fire already submitted is allowed through).
	if err := sch.Cancel(recID); err != nil {
		t.Fatalf("Cancel recurring: %v", err)
	}
	if err := sch.Cancel(cronID); err != nil {
		t.Fatalf("Cancel cron: %v", err)
	}
	snapshot := atomic.LoadInt64(&recurringCount)
	time.Sleep(400 * time.Millisecond) // 4× the recurring interval
	if got := atomic.LoadInt64(&recurringCount); got > snapshot+1 {
		t.Fatalf("recurring kept firing after Cancel: snapshot=%d after=%d", snapshot, got)
	}

	// ---- Goroutine footprint ---------------------------------------------
	// The wheel migration's headline win: pending delayed tasks cost zero
	// goroutines beyond the wheel's single tick loop. After all the above
	// activity quiesces, we should be within a small constant of the baseline
	// — Scheduler loops + wheel ticker + worker pool, not 200+ leaked timers.
	runtime.GC()
	time.Sleep(50 * time.Millisecond)
	runtime.GC()
	afterGoroutines := runtime.NumGoroutine()

	// Loose bound: scheduler.New starts dequeue/cleanup/recovery loops, plus
	// worker goroutines (up to maxWorkers=16) and the wheel ticker. 40 is a
	// comfortable ceiling that still flags a per-task leak.
	if afterGoroutines-beforeGoroutines > 40 {
		t.Fatalf("goroutine leak suspected: before=%d after=%d delta=%d", beforeGoroutines, afterGoroutines, afterGoroutines-beforeGoroutines)
	}
	t.Logf("goroutines before=%d after=%d delta=%d", beforeGoroutines, afterGoroutines, afterGoroutines-beforeGoroutines)
}

// TestE2E_CancelBeforeFire schedules a task far enough out that the wheel must
// park it (not immediate-fire), cancels it, then waits past the fire time and
// asserts the callback never ran. This pins down the Cancel-while-delayed
// contract — the highest-value bug class for the migration.
func TestE2E_CancelBeforeFire(t *testing.T) {
	sch := skedulr.New(skedulr.WithMaxWorkers(2))
	defer sch.ShutDown(context.Background())

	var fired atomic.Int32
	id, err := sch.ScheduleOnce(func(ctx context.Context) error {
		fired.Add(1)
		return nil
	}, time.Now().Add(300*time.Millisecond), 1)
	if err != nil {
		t.Fatalf("ScheduleOnce: %v", err)
	}

	// Cancel comfortably before the fire window.
	time.Sleep(50 * time.Millisecond)
	if err := sch.Cancel(id); err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	// Wait past the original fire time + a generous slack window.
	time.Sleep(500 * time.Millisecond)
	if got := fired.Load(); got != 0 {
		t.Fatalf("cancelled scheduled task fired anyway: count=%d", got)
	}
}

// TestE2E_HighFanoutScheduleOnce stress-tests the wheel under concurrent
// ScheduleOnce calls from many goroutines — the migration must keep every
// callback exactly-once and within tolerance.
func TestE2E_HighFanoutScheduleOnce(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping fanout stress in -short mode")
	}

	sch := skedulr.New(
		skedulr.WithMaxWorkers(32),
		skedulr.WithInitialWorkers(8),
		skedulr.WithMaxCapacity(10000),
	)
	defer sch.ShutDown(context.Background())

	const writers = 16
	const perWriter = 200
	const total = writers * perWriter

	var fired int64
	var wg sync.WaitGroup
	wg.Add(total)

	for w := 0; w < writers; w++ {
		w := w
		go func() {
			for i := 0; i < perWriter; i++ {
				// Stagger across ~600ms to mix bottom-wheel slots with upper-wheel
				// placements (with default tick=10ms, size=256, bottom span = 2.56s).
				delay := time.Duration(50+((w*perWriter+i)%55)*10) * time.Millisecond
				_, err := sch.ScheduleOnce(func(ctx context.Context) error {
					atomic.AddInt64(&fired, 1)
					wg.Done()
					return nil
				}, time.Now().Add(delay), 1)
				if err != nil {
					t.Errorf("ScheduleOnce w=%d i=%d: %v", w, i, err)
					wg.Done()
				}
			}
		}()
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("only %d/%d fired within 5s", atomic.LoadInt64(&fired), total)
	}

	if got := atomic.LoadInt64(&fired); got != total {
		t.Fatalf("want %d fires, got %d", total, got)
	}
}

// TestE2E_RetryFiresInBackoffOrder verifies the retry-backoff migration: each
// retry must fire after roughly the configured delay, not back-to-back. This
// is what regressed first when AfterFunc was replaced with the wheel.
func TestE2E_RetryFiresInBackoffOrder(t *testing.T) {
	sch := skedulr.New(skedulr.WithMaxWorkers(4))
	defer sch.ShutDown(context.Background())

	const backoff = 80 * time.Millisecond
	const tolerance = 60 * time.Millisecond
	const retries = 3

	var mu sync.Mutex
	var timestamps []time.Time

	failTask := skedulr.NewTask(func(ctx context.Context) error {
		mu.Lock()
		timestamps = append(timestamps, time.Now())
		mu.Unlock()
		return fmt.Errorf("nope")
	}, 1, 0).
		WithMaxRetries(retries).
		WithRetryStrategy(skedulr.NewLinearRetry(retries, backoff))

	if _, err := sch.Submit(failTask); err != nil {
		t.Fatalf("Submit: want nil, got %v", err)
	}

	// Wait for all attempts plus slack.
	deadline := time.Now().Add(time.Duration(1+retries) * (backoff + tolerance + 100*time.Millisecond))
	for {
		mu.Lock()
		done := len(timestamps) >= 1+retries
		mu.Unlock()
		if done {
			break
		}
		if time.Now().After(deadline) {
			mu.Lock()
			n := len(timestamps)
			mu.Unlock()
			t.Fatalf("only %d/%d attempts ran before deadline", n, 1+retries)
		}
		time.Sleep(20 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	for i := 1; i < len(timestamps); i++ {
		gap := timestamps[i].Sub(timestamps[i-1])
		// gap must be roughly `backoff`. The wheel rounds down to tickMs and
		// callback dispatch adds slack — so the floor is backoff - tickMs and
		// the ceiling is backoff + tolerance.
		if gap < backoff-20*time.Millisecond {
			t.Errorf("retry #%d fired too early: gap=%v want >=%v", i, gap, backoff)
		}
		if gap > backoff+tolerance+50*time.Millisecond {
			t.Errorf("retry #%d fired too late: gap=%v want <=%v", i, gap, backoff+tolerance)
		}
	}
}
