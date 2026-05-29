package skedulr

import (
	"container/heap"
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

// Errors returned by the timing wheel.
var (
	// ErrWheelNotStarted is returned when Schedule is called before Start.
	ErrWheelNotStarted = errors.New("timing wheel not started")
	// ErrWheelStopped is returned when Schedule is called after Stop.
	ErrWheelStopped = errors.New("timing wheel stopped")
	// ErrNilCallback is returned when Schedule is called with a nil callback.
	ErrNilCallback = errors.New("timing wheel: nil callback")
)

// wheelEntry is an intrusive doubly-linked list node representing a single
// scheduled callback. It is intrusive (not stored inside a slice or container)
// so that Cancel can detach it from its bucket in O(1) without searching: the
// entry's bucket back-pointer + sibling pointers are all the information we
// need to unlink. The `cancelled` flag is atomic so Cancel can race against
// the tick loop without a lock.
type wheelEntry struct {
	expirationMs int64        // absolute fire time in unix milliseconds
	taskID       string       // optional — empty entries are not indexed for Cancel
	callback     func()       // user code; runs on a dispatcher goroutine, not on the tick loop
	bucket       *wheelBucket // back-pointer for O(1) cancel; nil once the entry has been flushed
	prev, next   *wheelEntry  // doubly-linked sibling pointers within the bucket list
	cancelled    int32        // atomic; the tick loop skips entries whose flag is set
}

// wheelBucket is one slot of a single wheel level. It holds the intrusive
// linked list of every entry whose expiration falls within the same `tickMs`
// quantum. A bucket is "live" (member of the delay queue's min-heap) iff its
// expiration is non-negative — flushing resets it to -1 so the same bucket
// object can be reused across many ticks without allocation.
type wheelBucket struct {
	expiration atomic.Int64 // bucket fire-time boundary; -1 means empty / not on the heap
	mu         sync.Mutex   // guards the linked list — list ops cannot race with flush
	head, tail *wheelEntry  // singly-walkable via .next; .prev is used only for O(1) detach
	heapIdx    int          // index in delayQueue.h, or -1 if not currently on the heap
}

// newWheelBucket returns an empty bucket pre-initialized to "off the heap"
// (heapIdx=-1, expiration=-1). Pre-allocating buckets once per level avoids
// per-fire allocation in the hot path.
func newWheelBucket() *wheelBucket {
	b := &wheelBucket{heapIdx: -1}
	b.expiration.Store(-1)
	return b
}

// add links e at the tail of the bucket and sets the bucket's expiration to
// bucketExp. It returns true when the expiration value actually changed —
// which is the signal that the caller must offer this bucket to the delay
// queue (either to add a fresh bucket or re-fix one whose deadline moved).
// Adding to an already-live bucket with the same expiration returns false.
func (b *wheelBucket) add(e *wheelEntry, bucketExp int64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	e.bucket = b
	if b.tail == nil {
		b.head = e
	} else {
		e.prev = b.tail
		b.tail.next = e
	}
	b.tail = e
	prev := b.expiration.Load()
	b.expiration.Store(bucketExp)
	return prev != bucketExp
}

// flush detaches every entry from the bucket, resets the bucket to its empty
// state, and returns the head of the detached list. Callers walk the list via
// .next — .prev and .bucket are cleared so a flushed entry can be safely
// re-inserted into the wheel without stale links into the old bucket.
func (b *wheelBucket) flush() *wheelEntry {
	b.mu.Lock()
	head := b.head
	b.head = nil
	b.tail = nil
	b.expiration.Store(-1)
	b.mu.Unlock()

	// Detached entries are cleaned outside the lock — by this point nothing
	// in the bucket reaches them, so no other goroutine can observe partial
	// state via the bucket.
	for e := head; e != nil; e = e.next {
		e.bucket = nil
		e.prev = nil
	}
	return head
}

// timingWheel is a single level of the hierarchy: a ring of `wheelSize`
// buckets where each bucket covers `tickMs` of wall-clock time. The level
// owns `tickMs * wheelSize` of span; anything beyond that is escalated into
// the lazily-allocated `overflow` level, which has a tick of one full
// rotation of this level. The hierarchy is unbounded in depth but bounded in
// memory — most workloads only ever allocate two or three levels.
type timingWheel struct {
	tickMs      int64                       // quantum size for this level, in milliseconds
	wheelSize   int64                       // number of buckets in the ring
	interval    int64                       // tickMs * wheelSize — total span this level covers
	currentTime atomic.Int64                // floor(now/tickMs)*tickMs; the level's "current" boundary
	buckets     []*wheelBucket              // ring of `wheelSize` buckets, indexed by virtualId % wheelSize
	overflow    atomic.Pointer[timingWheel] // next level, allocated on first overflow insert
	queue       *delayQueue                 // shared across the whole hierarchy — one heap, many levels
}

// newTimingLevel constructs one level with all buckets pre-allocated. startMs
// is the wall-clock boundary the level starts at; currentTime is floored to
// the level's quantum so the first insert lands in a predictable slot.
func newTimingLevel(tickMs, wheelSize, startMs int64, q *delayQueue) *timingWheel {
	buckets := make([]*wheelBucket, wheelSize)
	for i := range buckets {
		buckets[i] = newWheelBucket()
	}
	w := &timingWheel{
		tickMs:    tickMs,
		wheelSize: wheelSize,
		interval:  tickMs * wheelSize,
		buckets:   buckets,
		queue:     q,
	}
	w.currentTime.Store(startMs - (startMs % tickMs))
	return w
}

// add routes an entry into the correct level of the hierarchy and returns
// whether routing succeeded. A `false` return signals "already due" — the
// caller (TimingWheel.insert) handles immediate dispatch so a past-due entry
// fires without waiting for the next tick. Cancelled entries are treated as
// successfully routed (return true) to avoid spurious fires.
func (w *timingWheel) add(e *wheelEntry) bool {
	if atomic.LoadInt32(&e.cancelled) == 1 {
		return true
	}
	exp := e.expirationMs
	curr := w.currentTime.Load()
	if exp < curr+w.tickMs {
		// Entry's deadline is in the current or a past tick — caller fires it.
		return false
	}
	if exp < curr+w.interval {
		// Fits in this level. The virtualId trick — exp/tickMs — gives a
		// monotonic bucket number; modulo wheelSize wraps it onto the ring.
		virtualId := exp / w.tickMs
		bucket := w.buckets[virtualId%w.wheelSize]
		bucketExp := virtualId * w.tickMs
		if bucket.add(e, bucketExp) {
			w.queue.offer(bucket)
		}
		return true
	}
	// Beyond this level's span — escalate to the overflow level. Lazy
	// allocation via CAS so two concurrent inserters cooperate on creating
	// the next level exactly once.
	ov := w.overflow.Load()
	if ov == nil {
		fresh := newTimingLevel(w.interval, w.wheelSize, curr, w.queue)
		if w.overflow.CompareAndSwap(nil, fresh) {
			ov = fresh
		} else {
			ov = w.overflow.Load()
		}
	}
	return ov.add(e)
}

// advanceClock moves the level's currentTime forward to the floor(timeMs/tickMs)
// boundary and recursively pushes the same advance into the overflow level so
// every level shares a consistent notion of "now". Only called from the tick
// loop, so there is at most one in-flight advance at a time.
func (w *timingWheel) advanceClock(timeMs int64) {
	curr := w.currentTime.Load()
	if timeMs >= curr+w.tickMs {
		newCurr := timeMs - (timeMs % w.tickMs)
		w.currentTime.Store(newCurr)
		if ov := w.overflow.Load(); ov != nil {
			ov.advanceClock(newCurr)
		}
	}
}


// bucketHeap is a min-heap of buckets ordered by expiration. Less reads
// expiration atomically without holding the bucket's mutex — the queue's
// own lock serializes structural changes, and the worst case from a racy
// read is a temporary mis-ordering that the next heap.Fix corrects.
type bucketHeap []*wheelBucket

func (h bucketHeap) Len() int { return len(h) }
func (h bucketHeap) Less(i, j int) bool {
	return h[i].expiration.Load() < h[j].expiration.Load()
}
func (h bucketHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].heapIdx = i
	h[j].heapIdx = j
}
func (h *bucketHeap) Push(x interface{}) {
	b := x.(*wheelBucket)
	b.heapIdx = len(*h)
	*h = append(*h, b)
}
func (h *bucketHeap) Pop() interface{} {
	old := *h
	n := len(old)
	b := old[n-1]
	old[n-1] = nil
	b.heapIdx = -1
	*h = old[:n-1]
	return b
}

