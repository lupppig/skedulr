package skedulr

import (
	"fmt"
	"math/rand"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// tickTolerance is the slack we allow between scheduled fire time and observed fire
// time. Bottom-level tick rounding alone can push a fire up to `tickMs` late, and
// goroutine scheduling adds more. 50ms is comfortable on a busy CI box.
const tickTolerance = 50 * time.Millisecond

func newTestWheel(t *testing.T) *TimingWheel {
	t.Helper()
	tw := NewTimingWheel(10*time.Millisecond, 32)
	tw.Start()
	t.Cleanup(tw.Stop)
	return tw
}

// TestWheelBasicFire schedules N tasks at varying short delays and verifies all
// callbacks fire within tickTolerance of their target time.
func TestWheelBasicFire(t *testing.T) {
	tw := newTestWheel(t)

	type sample struct {
		want time.Time
		got  time.Time
	}
	const N = 50
	samples := make([]sample, N)
	var wg sync.WaitGroup
	wg.Add(N)

	for i := 0; i < N; i++ {
		i := i
		delay := time.Duration(20+rand.Intn(400)) * time.Millisecond
		fireAt := time.Now().Add(delay)
		samples[i].want = fireAt
		if err := tw.Schedule(fmt.Sprintf("t-%d", i), fireAt, func() {
			samples[i].got = time.Now()
			wg.Done()
		}); err != nil {
			t.Fatalf("schedule: %v", err)
		}
	}

	wg.Wait()

	var lateCount int
	for i, s := range samples {
		drift := s.got.Sub(s.want)
		if drift < -tickTolerance || drift > tickTolerance {
			lateCount++
			if lateCount <= 3 {
				t.Logf("sample %d drift = %v (want ±%v)", i, drift, tickTolerance)
			}
		}
	}
	if lateCount > 0 {
		t.Fatalf("%d/%d samples fired outside ±%v", lateCount, N, tickTolerance)
	}
}

// TestWheelCancel verifies a cancelled entry never fires.
func TestWheelCancel(t *testing.T) {
	tw := newTestWheel(t)

	var fired int32
	if err := tw.Schedule("cancel-me", time.Now().Add(50*time.Millisecond), func() {
		atomic.StoreInt32(&fired, 1)
	}); err != nil {
		t.Fatalf("schedule: %v", err)
	}

	// Cancel well before fire time.
	time.Sleep(10 * time.Millisecond)
	tw.Cancel("cancel-me")

	// Sleep past the original fire time plus tolerance.
	time.Sleep(150 * time.Millisecond)

	if atomic.LoadInt32(&fired) != 0 {
		t.Fatal("cancelled entry fired")
	}
}

// TestWheelRescheduleSameID verifies that scheduling twice with the same taskID
// cancels the first scheduled callback — only the second fires.
func TestWheelRescheduleSameID(t *testing.T) {
	tw := newTestWheel(t)

	var firstFired, secondFired int32
	_ = tw.Schedule("dup", time.Now().Add(40*time.Millisecond), func() {
		atomic.StoreInt32(&firstFired, 1)
	})
	// Reschedule before first fires.
	time.Sleep(5 * time.Millisecond)
	_ = tw.Schedule("dup", time.Now().Add(80*time.Millisecond), func() {
		atomic.StoreInt32(&secondFired, 1)
	})

	time.Sleep(200 * time.Millisecond)

	if atomic.LoadInt32(&firstFired) != 0 {
		t.Fatal("first callback fired despite being superseded")
	}
	if atomic.LoadInt32(&secondFired) != 1 {
		t.Fatal("second callback did not fire")
	}
}

// TestWheelHierarchy schedules an entry far enough out to force upper-level
// placement, then verifies the demotion path fires exactly once.
//
// Bottom level: tick=10ms, size=8 → span 80ms.
// Level 1: tick=80ms, size=8 → span 640ms.
// Delay of 200ms forces placement in level 1, then demotion into level 0.
func TestWheelHierarchy(t *testing.T) {
	tw := NewTimingWheel(10*time.Millisecond, 8)
	tw.Start()
	defer tw.Stop()

	var count int32
	target := time.Now().Add(200 * time.Millisecond)
	if err := tw.Schedule("h", target, func() {
		atomic.AddInt32(&count, 1)
	}); err != nil {
		t.Fatalf("schedule: %v", err)
	}

	// Sanity: overflow level must have been allocated.
	time.Sleep(5 * time.Millisecond)
	if tw.base.overflow.Load() == nil {
		t.Fatal("expected overflow wheel to be allocated for 200ms delay")
	}

	time.Sleep(300 * time.Millisecond)
	if got := atomic.LoadInt32(&count); got != 1 {
		t.Fatalf("expected exactly 1 fire after hierarchy demotion, got %d", got)
	}
}

// TestWheelPastFire — scheduling with a fireAt in the past fires the callback
// immediately (on a dispatcher goroutine, so insert never blocks).
func TestWheelPastFire(t *testing.T) {
	tw := newTestWheel(t)

	done := make(chan struct{})
	if err := tw.Schedule("past", time.Now().Add(-time.Hour), func() {
		close(done)
	}); err != nil {
		t.Fatalf("schedule: %v", err)
	}

	select {
	case <-done:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("past-due callback did not fire")
	}
}

// TestWheelGoroutineCount inserts a large batch of pending entries and verifies
// the wheel itself only spends one goroutine, regardless of pending count.
func TestWheelGoroutineCount(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping goroutine-count test in -short mode")
	}

	// Baseline before any wheel work.
	runtime.GC()
	before := runtime.NumGoroutine()

	tw := NewTimingWheel(10*time.Millisecond, 256)
	tw.Start()
	defer tw.Stop()

	// Schedule 10k entries all far in the future — none of them should fire
	// during the measurement window.
	const N = 10000
	farFuture := time.Now().Add(1 * time.Hour)
	for i := 0; i < N; i++ {
		_ = tw.Schedule(fmt.Sprintf("g-%d", i), farFuture, func() {})
	}

	// Let things settle.
	time.Sleep(50 * time.Millisecond)
	runtime.GC()
	after := runtime.NumGoroutine()

	// We expect +1 for the tick loop. Allow +3 for test/runtime noise.
	if after-before > 3 {
		t.Fatalf("goroutine count grew by %d (before=%d after=%d) for %d pending entries — wheel is not O(1) in goroutines",
			after-before, before, after, N)
	}
}

