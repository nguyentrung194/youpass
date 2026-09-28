package sharelink

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

type fakeSubs struct{}

// Submission N belongs to user N % 1000.
func (fakeSubs) OwnerOf(_ context.Context, id int64) (int64, error) {
	if id <= 0 {
		return 0, ErrSubmissionNotFound
	}
	return id % 1000, nil
}

func (fakeSubs) PublicView(_ context.Context, id int64) (any, error) {
	return map[string]any{"id": id, "band_score": 7.0}, nil
}

type testEnv struct {
	mr    *miniredis.Miniredis
	rdb   *redis.Client
	store *MemoryStore
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { rdb.Close() })
	return &testEnv{mr: mr, rdb: rdb, store: NewMemoryStore()}
}

// instance simulates one API pod: its own L1 cache and singleflight,
// sharing Redis and the database with the other pods.
func (e *testEnv) instance(t *testing.T) *Service {
	t.Helper()
	cache := NewCache(e.rdb, DefaultCacheConfig(), discard)
	subscribers := e.mr.PubSubNumSub(invalidateChannel)[invalidateChannel]
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { cache.Listen(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	// Give the subscription time to register before the test publishes.
	waitFor(t, func() bool { return e.mr.PubSubNumSub(invalidateChannel)[invalidateChannel] > subscribers })
	return NewService(e.store, fakeSubs{}, cache, discard)
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met within 2s")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestNewCodeIsValidAndUnique(t *testing.T) {
	seen := make(map[string]bool)
	for range 50_000 {
		c, err := NewCode()
		if err != nil {
			t.Fatal(err)
		}
		if !ValidCode(c) {
			t.Fatalf("invalid code %q", c)
		}
		if seen[c] {
			t.Fatalf("duplicate code %q", c)
		}
		seen[c] = true
	}
}

func TestValidCode(t *testing.T) {
	for code, want := range map[string]bool{
		"aB3dE6gH": true, "": false, "short": false, "aB3dE6gH9": false,
		"aB3dE6g-": false, "aB3dE6g/": false, "aB3dÉ6gH": false,
	} {
		if got := ValidCode(code); got != want {
			t.Errorf("ValidCode(%q) = %v, want %v", code, got, want)
		}
	}
}

func TestCreateIsIdempotentAndChecksOwnership(t *testing.T) {
	env := newTestEnv(t)
	svc := env.instance(t)
	ctx := context.Background()

	l1, created, err := svc.Create(ctx, 1, 1001, CreateOptions{})
	if err != nil || !created {
		t.Fatalf("first create: created=%v err=%v", created, err)
	}
	l2, created, err := svc.Create(ctx, 1, 1001, CreateOptions{})
	if err != nil || created || l2.Code != l1.Code {
		t.Fatalf("second create must return the same link: %v %v %v", l2, created, err)
	}
	if _, _, err := svc.Create(ctx, 2, 1001, CreateOptions{}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("non-owner create: got %v, want ErrForbidden", err)
	}
	if _, err := svc.SetEnabled(ctx, 2, l1.Code, false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("non-owner toggle: got %v, want ErrNotFound", err)
	}
}

func TestDisableRevokesOnEveryInstance(t *testing.T) {
	env := newTestEnv(t)
	a, b := env.instance(t), env.instance(t)
	ctx := context.Background()

	link, _, _ := a.Create(ctx, 1, 1001, CreateOptions{})
	// Warm pod B's L1 cache and Redis.
	for range 3 {
		if _, err := b.Resolve(ctx, link.Code); err != nil {
			t.Fatalf("resolve before disable: %v", err)
		}
	}

	if _, err := a.SetEnabled(ctx, 1, link.Code, false); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Resolve(ctx, link.Code); !errors.Is(err, ErrNotFound) {
		t.Fatalf("pod A must see the revocation immediately, got %v", err)
	}
	// Pod B drops its L1 entry when the invalidation arrives, well within LocalTTL.
	waitFor(t, func() bool {
		_, err := b.Resolve(ctx, link.Code)
		return errors.Is(err, ErrNotFound)
	})

	// Re-enabling keeps the same URL working again.
	if _, err := a.SetEnabled(ctx, 1, link.Code, true); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		_, err := b.Resolve(ctx, link.Code)
		return err == nil
	})
}

func TestStaleFillCannotOverwriteNewerState(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	cache := NewCache(env.rdb, DefaultCacheConfig(), discard)

	// Owner disables (version 2) while a slow reader still holds the
	// version 1 "active" row it loaded earlier.
	disabled := &cachedLink{Code: "aaaaaaaa", Status: StatusDisabled, Version: 2}
	stale := &cachedLink{Code: "aaaaaaaa", Status: StatusActive, Version: 1}
	if err := cache.Publish(ctx, disabled); err != nil {
		t.Fatal(err)
	}
	cache.Fill(ctx, stale)

	fresh := NewCache(env.rdb, DefaultCacheConfig(), discard) // another pod, empty L1
	got, ok := fresh.Get(ctx, "aaaaaaaa")
	if !ok || got.Status != StatusDisabled || got.Version != 2 {
		t.Fatalf("stale fill overwrote newer state: %+v", got)
	}
}