// delayQueue is the shared scheduling structure that drives the tick loop.
// It is a min-heap of buckets keyed by expiration plus a one-slot wake channel:
// inserts wake the poller without spinning, and poll blocks on a single
// time.Timer per wait — so the cost of N pending entries is one goroutine
// regardless of N, which is the whole point of the wheel.
type delayQueue struct {
	mu   sync.Mutex
	h    bucketHeap
	wake chan struct{}
}

// newDelayQueue creates an empty queue with a generously-sized backing slice
// so the first ~128 distinct expiration boundaries don't re-allocate.
func newDelayQueue() *delayQueue {
	return &delayQueue{
		h:    make(bucketHeap, 0, 128),
		wake: make(chan struct{}, 1),
	}
}

// offer inserts a fresh bucket onto the heap or, when the bucket is already
// present (its expiration just moved), repairs its heap position via
// heap.Fix. The non-blocking send on `wake` collapses many simultaneous
// inserts into one wakeup of the polling goroutine.
func (q *delayQueue) offer(b *wheelBucket) {
	q.mu.Lock()
	if b.heapIdx >= 0 {
		heap.Fix(&q.h, b.heapIdx)
	} else {
		heap.Push(&q.h, b)
	}
	q.mu.Unlock()
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// poll blocks the caller until the earliest queued bucket's expiration has
// arrived, then returns that bucket. Returns nil when stopCh closes, which
// is how the tick loop exits cleanly on Stop. The loop structure handles
// three races: (1) a new earlier bucket inserted while we sleep, (2) the
// timer firing exactly when stopCh closes, and (3) the heap becoming empty
// after we read its head.
func (q *delayQueue) poll(stopCh <-chan struct{}) *wheelBucket {
	for {
		q.mu.Lock()
		var got *wheelBucket
		var waitMs int64 = -1
		if q.h.Len() > 0 {
			top := q.h[0]
			exp := top.expiration.Load()
			now := time.Now().UnixMilli()
			if exp <= now {
				got = heap.Pop(&q.h).(*wheelBucket)
			} else {
				waitMs = exp - now
			}
		}
		q.mu.Unlock()

		if got != nil {
			return got
		}

		var timerC <-chan time.Time
		var timer *time.Timer
		if waitMs > 0 {
			timer = time.NewTimer(time.Duration(waitMs) * time.Millisecond)
			timerC = timer.C
		}
		select {
		case <-q.wake:
			if timer != nil {
				timer.Stop()
			}
		case <-timerC:
		case <-stopCh:
			if timer != nil {
				timer.Stop()
			}
			return nil
		}
	}
}

// TimingWheel is a hierarchical timing wheel — the public façade over the
// internal `timingWheel` levels. It schedules callbacks with O(1) insertion
// and O(1) cancellation while using a single goroutine to drive every
// pending timer, regardless of how many entries are queued. This is the
// memory and concurrency-cost replacement for naively spawning one
// time.Timer (and one goroutine) per delayed task.
//
// The wheel is purely in-memory. Skedulr's Scheduler uses it as the hot
// cache in front of a Redis ZSET that provides crash-restart durability;
// used on its own, TimingWheel forgets pending entries when Stop returns.
type TimingWheel struct {
	base   *timingWheel // bottom level — owns the smallest tick
	queue  *delayQueue  // shared across every level of the hierarchy
	tickMs int64        // bottom-level tick, in milliseconds
	size   int64        // bucket count at every level

	started int32         // atomic; tickLoop runs iff this is 1
	stopped int32         // atomic; once 1, Schedule rejects new inserts
	stop    chan struct{} // closed by Stop to release poll
	wg      sync.WaitGroup

	mu      sync.Mutex
	entries map[string]*wheelEntry // taskID → current scheduled entry, for O(1) Cancel
}

// NewTimingWheel constructs a wheel with the given bottom-level tick quantum
// and bucket count per level. Higher levels are allocated lazily as needed
// to cover delays beyond `tick * wheelSize`, so the cost of supporting very
// long delays is paid only when something actually schedules one.
//
// Default for Skedulr is NewTimingWheel(10*time.Millisecond, 256) — that
// gives a 2.56s bottom-level span and a level-1 span of ~10.9 minutes,
// which covers the overwhelming majority of retry and ScheduleOnce delays
// without ever touching the overflow path. tick is clamped to >= 1ms and
// wheelSize to >= 2 so silly inputs don't break the math.
func NewTimingWheel(tick time.Duration, wheelSize int) *TimingWheel {
	if tick < time.Millisecond {
		tick = time.Millisecond
	}
	if wheelSize < 2 {
		wheelSize = 2
	}
	tickMs := int64(tick / time.Millisecond)
	size := int64(wheelSize)
	q := newDelayQueue()
	return &TimingWheel{
		base:    newTimingLevel(tickMs, size, time.Now().UnixMilli(), q),
		queue:   q,
		tickMs:  tickMs,
		size:    size,
		stop:    make(chan struct{}),
		entries: make(map[string]*wheelEntry),
	}
}

// Start launches the single tick-loop goroutine. Idempotent: a wheel that
// has already been started is a no-op. Schedule will reject inserts until
// Start has been called.
func (tw *TimingWheel) Start() {
	if !atomic.CompareAndSwapInt32(&tw.started, 0, 1) {
		return
	}
	tw.wg.Add(1)
	go tw.tickLoop()
}

// Stop signals the tick loop to exit and blocks until it does. Idempotent.
// Pending entries that have not yet fired are dropped — callers that need
// durability must persist alongside Schedule (see Scheduler's Redis ZSET
// backstop) and re-hydrate after restart.
func (tw *TimingWheel) Stop() {
	if !atomic.CompareAndSwapInt32(&tw.stopped, 0, 1) {
		return
	}
	close(tw.stop)
	tw.wg.Wait()
}

// Schedule arms cb to fire at the wall-clock instant fireAt. If taskID is
// non-empty the entry is indexed for O(1) Cancel(taskID) and scheduling a
// second entry with the same ID atomically cancels the first — this is the
// property recurring tasks rely on for re-arming. fireAt in the past does
// not fail; the entry is dispatched immediately on a fresh goroutine, so
// the tick loop is never blocked by user code regardless of how it lands.
func (tw *TimingWheel) Schedule(taskID string, fireAt time.Time, cb func()) error {
	if atomic.LoadInt32(&tw.started) == 0 {
		return ErrWheelNotStarted
	}
	if atomic.LoadInt32(&tw.stopped) == 1 {
		return ErrWheelStopped
	}
	if cb == nil {
		return ErrNilCallback
	}

	e := &wheelEntry{
		expirationMs: fireAt.UnixMilli(),
		taskID:       taskID,
		callback:     cb,
	}
	if taskID != "" {
		tw.mu.Lock()
		if old, ok := tw.entries[taskID]; ok {
			atomic.StoreInt32(&old.cancelled, 1)
		}
		tw.entries[taskID] = e
		tw.mu.Unlock()
	}
	tw.insert(e)
	return nil
}

// Cancel marks the entry with the given taskID as cancelled. The callback
// will not fire even if its bucket has already been flushed and the entry
// is sitting in the dispatch pipeline — every fire path re-checks the flag.
// Safe to call from any goroutine; safe to call for unknown IDs (no-op).
func (tw *TimingWheel) Cancel(taskID string) {
	if taskID == "" {
		return
	}
	tw.mu.Lock()
	e, ok := tw.entries[taskID]
	if ok {
		delete(tw.entries, taskID)
	}
	tw.mu.Unlock()
	if ok {
		atomic.StoreInt32(&e.cancelled, 1)
	}
}

// insert routes a freshly built entry through the bottom level of the
// hierarchy. The "already due" branch (add returned false) dispatches the
// callback on a fresh goroutine — running it inline would block the tick
// loop on arbitrary user code, defeating the whole single-goroutine model.
// The cancelled check before dispatch closes the obvious race between
// Cancel and a past-due fire.
func (tw *TimingWheel) insert(e *wheelEntry) {
	if !tw.base.add(e) {
		if atomic.LoadInt32(&e.cancelled) == 0 {
			tw.removeFromIndex(e)
			go e.callback()
		}
	}
}

// removeFromIndex unlinks an entry from the by-ID index, but only if the
// currently-indexed entry for that ID is the same one — Schedule may have
// already replaced it with a fresh entry (recurring tasks do this on every
// fire), in which case we must not delete the newer entry by mistake.
func (tw *TimingWheel) removeFromIndex(e *wheelEntry) {
	if e.taskID == "" {
		return
	}
	tw.mu.Lock()
	if cur, ok := tw.entries[e.taskID]; ok && cur == e {
		delete(tw.entries, e.taskID)
	}
	tw.mu.Unlock()
}

// tickLoop is the entire scheduling thread of the wheel — one goroutine for
// the whole instance, regardless of how many entries are pending. It blocks
// on the delay queue until the soonest bucket is due, advances every
// level's clock, then drains the bucket: bottom-level entries fire (via
// insert's "already due" branch) and upper-level entries demote into the
// now-shorter level until they reach the bottom. Exits cleanly when Stop
// closes the stop channel and delayQueue.poll returns nil.
func (tw *TimingWheel) tickLoop() {
	defer tw.wg.Done()
	for {
		bucket := tw.queue.poll(tw.stop)
		if bucket == nil {
			return
		}

		exp := bucket.expiration.Load()
		if exp >= 0 {
			tw.base.advanceClock(exp)
		}
		for e := bucket.flush(); e != nil; {
			next := e.next
			e.next = nil
			tw.insert(e)
			e = next
		}
	}
}
