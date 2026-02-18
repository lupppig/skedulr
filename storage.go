package skedulr

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// PersistentTask represents the serializable state of a task.
type PersistentTask struct {
	ID            string        `json:"id"`
	Key           string        `json:"key,omitempty"`
	Pool          string        `json:"pool,omitempty"`
	TypeName      string        `json:"type_name,omitempty"`
	Payload       []byte        `json:"payload,omitempty"`
	Priority      int           `json:"priority"`
	Timeout       time.Duration `json:"timeout"`
	Attempts      int           `json:"attempts"`
	RetryStrategy string        `json:"retry_strategy,omitempty"`
	DependsOn     []string      `json:"depends_on,omitempty"`
}

var (
	luaHeartbeat = redis.NewScript(`
		local val = redis.call("GET", KEYS[1])
		if val == ARGV[1] then
			return redis.call("EXPIRE", KEYS[1], ARGV[2])
		else
			return 0
		end
	`)

	luaResolveDeps = redis.NewScript(`
		local child_ids = redis.call("SMEMBERS", KEYS[1])
		local ready_tasks = {}
		for _, child_id in ipairs(child_ids) do
			local deps_key = ARGV[1] .. child_id .. ":deps_count"
			local rem = redis.call("DECR", deps_key)
			if tonumber(rem) == 0 then
				local task_data = redis.call("GET", ARGV[1] .. child_id)
				if task_data then
					table.insert(ready_tasks, task_data)
					redis.call("DEL", deps_key)
				end
			end
		end
		redis.call("DEL", KEYS[1])
		return ready_tasks
	`)

	luaDequeue = redis.NewScript(`
		local res = redis.call("ZPOPMAX", KEYS[1])
		if #res == 0 then return nil end
		local task_data = res[1]
		local task = cjson.decode(task_data)
		local lease_key = ARGV[1] .. task.id .. ":lease"
		redis.call("SET", lease_key, ARGV[2], "EX", ARGV[3])
		return task_data
	`)
)

// Storage defines the interface for persisting tasks.
type Storage interface {
	Save(ctx context.Context, t *PersistentTask) error
	Delete(ctx context.Context, id string) error
	LoadAll(ctx context.Context) ([]*PersistentTask, error)
	Claim(ctx context.Context, id string, instanceID string, duration time.Duration) (bool, error)
	Heartbeat(ctx context.Context, id string, instanceID string, duration time.Duration) error
	PublishCancel(ctx context.Context, id string) error
	SubscribeCancel(ctx context.Context, onCancel func(id string)) error
	SaveWaiting(ctx context.Context, t *PersistentTask) error
	ResolveDependencies(ctx context.Context, parentID string) ([]*PersistentTask, error)
	Enqueue(ctx context.Context, t *PersistentTask) error
	Dequeue(ctx context.Context, instanceID string, duration time.Duration) (*PersistentTask, error)
	AddToHistory(ctx context.Context, t TaskInfo, retention time.Duration) error
	GetHistory(ctx context.Context, limit int) ([]TaskInfo, error)
}

// InMemoryStorage is a basic storage implementation used as a fallback.
type InMemoryStorage struct {
	mu      sync.Mutex
	history []TaskInfo
}

func (s *InMemoryStorage) Save(ctx context.Context, t *PersistentTask) error      { return nil }
func (s *InMemoryStorage) Delete(ctx context.Context, id string) error            { return nil }
func (s *InMemoryStorage) LoadAll(ctx context.Context) ([]*PersistentTask, error) { return nil, nil }
func (s *InMemoryStorage) Claim(ctx context.Context, id, instanceID string, d time.Duration) (bool, error) {
	return true, nil
}
func (s *InMemoryStorage) Heartbeat(ctx context.Context, id, instanceID string, d time.Duration) error {
	return nil
}
func (s *InMemoryStorage) PublishCancel(ctx context.Context, id string) error { return nil }
func (s *InMemoryStorage) SubscribeCancel(ctx context.Context, onCancel func(id string)) error {
	return nil
}
func (s *InMemoryStorage) SaveWaiting(ctx context.Context, t *PersistentTask) error { return nil }
func (s *InMemoryStorage) ResolveDependencies(ctx context.Context, parentID string) ([]*PersistentTask, error) {
	return nil, nil
}
func (s *InMemoryStorage) AddToHistory(ctx context.Context, t TaskInfo, retention time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.history = append([]TaskInfo{t}, s.history...)
	if len(s.history) > 100 {
		s.history = s.history[:100]
	}
	return nil
}
func (s *InMemoryStorage) Enqueue(ctx context.Context, t *PersistentTask) error { return nil }
func (s *InMemoryStorage) Dequeue(ctx context.Context, instanceID string, d time.Duration) (*PersistentTask, error) {
	return nil, nil
}
func (s *InMemoryStorage) GetHistory(ctx context.Context, limit int) ([]TaskInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if limit > len(s.history) {
		limit = len(s.history)
	}
	res := make([]TaskInfo, limit)
	copy(res, s.history[:limit])
	return res, nil
}

// RedisStorage implements Storage using Redis.
type RedisStorage struct {
	client *redis.Client
	prefix string
}

