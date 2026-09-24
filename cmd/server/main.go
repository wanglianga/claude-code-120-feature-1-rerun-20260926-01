package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"seafoodtrace/internal/api"
	"seafoodtrace/internal/db"

	"github.com/redis/go-redis/v9"
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	dsn := "postgres://" + env("PGUSER", "seafood") + ":" + env("PGPASSWORD", "seafood_pw") +
		"@" + env("PGHOST", "db") + ":" + env("PGPORT", "5432") + "/" + env("PGNAME", "seafood") +
		"?sslmode=disable"

	pool, err := db.Connect(dsn)
	if err != nil {
		log.Fatalf("connect postgres: %v", err)
	}
	defer pool.Close()

	if err := db.Migrate(context.Background(), pool); err != nil {
		log.Fatalf("migrate: %v", err)
	}
	if err := db.Seed(context.Background(), pool); err != nil {
		log.Fatalf("seed: %v", err)
	}

	rdb := redis.NewClient(&redis.Options{Addr: env("REDIS_ADDR", "redis:6379")})
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		log.Fatalf("connect redis: %v", err)
	}

	srv := api.NewServer(pool, rdb)
	addr := ":" + env("APP_PORT", "8080")
	httpSrv := &http.Server{Addr: addr, Handler: srv.Router(), ReadHeaderTimeout: 5 * time.Second}

	go func() {
		log.Printf("seafood trace service listening on %s", addr)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("http: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(ctx)
}
