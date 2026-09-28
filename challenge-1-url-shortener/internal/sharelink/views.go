package sharelink

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
)

// All flush keys share the {sharelink:views} hash tag so the Lua script below
// stays valid on Redis Cluster (every key it touches lives in one slot).
const (
	viewsPendingKey   = "{sharelink:views}:pending"
	viewsInflightKey  = "{sharelink:views}:inflight"
	viewsFlushingPref = "{sharelink:views}:flushing:"
	viewsSeenPrefix   = "sharelink:seen:"
)

type ViewCounterConfig struct {
	// DedupWindow: repeat views by the same viewer inside it count once, so
	// pressing F5 does not inflate the counter.
	DedupWindow   time.Duration
	FlushInterval time.Duration
	QueueSize     int
	Workers       int
}

func DefaultViewCounterConfig() ViewCounterConfig {
	return ViewCounterConfig{
		DedupWindow:   30 * time.Minute,
		FlushInterval: 10 * time.Second,
		QueueSize:     10_000,
		Workers:       4,
	}
}

type viewEvent struct{ code, viewer string }

// ViewCounter keeps view counting off the request path and away from
// Postgres:
//
//	request → Enqueue (non-blocking) → workers: SET NX dedup + HINCRBY pending
//	every FlushInterval: pending → inflight batch → one UPDATE per batch
//
// A shared link can get thousands of views a minute, which becomes one row
// update per link per flush instead of one per view.
type ViewCounter struct {
	rdb   redis.UniversalClient
	store Store
	cfg   ViewCounterConfig
	log   *slog.Logger
	queue chan viewEvent

	Dropped atomic.Int64 // events dropped because the queue was full (export as a metric)
}

func NewViewCounter(rdb redis.UniversalClient, store Store, cfg ViewCounterConfig, log *slog.Logger) *ViewCounter {
	return &ViewCounter{rdb: rdb, store: store, cfg: cfg, log: log, queue: make(chan viewEvent, cfg.QueueSize)}
}

// Enqueue never blocks the request. Under extreme load it drops events
// instead of adding latency, because a view counter is approximate by nature.
func (v *ViewCounter) Enqueue(code, viewer string) bool {
	select {
	case v.queue <- viewEvent{code, viewer}:
		return true
	default:
		v.Dropped.Add(1)
		return false
	}
}

// Record counts one view unless the viewer already viewed within DedupWindow.
func (v *ViewCounter) Record(ctx context.Context, code, viewer string) (bool, error) {
	fresh, err := v.rdb.SetNX(ctx, viewsSeenPrefix+code+":"+viewer, 1, v.cfg.DedupWindow).Result()
	if err != nil || !fresh {
		return false, err
	}
	// If this fails after SET NX succeeded we lose one view, which is the
	// safe direction for an approximate counter.
	if err := v.rdb.HIncrBy(ctx, viewsPendingKey, code, 1).Err(); err != nil {
		return false, err
	}
	return true, nil
}

// Pending returns views counted in Redis but not yet flushed, so owners see
// near-realtime totals (database count + pending).
func (v *ViewCounter) Pending(ctx context.Context, code string) int64 {
	n, err := v.rdb.HGet(ctx, viewsPendingKey, code).Int64()
	if err != nil {
		return 0
	}
	return n
}

// takePending atomically moves the pending hash to a new batch key and
// records the batch as in flight. New views keep landing in a fresh pending
// hash, so flushing never blocks counting.
var takePending = redis.NewScript(`
if redis.call('EXISTS', KEYS[1]) == 0 then return 0 end
redis.call('RENAME', KEYS[1], KEYS[2])
redis.call('SADD', KEYS[3], ARGV[1])
return 1
`)

// Flush moves pending views into Postgres. Any instance may run it
// concurrently:
//   - takePending is atomic, so each view lands in exactly one batch;
//   - every in-flight batch (including ones left by a crashed instance) is
//     retried until it is applied and removed;
//   - ApplyViewDeltas is idempotent per batch id, so a retry never
//     double-counts.
func (v *ViewCounter) Flush(ctx context.Context) error {
	batchID, err := newBatchID()
	if err != nil {
		return err
	}
	err = takePending.Run(ctx, v.rdb,
		[]string{viewsPendingKey, viewsFlushingPref + batchID, viewsInflightKey}, batchID).Err()
	if err != nil {
		return fmt.Errorf("take pending views: %w", err)
	}

	batches, err := v.rdb.SMembers(ctx, viewsInflightKey).Result()
	if err != nil {
		return fmt.Errorf("list in-flight view batches: %w", err)
	}
	for _, id := range batches {
		if err := v.applyBatch(ctx, id); err != nil {
			return err // retried on the next tick
		}
	}
	return nil
}

func (v *ViewCounter) applyBatch(ctx context.Context, batchID string) error {
	key := viewsFlushingPref + batchID
	raw, err := v.rdb.HGetAll(ctx, key).Result()
	if err != nil {
		return fmt.Errorf("read view batch %s: %w", batchID, err)
	}
	deltas := make(map[string]int64, len(raw))
	for code, s := range raw {
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil || n <= 0 {
			continue
		}
		deltas[code] = n
	}
	if len(deltas) > 0 {
		if err := v.store.ApplyViewDeltas(ctx, batchID, deltas); err != nil {
			return fmt.Errorf("apply view batch %s: %w", batchID, err)
		}
	}
	// Only forget the batch after the database has committed it.
	pipe := v.rdb.TxPipeline()
	pipe.Del(ctx, key)
	pipe.SRem(ctx, viewsInflightKey, batchID)
	_, err = pipe.Exec(ctx)
	return err
}

// Run starts the record workers and the periodic flusher. When ctx is done it
// drains the queue and flushes once more, so a deploy does not lose views.
func (v *ViewCounter) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for range v.cfg.Workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v.work(ctx)
		}()
	}

	ticker := time.NewTicker(v.cfg.FlushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if err := v.Flush(ctx); err != nil {
				v.log.WarnContext(ctx, "sharelink: view flush failed", "err", err)
			}
		case <-ctx.Done():
			wg.Wait()
			final, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := v.Flush(final); err != nil {
				v.log.Warn("sharelink: final view flush failed", "err", err)
			}
			return
		}
	}
}

func (v *ViewCounter) work(ctx context.Context) {
	record := func(ev viewEvent) {
		rctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if _, err := v.Record(rctx, ev.code, ev.viewer); err != nil {
			v.log.Warn("sharelink: record view failed", "code", ev.code, "err", err)
		}
	}
	for {
		select {
		case ev := <-v.queue:
			record(ev)
		case <-ctx.Done():
			for {
				select {
				case ev := <-v.queue:
					record(ev)
				default:
					return
				}
			}
		}
	}
}

func newBatchID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return strconv.FormatInt(time.Now().UnixMilli(), 10) + "-" + hex.EncodeToString(b), nil
}
