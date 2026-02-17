package skedulr

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// PersistentTask represents the serializable state of a task.
type PersistentTask struct {
	ID            string        `json:"id"`
	Key           string        `json:"key,omitempty"`
	TypeName      string        `json:"type_name"`
	Payload       []byte        `json:"payload,omitempty"`
	Priority      int           `json:"priority"`
	Timeout       time.Duration `json:"timeout"`
	Attempts      int           `json:"attempts"`
	RetryStrategy string        `json:"retry_strategy,omitempty"`
}

// Storage defines the interface for persisting tasks.
type Storage interface {
	Save(ctx context.Context, t *PersistentTask) error
	Delete(ctx context.Context, id string) error
	LoadAll(ctx context.Context) ([]*PersistentTask, error)
	Claim(ctx context.Context, id string, instanceID string, duration time.Duration) (bool, error)
	Heartbeat(ctx context.Context, id string, instanceID string, duration time.Duration) error
}

// InMemoryStorage is a no-op storage implementation used as a fallback.
type InMemoryStorage struct{}

func (s *InMemoryStorage) Save(ctx context.Context, t *PersistentTask) error      { return nil }
func (s *InMemoryStorage) Delete(ctx context.Context, id string) error            { return nil }
func (s *InMemoryStorage) LoadAll(ctx context.Context) ([]*PersistentTask, error) { return nil, nil }
func (s *InMemoryStorage) Claim(ctx context.Context, id, instanceID string, d time.Duration) (bool, error) {
	return true, nil
}
func (s *InMemoryStorage) Heartbeat(ctx context.Context, id, instanceID string, d time.Duration) error {
	return nil
}

// RedisStorage implements Storage using Redis.
type RedisStorage struct {
	client *redis.Client
	prefix string
}

// NewRedisStorage creates a new RedisStorage instance.
func NewRedisStorage(addr, password string, db int, prefix string) *RedisStorage {
	rdb := redis.NewClient(&redis.Options{
		Addr:     addr,
		Password: password,
		DB:       db,
	})
	if prefix == "" {
		prefix = "skedulr:task:"
	}
	return &RedisStorage{
		client: rdb,
		prefix: prefix,
	}
}

func (s *RedisStorage) Save(ctx context.Context, t *PersistentTask) error {
	data, err := json.Marshal(t)
	if err != nil {
		return fmt.Errorf("failed to marshal task: %w", err)
	}
	return s.client.Set(ctx, s.prefix+t.ID, data, 0).Err()
}

func (s *RedisStorage) Delete(ctx context.Context, id string) error {
	return s.client.Del(ctx, s.prefix+id).Err()
}

func (s *RedisStorage) LoadAll(ctx context.Context) ([]*PersistentTask, error) {
	keys, err := s.client.Keys(ctx, s.prefix+"*").Result()
	if err != nil {
		return nil, fmt.Errorf("failed to list keys: %w", err)
	}

	tasks := make([]*PersistentTask, 0, len(keys))
	for _, key := range keys {
		data, err := s.client.Get(ctx, key).Bytes()
		if err != nil {
			continue // Skip failed reads? Or return error?
		}
		var t PersistentTask
		if err := json.Unmarshal(data, &t); err != nil {
			continue
		}
		tasks = append(tasks, &t)
	}
	return tasks, nil
}

func (s *RedisStorage) Claim(ctx context.Context, id, instanceID string, d time.Duration) (bool, error) {
	leaseKey := s.prefix + id + ":lease"
	ok, err := s.client.SetNX(ctx, leaseKey, instanceID, d).Result()
	if err != nil {
		return false, fmt.Errorf("failed to claim task %s: %w", id, err)
	}
	return ok, nil
}

func (s *RedisStorage) Heartbeat(ctx context.Context, id, instanceID string, d time.Duration) error {
	leaseKey := s.prefix + id + ":lease"
	// Only update if we still own it
	val, err := s.client.Get(ctx, leaseKey).Result()
	if err != nil {
		return fmt.Errorf("failed to check lease for %s: %w", id, err)
	}
	if val != instanceID {
		return fmt.Errorf("lease for %s lost (owned by %s)", id, val)
	}
	return s.client.Expire(ctx, leaseKey, d).Err()
}