// TestWheelManyConcurrentSchedules stress-tests concurrent inserts and verifies
// every callback fires exactly once.
func TestWheelManyConcurrentSchedules(t *testing.T) {
	tw := newTestWheel(t)

	const N = 2000
	var fired int64
	var wg sync.WaitGroup
	wg.Add(N)

	// Fan out from many goroutines to exercise the locks.
	const writers = 16
	per := N / writers
	for w := 0; w < writers; w++ {
		w := w
		go func() {
			for i := 0; i < per; i++ {
				delay := time.Duration(20+rand.Intn(300)) * time.Millisecond
				_ = tw.Schedule(fmt.Sprintf("c-%d-%d", w, i), time.Now().Add(delay), func() {
					atomic.AddInt64(&fired, 1)
					wg.Done()
				})
			}
		}()
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("only %d/%d callbacks fired within 2s", atomic.LoadInt64(&fired), N)
	}

	if got := atomic.LoadInt64(&fired); got != N {
		t.Fatalf("expected %d fires, got %d", N, got)
	}
}

// TestWheelScheduleBeforeStart returns an error rather than silently dropping.
func TestWheelScheduleBeforeStart(t *testing.T) {
	tw := NewTimingWheel(10*time.Millisecond, 32)
	defer tw.Stop()
	if err := tw.Schedule("x", time.Now().Add(time.Second), func() {}); err != ErrWheelNotStarted {
		t.Fatalf("want ErrWheelNotStarted, got %v", err)
	}
}

// TestWheelScheduleAfterStop returns an error rather than silently dropping.
func TestWheelScheduleAfterStop(t *testing.T) {
	tw := NewTimingWheel(10*time.Millisecond, 32)
	tw.Start()
	tw.Stop()
	if err := tw.Schedule("x", time.Now().Add(time.Second), func() {}); err != ErrWheelStopped {
		t.Fatalf("want ErrWheelStopped, got %v", err)
	}
}

// TestWheelNilCallback rejects nil callbacks early rather than panicking at fire time.
func TestWheelNilCallback(t *testing.T) {
	tw := newTestWheel(t)
	if err := tw.Schedule("x", time.Now().Add(time.Second), nil); err != ErrNilCallback {
		t.Fatalf("want ErrNilCallback, got %v", err)
	}
}

// BenchmarkWheelInsert measures pure insert throughput into a started wheel with
// no contention from firing (all entries land far in the future).
func BenchmarkWheelInsert(b *testing.B) {
	tw := NewTimingWheel(10*time.Millisecond, 256)
	tw.Start()
	defer tw.Stop()

	farFuture := time.Now().Add(time.Hour)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = tw.Schedule("", farFuture, func() {})
	}
}

// BenchmarkWheelInsertCancel measures interleaved insert + cancel — the path
// that retry backoff hits when many tasks are reqeued and cancelled mid-flight.
func BenchmarkWheelInsertCancel(b *testing.B) {
	tw := NewTimingWheel(10*time.Millisecond, 256)
	tw.Start()
	defer tw.Stop()

	farFuture := time.Now().Add(time.Hour)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		id := fmt.Sprintf("b-%d", i)
		_ = tw.Schedule(id, farFuture, func() {})
		tw.Cancel(id)
	}
}
