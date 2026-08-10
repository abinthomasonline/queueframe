package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/abinthomasonline/queueframe/internal/broker"
	"github.com/abinthomasonline/queueframe/internal/config"
	"github.com/abinthomasonline/queueframe/internal/kitebroker"
	"github.com/abinthomasonline/queueframe/internal/orders"
	"github.com/abinthomasonline/queueframe/internal/processlock"
	"github.com/abinthomasonline/queueframe/internal/scheduler"
	"github.com/abinthomasonline/queueframe/internal/secure"
	"github.com/abinthomasonline/queueframe/internal/store"
	"github.com/abinthomasonline/queueframe/internal/web"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)
	if err := run(); err != nil {
		slog.Error("Queueframe stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	cfg.DatabasePath, err = canonicalDatabasePath(cfg.DatabasePath)
	if err != nil {
		return err
	}
	lock, err := processlock.Acquire(cfg.DatabasePath + ".lock")
	if err != nil {
		return err
	}
	defer lock.Close()

	vault, err := secure.NewVault(cfg.MasterKey)
	if err != nil {
		return err
	}
	for index := range cfg.MasterKey {
		cfg.MasterKey[index] = 0
	}
	database, err := store.Open(cfg.DatabasePath, vault)
	if err != nil {
		return err
	}
	defer database.Close()
	if err := database.BootstrapAdmin(context.Background(), cfg.AdminUsername, cfg.AdminPassword); err != nil {
		return err
	}
	if err := database.DeleteExpiredSessions(context.Background()); err != nil {
		return fmt.Errorf("delete expired web sessions: %w", err)
	}
	cfg.AdminPassword = ""

	var brokerFactory broker.Factory = kitebroker.NewFactory()
	if cfg.DryRun {
		brokerFactory = broker.NewDryRunFactory(brokerFactory)
	}
	catalog := orders.NewCatalog()
	dailyScheduler, err := scheduler.New(database, brokerFactory, catalog, cfg)
	if err != nil {
		return err
	}
	application, err := web.New(web.Options{
		Store: database, Broker: brokerFactory, Catalog: catalog, Config: cfg, Status: dailyScheduler.Status,
	})
	if err != nil {
		return err
	}

	server := &http.Server{
		Addr:              cfg.Address,
		Handler:           application.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	schedulerDone := make(chan error, 1)
	go func() { schedulerDone <- dailyScheduler.Run(ctx) }()
	serverErrors := make(chan error, 1)
	go func() {
		slog.Info("web server listening", "address", cfg.Address, "base_url", cfg.BaseURL, "dry_run", cfg.DryRun)
		serverErrors <- server.ListenAndServe()
	}()

	var schedulerErr, listenerErr error
	schedulerFinished := false
	select {
	case <-ctx.Done():
	case err := <-serverErrors:
		if !errors.Is(err, http.ErrServerClosed) {
			listenerErr = err
		}
	case schedulerErr = <-schedulerDone:
		schedulerFinished = true
	}
	cancel()
	serverDone := make(chan error, 1)
	go func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer shutdownCancel()
		serverDone <- server.Shutdown(shutdownCtx)
	}()
	if !schedulerFinished {
		var drainErr error
		schedulerErr, drainErr = waitForScheduler(schedulerDone, 75*time.Second)
		listenerErr = errors.Join(listenerErr, drainErr)
	}
	serverErr := <-serverDone
	if schedulerErr != nil && !errors.Is(schedulerErr, context.Canceled) {
		return errors.Join(listenerErr, serverErr, schedulerErr)
	}
	return errors.Join(listenerErr, serverErr)
}

// waitForScheduler reports a missed drain target but never returns while the
// scheduler can still access the database. Process-level termination remains
// the final bound if a dependency violates its own context deadline.
func waitForScheduler(done <-chan error, target time.Duration) (error, error) {
	timer := time.NewTimer(target)
	defer timer.Stop()
	select {
	case err := <-done:
		return err, nil
	case <-timer.C:
		err := <-done
		return err, fmt.Errorf("scheduler did not finish persisting in-memory work before the %s shutdown deadline", target)
	}
}

// canonicalDatabasePath makes differently-spelled paths to the same database
// share one process-lock path. For a new database only its existing parent can
// be resolved; for an existing database the file itself is resolved too.
func canonicalDatabasePath(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve database path: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(absolute), 0o700); err != nil {
		return "", fmt.Errorf("create database directory: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(absolute); err == nil {
		return resolved, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("resolve database path: %w", err)
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(absolute))
	if err != nil {
		return "", fmt.Errorf("resolve database directory: %w", err)
	}
	return filepath.Join(parent, filepath.Base(absolute)), nil
}