func TestDeleteIsPermanent(t *testing.T) {
	env := newTestEnv(t)
	svc := env.instance(t)
	ctx := context.Background()

	link, _, _ := svc.Create(ctx, 1, 1001, CreateOptions{})
	if err := svc.Delete(ctx, 1, link.Code); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Resolve(ctx, link.Code); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted link resolved: %v", err)
	}
	if _, err := svc.SetEnabled(ctx, 1, link.Code, true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted link was re-enabled: %v", err)
	}
	again, created, err := svc.Create(ctx, 1, 1001, CreateOptions{})
	if err != nil || !created || again.Code == link.Code {
		t.Fatalf("sharing again must issue a new code: %+v created=%v err=%v", again, created, err)
	}
}

func TestExpiredLinkIsNotAccessible(t *testing.T) {
	env := newTestEnv(t)
	svc := env.instance(t)
	ctx := context.Background()

	exp := time.Now().Add(time.Hour)
	link, _, _ := svc.Create(ctx, 1, 1001, CreateOptions{ExpiresAt: &exp})
	if _, err := svc.Resolve(ctx, link.Code); err != nil {
		t.Fatalf("before expiry: %v", err)
	}
	svc.now = func() time.Time { return exp.Add(time.Second) }
	if _, err := svc.Resolve(ctx, link.Code); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after expiry: got %v, want ErrNotFound", err)
	}
}

func TestConcurrentMissesHitDatabaseOnce(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	link, _, _ := env.instance(t).Create(ctx, 1, 1001, CreateOptions{})

	env.mr.FlushAll() // cold Redis
	cold := env.instance(t)
	env.store.FindByCodeCalls.Store(0)
	env.store.FindByCodeDelay = 50 * time.Millisecond

	var wg sync.WaitGroup
	for range 200 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := cold.Resolve(ctx, link.Code); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if n := env.store.FindByCodeCalls.Load(); n != 1 {
		t.Fatalf("200 concurrent misses caused %d database reads, want 1", n)
	}
}

func TestUnknownCodesAreNegativelyCached(t *testing.T) {
	env := newTestEnv(t)
	svc := env.instance(t)
	ctx := context.Background()

	for range 5 {
		if _, err := svc.Resolve(ctx, "zzzzzzzz"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("got %v", err)
		}
	}
	if _, err := svc.Resolve(ctx, "bad-code"); !errors.Is(err, ErrNotFound) {
		t.Fatal("malformed code must be rejected")
	}
	if n := env.store.FindByCodeCalls.Load(); n != 1 {
		t.Fatalf("database reads = %d, want 1 (one miss, then negative cache)", n)
	}
}

func TestResolveSurvivesRedisOutage(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	link, _, _ := env.instance(t).Create(ctx, 1, 1001, CreateOptions{})

	cold := env.instance(t)
	env.mr.Close()
	if _, err := cold.Resolve(ctx, link.Code); err != nil {
		t.Fatalf("resolve must fall back to the database when Redis is down: %v", err)
	}
}

func TestViewsAreDedupedAndFlushedOnce(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	link, _, _ := env.instance(t).Create(ctx, 1, 1001, CreateOptions{})
	vc := NewViewCounter(env.rdb, env.store, DefaultViewCounterConfig(), discard)

	for _, viewer := range []string{"alice", "alice", "bob", "alice"} {
		if _, err := vc.Record(ctx, link.Code, viewer); err != nil {
			t.Fatal(err)
		}
	}
	if p := vc.Pending(ctx, link.Code); p != 2 {
		t.Fatalf("pending = %d, want 2 (alice deduped)", p)
	}

	if err := vc.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if err := vc.Flush(ctx); err != nil { // nothing new; must not double-count
		t.Fatal(err)
	}
	stored, _ := env.store.FindByCode(ctx, link.Code)
	if stored.ViewCount != 2 || vc.Pending(ctx, link.Code) != 0 {
		t.Fatalf("view_count = %d pending = %d, want 2 and 0", stored.ViewCount, vc.Pending(ctx, link.Code))
	}

	// After the dedup window a returning viewer counts again.
	env.mr.FastForward(31 * time.Minute)
	if counted, _ := vc.Record(ctx, link.Code, "alice"); !counted {
		t.Fatal("returning viewer after the window must be counted")
	}
}

