package sharelink

import (
	"context"
	"encoding/json"
	"log/slog"
	"math/rand/v2"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	redisLinkPrefix   = "sharelink:link:"
	invalidateChannel = "sharelink:invalidate"
)

type CacheConfig struct {
	// LocalTTL bounds how long an instance can serve a stale entry if an
	// invalidation message is lost. It is the worst-case revocation delay.
	LocalTTL        time.Duration
	LocalMaxEntries int
	RedisTTL        time.Duration
	NegativeTTL     time.Duration
}

func DefaultCacheConfig() CacheConfig {
	return CacheConfig{
		LocalTTL:        5 * time.Second,
		LocalMaxEntries: 100_000,
		RedisTTL:        10 * time.Minute,
		NegativeTTL:     30 * time.Second,
	}
}

// cachedLink is the minimum state needed to answer a public request.
// View counts are deliberately not cached; they change on every view.
type cachedLink struct {
	Code         string     `json:"c"`
	SubmissionID int64      `json:"s"`
	OwnerID      int64      `json:"o"`
	Status       Status     `json:"st"`
	ExpiresAt    *time.Time `json:"e,omitempty"`
	Version      int64      `json:"v"`
}

func toCached(l *Link) *cachedLink {
	return &cachedLink{
		Code: l.Code, SubmissionID: l.SubmissionID, OwnerID: l.OwnerID,
		Status: l.Status, ExpiresAt: l.ExpiresAt, Version: l.Version,
	}
}

// missingEntry has version 0, so the first real write (version 1) always wins.
func missingEntry(code string) *cachedLink {
	return &cachedLink{Code: code, Status: statusMissing}
}

func (e *cachedLink) accessible(now time.Time) bool {
	return e.Status == StatusActive && (e.ExpiresAt == nil || now.Before(*e.ExpiresAt))
}

// setIfNewer writes "<version>|<json>" unless Redis already holds a newer
// version. Without it, a reader that loaded an "active" row just before the
// owner disabled the link could put that stale row back into Redis after the
// owner's write, and the link would stay public for the whole Redis TTL.
var setIfNewer = redis.NewScript(`
local cur = redis.call('GET', KEYS[1])
if cur then
  local v = tonumber(string.match(cur, '^(%d+)|'))
  if v and v > tonumber(ARGV[2]) then return 0 end
end
redis.call('SET', KEYS[1], ARGV[1], 'PX', ARGV[3])
return 1
`)

// Cache is a two-tier cache: L1 in process, L2 in Redis. Postgres stays the
// source of truth, and every failure here degrades to a miss, never an error.
type Cache struct {
	rdb   redis.UniversalClient
	local *localCache
	cfg   CacheConfig
	log   *slog.Logger
}

func NewCache(rdb redis.UniversalClient, cfg CacheConfig, log *slog.Logger) *Cache {
	return &Cache{rdb: rdb, local: newLocalCache(cfg.LocalMaxEntries, cfg.LocalTTL), cfg: cfg, log: log}
}

func (c *Cache) Get(ctx context.Context, code string) (*cachedLink, bool) {
	if e, ok := c.local.get(code); ok {
		return e, true
	}
	raw, err := c.rdb.Get(ctx, redisLinkPrefix+code).Result()
	if err != nil {
		if err != redis.Nil {
			c.log.WarnContext(ctx, "sharelink: redis get failed, falling back to db", "err", err)
		}
		return nil, false
	}
	e, ok := decodeEntry(raw)
	if !ok {
		return nil, false
	}
	c.local.set(code, e)
	return e, true
}

// Fill stores an entry loaded from the database.
func (c *Cache) Fill(ctx context.Context, e *cachedLink) {
	c.writeRedis(ctx, e)
	c.local.set(e.Code, e)
}

// Publish writes a state change through to Redis and evicts the entry from
// L1 on every instance. It returns an error when Redis could not be updated,
// because the caller then cannot promise fast revocation.
func (c *Cache) Publish(ctx context.Context, e *cachedLink) error {
	c.local.delete(e.Code)
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		if err = c.writeRedisErr(ctx, e); err == nil {
			break
		}
		time.Sleep(time.Duration(attempt+1) * 50 * time.Millisecond)
	}
	if err != nil {
		return err
	}
	if perr := c.rdb.Publish(ctx, invalidateChannel, e.Code).Err(); perr != nil {
		// Other instances will pick the change up within LocalTTL.
		c.log.WarnContext(ctx, "sharelink: publish invalidation failed", "code", e.Code, "err", perr)
	}
	return nil
}

// Listen evicts L1 entries changed by other instances. It blocks until ctx is
// done. go-redis resubscribes after a disconnect; anything missed meanwhile
// expires within LocalTTL.
func (c *Cache) Listen(ctx context.Context) {
	sub := c.rdb.Subscribe(ctx, invalidateChannel)
	defer sub.Close()
	ch := sub.Channel()
	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-ch:
			if !ok {
				return
			}
			c.local.delete(msg.Payload)
		}
	}
}

func (c *Cache) writeRedis(ctx context.Context, e *cachedLink) {
	if err := c.writeRedisErr(ctx, e); err != nil {
		c.log.WarnContext(ctx, "sharelink: redis fill failed", "code", e.Code, "err", err)
	}
}

func (c *Cache) writeRedisErr(ctx context.Context, e *cachedLink) error {
	body, err := json.Marshal(e)
	if err != nil {
		return err
	}
	ttl := c.cfg.NegativeTTL
	if e.Status != statusMissing {
		// Up to 10% jitter so entries filled together don't all expire together.
		ttl = c.cfg.RedisTTL + time.Duration(rand.Int64N(int64(c.cfg.RedisTTL/10)+1))
	}
	val := strconv.FormatInt(e.Version, 10) + "|" + string(body)
	return setIfNewer.Run(ctx, c.rdb, []string{redisLinkPrefix + e.Code},
		val, e.Version, ttl.Milliseconds()).Err()
}

func decodeEntry(raw string) (*cachedLink, bool) {
	_, body, ok := strings.Cut(raw, "|")
	if !ok {
		return nil, false
	}
	var e cachedLink
	if err := json.Unmarshal([]byte(body), &e); err != nil {
		return nil, false
	}
	return &e, true
}

// localCache is a small TTL map. It absorbs "hot" links, for example one
// shared to a Facebook group with thousands of members, so they never reach
// Redis. In production a library such as otter or ristretto would add proper
// LRU/LFU eviction; the idea is the same.
type localCache struct {
	mu  sync.RWMutex
	m   map[string]localEntry
	max int
	ttl time.Duration
}

type localEntry struct {
	e   *cachedLink
	exp time.Time
}

func newLocalCache(max int, ttl time.Duration) *localCache {
	return &localCache{m: make(map[string]localEntry), max: max, ttl: ttl}
}

func (c *localCache) get(code string) (*cachedLink, bool) {
	c.mu.RLock()
	le, ok := c.m[code]
	c.mu.RUnlock()
	if !ok || time.Now().After(le.exp) {
		return nil, false
	}
	return le.e, true
}

func (c *localCache) set(code string, e *cachedLink) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.m) >= c.max {
		// Go randomises map iteration order, so this evicts an arbitrary entry.
		for k := range c.m {
			delete(c.m, k)
			break
		}
	}
	c.m[code] = localEntry{e: e, exp: time.Now().Add(c.ttl)}
}

func (c *localCache) delete(code string) {
	c.mu.Lock()
	delete(c.m, code)
	c.mu.Unlock()
}
