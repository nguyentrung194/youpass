// Command server runs the share-link API as a standalone service.
//
// Inside YouPass it would be mounted into the existing Go API instead, with
// the real JWT middleware and submission service in place of the demo stubs
// below.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/youpass/sharelink/internal/sharelink"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(log); err != nil {
		log.Error("server stopped", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := pgxpool.New(ctx, env("DATABASE_URL", "postgres://youpass:youpass@localhost:5432/youpass"))
	if err != nil {
		return err
	}
	defer pool.Close()

	ropts, err := redis.ParseURL(env("REDIS_URL", "redis://localhost:6379/0"))
	if err != nil {
		return err
	}
	rdb := redis.NewClient(ropts)
	defer rdb.Close()

	store := sharelink.NewPostgresStore(pool)
	subs := demoSubmissions{}
	cache := sharelink.NewCache(rdb, sharelink.DefaultCacheConfig(), log)
	svc := sharelink.NewService(store, subs, cache, log)
	views := sharelink.NewViewCounter(rdb, store, sharelink.DefaultViewCounterConfig(), log)

	mux := http.NewServeMux()
	sharelink.NewHandler(svc, views, subs, demoUser, env("PUBLIC_BASE_URL", "https://youpass.vn"), log).Register(mux)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	srv := &http.Server{
		Addr:              env("HTTP_ADDR", ":8080"),
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	bg, cancelBg := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); cache.Listen(bg) }()
	go func() { defer wg.Done(); views.Run(bg) }()

	errCh := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", srv.Addr)
		errCh <- srv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		cancelBg()
		wg.Wait()
		return err
	case <-ctx.Done():
	}

	// Stop taking requests first, then stop the background workers, so the
	// final view flush sees every enqueued view.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	err = srv.Shutdown(shutdownCtx)
	cancelBg()
	wg.Wait()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// demoUser trusts an X-User-ID header. DEMO ONLY: YouPass would read the
// user id from its verified access token.
func demoUser(r *http.Request) int64 {
	id, _ := strconv.ParseInt(r.Header.Get("X-User-ID"), 10, 64)
	return id
}

// demoSubmissions stands in for YouPass's submission service: submission N
// belongs to user N % 1000 (so /submissions/1001 belongs to user 1).
type demoSubmissions struct{}

func (demoSubmissions) OwnerOf(_ context.Context, submissionID int64) (int64, error) {
	if submissionID <= 0 {
		return 0, sharelink.ErrSubmissionNotFound
	}
	return submissionID % 1000, nil
}

func (demoSubmissions) PublicView(_ context.Context, submissionID int64) (any, error) {
	return map[string]any{
		"id":          submissionID,
		"test":        "IELTS Writing Task 2",
		"band_score":  6.5,
		"feedback":    "Coherence and cohesion are good; work on lexical resource.",
		"author_name": "Học viên YouPass",
	}, nil
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
