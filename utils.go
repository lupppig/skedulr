package skedulr

import (
	"math"
	"runtime"

	"github.com/google/uuid"
)

func generateId() string {
	return uuid.New().String()
}

func calculateWorkerPertTask(queueSize int) int {
	maxWorkers := runtime.NumCPU() * 2
	workers := int(math.Ceil(float64(queueSize) / 10.0))

	if workers < 1 {
		workers = 1
	} else if workers > maxWorkers {
		workers = maxWorkers
	}
	return workers
}