func TestFlushRecoversCrashedBatchWithoutDoubleCounting(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	link, _, _ := env.instance(t).Create(ctx, 1, 1001, CreateOptions{})
	vc := NewViewCounter(env.rdb, env.store, DefaultViewCounterConfig(), discard)

	for _, viewer := range []string{"a", "b", "c"} {
		vc.Record(ctx, link.Code, viewer)
	}
	// Simulate a pod that took the batch and committed it to the database,
	// then crashed before cleaning up Redis.
	takePending.Run(ctx, env.rdb, []string{viewsPendingKey, viewsFlushingPref + "crashed", viewsInflightKey}, "crashed")
	env.store.ApplyViewDeltas(ctx, "crashed", map[string]int64{link.Code: 3})

	vc.Record(ctx, link.Code, "d")
	if err := vc.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	stored, _ := env.store.FindByCode(ctx, link.Code)
	if stored.ViewCount != 4 {
		t.Fatalf("view_count = %d, want 4 (3 recovered once + 1 new)", stored.ViewCount)
	}
	if n, _ := env.rdb.SCard(ctx, viewsInflightKey).Result(); n != 0 {
		t.Fatalf("%d batches still in flight", n)
	}
}

// drainViews records queued events synchronously, standing in for the
// background workers.
func drainViews(t *testing.T, vc *ViewCounter) {
	t.Helper()
	for {
		select {
		case ev := <-vc.queue:
			if _, err := vc.Record(context.Background(), ev.code, ev.viewer); err != nil {
				t.Fatal(err)
			}
		default:
			return
		}
	}
}

func TestHTTPFlow(t *testing.T) {
	env := newTestEnv(t)
	svc := env.instance(t)
	vc := NewViewCounter(env.rdb, env.store, DefaultViewCounterConfig(), discard)
	user := func(r *http.Request) int64 {
		switch r.Header.Get("X-User-ID") {
		case "1":
			return 1
		case "2":
			return 2
		}
		return 0
	}
	mux := http.NewServeMux()
	NewHandler(svc, vc, fakeSubs{}, user, "https://youpass.vn", discard).Register(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	do := func(method, path, userID, body string, hdr map[string]string) *http.Response {
		req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		if userID != "" {
			req.Header.Set("X-User-ID", userID)
		}
		req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh)")
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { res.Body.Close() })
		return res
	}
	expect := func(res *http.Response, status int) {
		t.Helper()
		if res.StatusCode != status {
			b, _ := io.ReadAll(res.Body)
			t.Fatalf("%s %s: status %d, want %d (%s)", res.Request.Method, res.Request.URL.Path, res.StatusCode, status, b)
		}
	}

	expect(do("POST", "/api/v1/submissions/1001/share", "", "", nil), http.StatusUnauthorized)
	expect(do("POST", "/api/v1/submissions/1001/share", "2", "", nil), http.StatusForbidden)
	expect(do("POST", "/api/v1/submissions/1001/share", "1", "", nil), http.StatusCreated)
	expect(do("POST", "/api/v1/submissions/1001/share", "1", "", nil), http.StatusOK)

	link, _ := env.store.FindLiveBySubmission(context.Background(), 1001)
	public := "/api/v1/s/" + link.Code

	res := do("GET", public, "", "", nil)
	expect(res, http.StatusOK)
	if res.Header.Get("Cache-Control") != "no-store" || res.Header.Get("X-Robots-Tag") == "" {
		t.Fatalf("public response must be uncacheable and noindex: %v", res.Header)
	}

	// Views: bots, prefetches and the owner are ignored; an anonymous
	// visitor counts once per dedup window thanks to the viewer cookie.
	expect(do("POST", public+"/views", "", "", map[string]string{"User-Agent": "facebookexternalhit/1.1"}), http.StatusAccepted)
	expect(do("POST", public+"/views", "", "", map[string]string{"Sec-Purpose": "prefetch"}), http.StatusAccepted)
	expect(do("POST", public+"/views", "1", "", nil), http.StatusAccepted)
	first := do("POST", public+"/views", "", "", nil)
	expect(first, http.StatusAccepted)
	var cookie *http.Cookie
	for _, c := range first.Cookies() {
		if c.Name == viewerCookie {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("anonymous visitor did not get a viewer cookie")
	}
	expect(do("POST", public+"/views", "", "", map[string]string{"Cookie": viewerCookie + "=" + cookie.Value}), http.StatusAccepted)
	expect(do("POST", public+"/views", "2", "", nil), http.StatusAccepted)
	drainViews(t, vc)
	if p := vc.Pending(context.Background(), link.Code); p != 2 {
		t.Fatalf("pending views = %d, want 2 (anonymous once + user 2)", p)
	}

	// Turning sharing off, then deleting.
	expect(do("PATCH", "/api/v1/shares/"+link.Code, "1", `{"enabled":false}`, nil), http.StatusOK)
	expect(do("GET", public, "", "", nil), http.StatusNotFound)
	expect(do("PATCH", "/api/v1/shares/"+link.Code, "1", `{"enabled":true}`, nil), http.StatusOK)
	expect(do("GET", public, "", "", nil), http.StatusOK)
	expect(do("DELETE", "/api/v1/shares/"+link.Code, "2", "", nil), http.StatusNotFound)
	expect(do("DELETE", "/api/v1/shares/"+link.Code, "1", "", nil), http.StatusNoContent)
	expect(do("GET", public, "", "", nil), http.StatusNotFound)
	expect(do("GET", "/api/v1/s/not-a-code", "", "", nil), http.StatusNotFound)
}