// NewRedisStorage creates a new RedisStorage instance.
func NewRedisStorage(addr, password string, db int, prefix string) *RedisStorage {
	redis.SetLogger(nil)

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
			continue
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
	res, err := s.client.SetArgs(ctx, leaseKey, instanceID, redis.SetArgs{
		Mode: "NX",
		TTL:  d,
	}).Result()
	if err != nil {
		if err == redis.Nil {
			return false, nil
		}
		return false, fmt.Errorf("failed to claim task %s: %w", id, err)
	}
	return res == "OK", nil
}

func (s *RedisStorage) Heartbeat(ctx context.Context, id, instanceID string, d time.Duration) error {
	leaseKey := s.prefix + id + ":lease"
	res, err := luaHeartbeat.Run(ctx, s.client, []string{leaseKey}, instanceID, int(d.Seconds())).Int()
	if err != nil {
		return fmt.Errorf("heartbeat failed for %s: %w", id, err)
	}
	if res == 0 {
		return fmt.Errorf("lease for %s lost or owned by another instance", id)
	}
	return nil
}

func (s *RedisStorage) SaveWaiting(ctx context.Context, t *PersistentTask) error {
	data, err := json.Marshal(t)
	if err != nil {
		return err
	}
	s.client.Set(ctx, s.prefix+t.ID, data, 0)

	for _, parentID := range t.DependsOn {
		s.client.SAdd(ctx, s.prefix+parentID+":children", t.ID)
	}
	s.client.Set(ctx, s.prefix+t.ID+":deps_count", len(t.DependsOn), 0)
	return nil
}

func (s *RedisStorage) ResolveDependencies(ctx context.Context, parentID string) ([]*PersistentTask, error) {
	childrenKey := s.prefix + parentID + ":children"
	res, err := luaResolveDeps.Run(ctx, s.client, []string{childrenKey}, s.prefix).StringSlice()
	if err != nil {
		return nil, fmt.Errorf("failed to resolve dependencies: %w", err)
	}

	readyTasks := make([]*PersistentTask, 0, len(res))
	for _, data := range res {
		var t PersistentTask
		if err := json.Unmarshal([]byte(data), &t); err == nil {
			readyTasks = append(readyTasks, &t)
		}
	}
	return readyTasks, nil
}

func (s *RedisStorage) PublishCancel(ctx context.Context, id string) error {
	channel := s.prefix + "cmd:cancel"
	return s.client.Publish(ctx, channel, id).Err()
}

func (s *RedisStorage) SubscribeCancel(ctx context.Context, onCancel func(id string)) error {
	channel := s.prefix + "cmd:cancel"
	pubsub := s.client.Subscribe(ctx, channel)

	go func() {
		defer pubsub.Close()
		ch := pubsub.Channel()
		for {
			select {
			case msg, ok := <-ch:
				if !ok {
					return
				}
				onCancel(msg.Payload)
			case <-ctx.Done():
				return
			}
		}
	}()
	return nil
}

func (s *RedisStorage) AddToHistory(ctx context.Context, t TaskInfo, retention time.Duration) error {
	data, err := json.Marshal(t)
	if err != nil {
		return err
	}
	key := s.prefix + "history"
	now := time.Now().UnixNano()
	pipe := s.client.Pipeline()
	pipe.ZAdd(ctx, key, redis.Z{
		Score:  float64(now),
		Member: data,
	})
	if retention > 0 {
		oldest := time.Now().Add(-retention).UnixNano()
		pipe.ZRemRangeByScore(ctx, key, "-inf", fmt.Sprintf("%d", oldest))
	} else {
		pipe.ZRemRangeByRank(ctx, key, 0, -101)
	}
	_, err = pipe.Exec(ctx)
	return err
}

func (s *RedisStorage) Enqueue(ctx context.Context, t *PersistentTask) error {
	data, err := json.Marshal(t)
	if err != nil {
		return err
	}
	key := s.prefix + "queue"
	// Use ZSET for priority queue. Score is priority.
	return s.client.ZAdd(ctx, key, redis.Z{
		Score:  float64(t.Priority),
		Member: data,
	}).Err()
}

func (s *RedisStorage) Dequeue(ctx context.Context, instanceID string, d time.Duration) (*PersistentTask, error) {
	key := s.prefix + "queue"
	res, err := luaDequeue.Run(ctx, s.client, []string{key}, s.prefix, instanceID, int(d.Seconds())).Result()
	if err != nil {
		if err == redis.Nil {
			return nil, nil
		}
		return nil, fmt.Errorf("dequeue failed: %w", err)
	}

	var t PersistentTask
	if err := json.Unmarshal([]byte(res.(string)), &t); err != nil {
		return nil, fmt.Errorf("failed to unmarshal task: %w", err)
	}
	return &t, nil
}

func (s *RedisStorage) GetHistory(ctx context.Context, limit int) ([]TaskInfo, error) {
	key := s.prefix + "history"
	data, err := s.client.ZRevRange(ctx, key, 0, int64(limit-1)).Result()
	if err != nil {
		return nil, err
	}
	history := make([]TaskInfo, 0, len(data))
	for _, d := range data {
		var t TaskInfo
		if err := json.Unmarshal([]byte(d), &t); err == nil {
			history = append(history, t)
		}
	}
	return history, nil
}
