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
	"github.com/thinkovation/service_example/internal/store"
	"github.com/thinkovation/service_example/internal/store/postgres"
	"github.com/thinkovation/service_example/internal/store/sqlite"
)

func main() {
	// Subcommands keep the image free of a shell: the container healthcheck is
	// the binary itself. With no arguments the process runs the service.
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "healthcheck":
			os.Exit(healthcheck(os.Args[2:]))
		case "version", "--version", "-version":
			printVersion()
			return
		case "help", "--help", "-h":
			fmt.Print(usage)
			return
		default:
			fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", os.Args[1], usage)
			os.Exit(2)
		}
	}

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
	// service and env are already attached to every line by newLogger.
	logger.Info("starting service",
		slog.String("version", build.Version),
		slog.String("commit", build.Commit),
		slog.String("store_driver", cfg.StoreDriver),
		slog.String("addr", cfg.HTTPAddr),
	)

	// Signals are wired up before the database so that a slow or unreachable
	// database can still be interrupted with Ctrl-C.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := openStore(ctx, cfg)
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

// openStore picks the driver named by STORE_DRIVER. Each driver is its own
// package with its own SQL; this is the only place that chooses between them.
func openStore(ctx context.Context, cfg config.Config) (store.Store, error) {
	switch cfg.StoreDriver {
	case config.DriverSQLite:
		return sqlite.Open(ctx, sqlite.Config{
			DSN:         cfg.SQLiteDSN,
			AutoMigrate: cfg.AutoMigrate,
			PingTimeout: cfg.DBPingTimeout,
		})

	case config.DriverPostgres:
		return postgres.Open(ctx, postgres.Config{
			DSN:             cfg.PostgresDSN,
			AutoMigrate:     cfg.AutoMigrate,
			PingTimeout:     cfg.DBPingTimeout,
			MaxOpenConns:    cfg.MaxOpenConns,
			MaxIdleConns:    cfg.MaxIdleConns,
			ConnMaxLifetime: cfg.ConnMaxLifetime,
			ConnMaxIdleTime: cfg.ConnMaxIdleTime,
		})

	default:
		// config.Load has already rejected anything else; this keeps the
		// switch honest if a driver is added there and forgotten here.
		return nil, fmt.Errorf("unsupported STORE_DRIVER %q", cfg.StoreDriver)
	}
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
