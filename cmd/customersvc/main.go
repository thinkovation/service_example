// Command customersvc runs the customer microservice.
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

	"github.com/thinkovation/service_example/internal/buildinfo"
	"github.com/thinkovation/service_example/internal/config"
	"github.com/thinkovation/service_example/internal/httpapi"
	"github.com/thinkovation/service_example/internal/store/sqlstore"
)

func main() {
	if err := run(); err != nil {
		// The logger may not exist yet, so report to stderr as well.
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	logger := newLogger(cfg)
	slog.SetDefault(logger)

	build := buildinfo.Get()
	logger.Info("starting service",
		slog.String("service", cfg.ServiceName),
		slog.String("env", cfg.Env),
		slog.String("version", build.Version),
		slog.String("commit", build.Commit),
		slog.String("store_driver", cfg.StoreDriver),
		slog.String("addr", cfg.HTTPAddr),
	)

	// Signals are wired up before the database so that a slow or unreachable
	// database can still be interrupted with Ctrl-C.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := sqlstore.Open(ctx, sqlstore.Config{
		Dialect:         cfg.StoreDriver,
		DSN:             cfg.DSN(),
		MaxOpenConns:    cfg.MaxOpenConns,
		MaxIdleConns:    cfg.MaxIdleConns,
		ConnMaxLifetime: cfg.ConnMaxLifetime,
		ConnMaxIdleTime: cfg.ConnMaxIdleTime,
		AutoMigrate:     cfg.AutoMigrate,
		PingTimeout:     cfg.DBPingTimeout,
	})
	if err != nil {
		return err
	}
	defer func() {
		if err := db.Close(); err != nil {
			logger.Error("close store", slog.Any("error", err))
		}
	}()

	api := httpapi.New(httpapi.Options{
		Store:           db,
		Logger:          logger,
		ServiceName:     cfg.ServiceName,
		Env:             cfg.Env,
		MaxBodyBytes:    cfg.MaxBodyBytes,
		RequestTimeout:  cfg.RequestTimeout,
		HealthTimeout:   cfg.DBPingTimeout,
		DefaultPageSize: cfg.DefaultPageSize,
		MaxPageSize:     cfg.MaxPageSize,
	})

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           api,
		ReadTimeout:       cfg.ReadTimeout,
		ReadHeaderTimeout: cfg.ReadTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       cfg.IdleTimeout,
		BaseContext:       func(net.Listener) context.Context { return context.Background() },
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}

	listener, err := net.Listen("tcp", cfg.HTTPAddr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.HTTPAddr, err)
	}

	serveErr := make(chan error, 1)
	go func() {
		logger.Info("listening", slog.String("addr", listener.Addr().String()))
		if err := srv.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
		stop() // restore default signal handling: a second signal now kills us
		logger.Info("shutdown signal received", slog.Duration("grace", cfg.ShutdownTimeout))
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("graceful shutdown failed, closing connections", slog.Any("error", err))
		_ = srv.Close()
		return err
	}
	if err := <-serveErr; err != nil {
		return err
	}

	logger.Info("shutdown complete")
	return nil
}

func newLogger(cfg config.Config) *slog.Logger {
	level := slog.LevelInfo
	switch cfg.LogLevel {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}

	opts := &slog.HandlerOptions{Level: level}
	var handler slog.Handler
	if cfg.LogFormat == "text" {
		handler = slog.NewTextHandler(os.Stdout, opts)
	} else {
		handler = slog.NewJSONHandler(os.Stdout, opts)
	}
	return slog.New(handler).With(
		slog.String("service", cfg.ServiceName),
		slog.String("env", cfg.Env),
	)
}
