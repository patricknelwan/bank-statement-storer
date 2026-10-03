package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"example.com/bca/internal/auth"
	"example.com/bca/internal/config"
	"example.com/bca/internal/database"
	"example.com/bca/internal/gmail"
	"example.com/bca/internal/httpapi"
	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "time/tzdata"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	if err := run(); err != nil {
		slog.Error("service stopped", "error", err)
		os.Exit(1)
	}
}
func run() error {
	if len(os.Args) > 1 && os.Args[1] == "seed" {
		return seed()
	}
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		client := http.Client{Timeout: 3 * time.Second}
		response, err := client.Get("http://127.0.0.1:8080/health/ready")
		if err != nil {
			return err
		}
		defer response.Body.Close()
		if response.StatusCode != 200 {
			return fmt.Errorf("readiness status %d", response.StatusCode)
		}
		return nil
	}
	if len(os.Args) > 1 && os.Args[1] == "migrate" {
		path := os.Getenv("MIGRATIONS_DIR")
		if path == "" {
			path = "db/migrations"
		}
		url := os.Getenv("DATABASE_URL")
		if url == "" {
			return errors.New("DATABASE_URL is required")
		}
		m, err := migrate.New("file://"+path, url)
		if err != nil {
			return err
		}
		defer m.Close()
		err = m.Up()
		if errors.Is(err, migrate.ErrNoChange) {
			return nil
		}
		return err
	}
	c, err := config.Load()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	poolConfig, err := pgxpool.ParseConfig(c.DatabaseURL)
	if err != nil {
		return fmt.Errorf("DATABASE_URL: %w", err)
	}
	poolConfig.MaxConns = 4
	db, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		return err
	}
	defer db.Close()
	if err = db.Ping(ctx); err != nil {
		return fmt.Errorf("database: %w", err)
	}
	ready, err := database.New(db).DatabaseReady(ctx)
	if err != nil || !ready {
		return errors.New("database migrations are not applied")
	}
	authService, err := auth.New(ctx, db, c)
	if err != nil {
		return fmt.Errorf("google identity setup: %w", err)
	}
	worker := gmail.NewWorker(db, authService, c)
	api := httpapi.API{DB: db, Auth: authService, Worker: worker, Config: c}
	listener, err := net.Listen("tcp", c.HTTPAddr)
	if err != nil {
		return err
	}
	server := &http.Server{Handler: api.Router(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second}
	serverErrors := make(chan error, 1)
	go func() { serverErrors <- server.Serve(listener) }()
	slog.Info("http listening", "addr", c.HTTPAddr)
	workerCtx, cancelWorker := context.WithCancel(ctx)
	workerDone := make(chan struct{})
	go func() { defer close(workerDone); worker.Run(workerCtx) }()
	select {
	case <-ctx.Done():
	case err = <-serverErrors:
		if !errors.Is(err, http.ErrServerClosed) {
			cancelWorker()
			return err
		}
	}
	cancelWorker()
	<-workerDone
	shutdown, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return server.Shutdown(shutdown)
}
