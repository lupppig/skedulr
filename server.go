package skedulr

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

//go:embed web/dashboard.html
var dashboardHTML []byte

func (s *Scheduler) DashboardHandler() http.Handler {
	mux := http.NewServeMux()

	// Simple rate limiting: 10 requests per second
	limitChan := make(chan struct{}, 10)
	for i := 0; i < 10; i++ {
		limitChan <- struct{}{}
	}

	go func() {
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				select {
				case limitChan <- struct{}{}:
				default:
				}
			case <-s.stop:
				return
			}
		}
	}()

	throttle := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-limitChan:
				next.ServeHTTP(w, r)
			default:
				http.Error(w, "Too many requests", http.StatusTooManyRequests)
			}
		}
	}

	mux.HandleFunc("/api/stats", throttle(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		limit := 50
		filter := HistoryFilter{
			ID:     r.URL.Query().Get("id"),
			Type:   r.URL.Query().Get("type"),
			Status: r.URL.Query().Get("status"),
			Query:  r.URL.Query().Get("q"),
			Limit:  limit,
		}
		json.NewEncoder(w).Encode(s.StatsWithFilter(filter))
	}))

	mux.HandleFunc("/api/pause", throttle(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			s.Pause()
			w.WriteHeader(http.StatusOK)
		}
	}))

	mux.HandleFunc("/api/resume", throttle(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			s.Resume()
			w.WriteHeader(http.StatusOK)
		}
	}))

	mux.HandleFunc("/api/cancel", throttle(func(w http.ResponseWriter, r *http.Request) {
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
	}))

	mux.HandleFunc("/api/resubmit", throttle(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		id := r.URL.Query().Get("id")
		if id == "" {
			http.Error(w, "Missing id", http.StatusBadRequest)
			return
		}
		if err := s.Resubmit(id); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))

	mux.HandleFunc("/api/scale", throttle(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		pool := r.URL.Query().Get("pool")
		countStr := r.URL.Query().Get("count")
		if pool == "" || countStr == "" {
			http.Error(w, "Missing pool or count", http.StatusBadRequest)
			return
		}

		var count int
		fmt.Sscanf(countStr, "%d", &count)
		s.ScalePool(pool, count)
		w.WriteHeader(http.StatusOK)
	}))

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
	tasks := make([]TaskInfo, 0, len(s.tasks))
	for _, t := range s.tasks {
		// ... existing filtering logic (truncated for readability)
		if filter.ID != "" && t.id != filter.ID {
			continue
		}
		if filter.Type != "" && t.typeName != filter.Type {
			continue
		}
		statusStr := t.status.String()
		if filter.Status != "" && statusStr != filter.Status {
			continue
		}
		if filter.Query != "" {
			q := strings.ToLower(filter.Query)
			match := strings.Contains(strings.ToLower(t.id), q) ||
				strings.Contains(strings.ToLower(t.typeName), q)
			if !match {
				continue
			}
		}
		tasks = append(tasks, TaskInfo{
			ID:       t.id,
			Key:      t.key,
			Pool:     t.pool,
			Type:     t.typeName,
			Status:   statusStr,
			Priority: t.priority,
			Progress: t.progress,
		})
	}

	pools := make([]PoolStats, 0, len(s.poolQueues))
	for name, ch := range s.poolQueues {
		pools = append(pools, PoolStats{
			Name:      name,
			Workers:   s.poolWorkers[name],
			QueueSize: len(ch),
		})
	}

	stats := Stats{
		QueueSize:      atomic.LoadInt64(&s.queueSize),
		SuccessCount:   atomic.LoadInt64(&s.successCount),
		FailureCount:   atomic.LoadInt64(&s.failureCount),
		DeadCount:      atomic.LoadInt64(&s.deadCount),
		PanicCount:     atomic.LoadInt64(&s.panicCount),
		CurrentWorkers: atomic.LoadInt32(&s.currentWorkers),
		ActiveTasks:    tasks,
		Pools:          pools,
		IsPaused:       s.IsPaused(),
	}
	s.mu.Unlock()

	history, _ := s.storage.GetHistory(context.Background(), filter)
	stats.History = history

	return stats
}

// Stats holds scheduler metrics.
type Stats struct {
	QueueSize      int64       `json:"queue_size"`
	SuccessCount   int64       `json:"success_count"`
	FailureCount   int64       `json:"failure_count"`
	DeadCount      int64       `json:"dead_count"`
	PanicCount     int64       `json:"panic_count"`
	CurrentWorkers int32       `json:"current_workers"`
	ActiveTasks    []TaskInfo  `json:"active_tasks"`
	History        []TaskInfo  `json:"history"`
	Pools          []PoolStats `json:"pools"`
	IsPaused       bool        `json:"is_paused"`
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

// Dashboard returns an http.Handler that serves the operations dashboard.
// It automatically handles the prefixing for API calls and static assets.
func (s *Scheduler) Dashboard(prefix string) http.Handler {
	handler := s.DashboardHandler()
	if prefix == "" || prefix == "/" {
		return handler
	}
	return http.StripPrefix(strings.TrimSuffix(prefix, "/"), handler)
}
