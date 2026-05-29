package skedulr_test

import (
	"context"
	"runtime"
	"testing"
	"time"

	"github.com/lupppig/skedulr"
)

func BenchmarkScheduleAndRun(b *testing.B) {
	s := skedulr.New(skedulr.WithMaxWorkers(100))
	defer s.ShutDown(context.Background())

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.Submit(skedulr.NewTask(func(ctx context.Context) error {
			return nil
		}, 1, 0))
	}
}

func BenchmarkMultipleWorkers(b *testing.B) {
	s := skedulr.New(skedulr.WithMaxWorkers(1000), skedulr.WithInitialWorkers(100))
	defer s.ShutDown(context.Background())

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			s.Submit(skedulr.NewTask(func(ctx context.Context) error {
				return nil
			}, 1, 0))
		}
	})
}

// BenchmarkScheduleOnce_Wheel exercises the post-migration ScheduleOnce path —
// every scheduled fire now lives in the timing wheel rather than its own
// timer + goroutine. Allocs/op should be flat regardless of N.
func BenchmarkScheduleOnce_Wheel(b *testing.B) {
	s := skedulr.New(skedulr.WithMaxWorkers(8))
	defer s.ShutDown(context.Background())

	// Far enough in the future that none fire during the bench window.
	at := time.Now().Add(time.Hour)
	job := func(ctx context.Context) error { return nil }

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.ScheduleOnce(job, at, 1); err != nil {
			b.Fatalf("ScheduleOnce: %v", err)
		}
	}
}

// BenchmarkScheduleOnce_GoroutineFootprint asserts the wheel keeps goroutine
// count flat across many pending scheduled tasks. Reported as ns/op for shape
// only; the real signal is the b.Log of before/after goroutine counts.
func BenchmarkScheduleOnce_GoroutineFootprint(b *testing.B) {
	s := skedulr.New(skedulr.WithMaxWorkers(8))
	defer s.ShutDown(context.Background())

	runtime.GC()
	before := runtime.NumGoroutine()

	at := time.Now().Add(time.Hour)
	job := func(ctx context.Context) error { return nil }

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.ScheduleOnce(job, at, 1)
	}
	b.StopTimer()
	runtime.GC()
	after := runtime.NumGoroutine()
	b.Logf("goroutines before=%d after=%d delta=%d (N=%d)", before, after, after-before, b.N)
}
