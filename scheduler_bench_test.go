package skedulr_test

import (
	"context"
	"testing"

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
