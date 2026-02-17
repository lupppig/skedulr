package skedulr

import (
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
		json.NewEncoder(w).Encode(s.Stats())
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
		w.Write(dashboardHTML)
	})

	return mux
}

// Stats returns the current scheduler statistics.
func (s *Scheduler) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()

	tasks := make([]TaskInfo, 0, len(s.tasks))
	for _, t := range s.tasks {
		tasks = append(tasks, TaskInfo{
			ID:       t.id,
			Key:      t.key,
			Type:     t.typeName,
			Status:   t.status.String(),
			Priority: t.priority,
		})
	}

	history := make([]TaskInfo, 0, len(s.history))
	for _, t := range s.history {
		history = append(history, TaskInfo{
			ID:       t.id,
			Key:      t.key,
			Type:     t.typeName,
			Status:   t.status.String(),
			Priority: t.priority,
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
	}
}

// Stats holds scheduler metrics.
type Stats struct {
	QueueSize      int64      `json:"queue_size"`
	SuccessCount   int64      `json:"success_count"`
	FailureCount   int64      `json:"failure_count"`
	PanicCount     int64      `json:"panic_count"`
	CurrentWorkers int32      `json:"current_workers"`
	ActiveTasks    []TaskInfo `json:"active_tasks"`
	History        []TaskInfo `json:"history"`
}

// TaskInfo holds basic info about a task for the dashboard.
type TaskInfo struct {
	ID       string `json:"id"`
	Key      string `json:"key,omitempty"`
	Type     string `json:"type,omitempty"`
	Status   string `json:"status"`
	Priority int    `json:"priority"`
}
