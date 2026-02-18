package skedulr

import (
	"context"
	_ "embed"
	"encoding/json"
	"net/http"
	"sync/atomic"
)

//go:embed web/dashboard.html
var dashboardHTML []byte

func (s *Scheduler) DashboardHandler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/api/stats", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		limit := 50
		filter := HistoryFilter{
			ID:     r.URL.Query().Get("id"),
			Type:   r.URL.Query().Get("type"),
			Status: r.URL.Query().Get("status"),
			Limit:  limit,
		}
		json.NewEncoder(w).Encode(s.StatsWithFilter(filter))
	})

	mux.HandleFunc("/api/cancel", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		id := r.URL.Query().Get("id")
		if id == "" {
			http.Error(w, "Missing id", http.StatusBadRequest)
			return
		}
		s.Cancel(id)
		w.WriteHeader(http.StatusOK)
	})

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, max-age=0")
		w.Header().Set("Pragma", "no-cache")
		w.Write(dashboardHTML)
	})

	return mux
}

// Stats returns the current scheduler statistics with default limits.
func (s *Scheduler) Stats() Stats {
	return s.StatsWithFilter(HistoryFilter{Limit: 50})
}

// StatsWithFilter returns the current scheduler statistics with custom history filtering.
func (s *Scheduler) StatsWithFilter(filter HistoryFilter) Stats {
	s.mu.Lock()
	defer s.mu.Unlock()

	tasks := make([]TaskInfo, 0, len(s.tasks))
	for _, t := range s.tasks {
		tasks = append(tasks, TaskInfo{
			ID:       t.id,
			Key:      t.key,
			Pool:     t.pool,
			Type:     t.typeName,
			Status:   t.status.String(),
			Priority: t.priority,
			Progress: t.progress,
		})
	}

	history, _ := s.storage.GetHistory(context.Background(), filter)

	pools := make([]PoolStats, 0, len(s.poolQueues))
	for name, ch := range s.poolQueues {
		pools = append(pools, PoolStats{
			Name:      name,
			Workers:   s.poolWorkers[name],
			QueueSize: len(ch),
		})
	}

	return Stats{
		QueueSize:      atomic.LoadInt64(&s.queueSize),
		SuccessCount:   atomic.LoadInt64(&s.successCount),
		FailureCount:   atomic.LoadInt64(&s.failureCount),
		PanicCount:     atomic.LoadInt64(&s.panicCount),
		CurrentWorkers: atomic.LoadInt32(&s.currentWorkers),
		ActiveTasks:    tasks,
		History:        history,
		Pools:          pools,
	}
}

// Stats holds scheduler metrics.
type Stats struct {
	QueueSize      int64       `json:"queue_size"`
	SuccessCount   int64       `json:"success_count"`
	FailureCount   int64       `json:"failure_count"`
	PanicCount     int64       `json:"panic_count"`
	CurrentWorkers int32       `json:"current_workers"`
	ActiveTasks    []TaskInfo  `json:"active_tasks"`
	History        []TaskInfo  `json:"history"`
	Pools          []PoolStats `json:"pools"`
}

// PoolStats holds metrics for a specific worker pool.
type PoolStats struct {
	Name      string `json:"name"`
	Workers   int    `json:"workers"`
	QueueSize int    `json:"queue_size"`
}

// TaskInfo holds basic info about a task for the dashboard.
type TaskInfo struct {
	ID       string `json:"id"`
	Key      string `json:"key,omitempty"`
	Pool     string `json:"pool,omitempty"`
	Type     string `json:"type,omitempty"`
	Status   string `json:"status"`
	Priority int    `json:"priority"`
	Progress int    `json:"progress"`
}
