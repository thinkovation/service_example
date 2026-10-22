// Package config loads the service configuration from environment variables.
// Every setting has a usable default except the Postgres DSN, which is only
// required when the Postgres driver is selected.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/thinkovation/service_example/internal/store/sqlstore"
)

// Config is the fully resolved service configuration.
type Config struct {
	ServiceName string
	Env         string

	HTTPAddr        string
	ReadTimeout     time.Duration
	WriteTimeout    time.Duration
	IdleTimeout     time.Duration
	RequestTimeout  time.Duration
	ShutdownTimeout time.Duration
	MaxBodyBytes    int64

	StoreDriver     string
	SQLiteDSN       string
	PostgresDSN     string
	AutoMigrate     bool
	MaxOpenConns    int
	MaxIdleConns    int
	ConnMaxLifetime time.Duration
	ConnMaxIdleTime time.Duration
	DBPingTimeout   time.Duration

	DefaultPageSize int
	MaxPageSize     int

	LogLevel  string
	LogFormat string
}

// DSN returns the connection string for the selected driver.
func (c Config) DSN() string {
	if c.StoreDriver == sqlstore.DialectPostgres {
		return c.PostgresDSN
	}
	return c.SQLiteDSN
}

// Load reads the configuration from the environment and validates it.
func Load() (Config, error) {
	cfg := Config{
		ServiceName: env("SERVICE_NAME", "customer-service"),
		Env:         env("SERVICE_ENV", "development"),

		HTTPAddr:  env("HTTP_ADDR", ":8080"),
		LogLevel:  strings.ToLower(env("LOG_LEVEL", "info")),
		LogFormat: strings.ToLower(env("LOG_FORMAT", "json")),

		StoreDriver: strings.ToLower(env("STORE_DRIVER", sqlstore.DialectSQLite)),
		SQLiteDSN:   env("SQLITE_DSN", "file:./data/customers.db"),
		PostgresDSN: env("DATABASE_URL", ""),
	}

	var errs []string
	collect := func(err error) {
		if err != nil {
			errs = append(errs, err.Error())
		}
	}

	var err error
	cfg.ReadTimeout, err = envDuration("HTTP_READ_TIMEOUT", 15*time.Second)
	collect(err)
	cfg.WriteTimeout, err = envDuration("HTTP_WRITE_TIMEOUT", 30*time.Second)
	collect(err)
	cfg.IdleTimeout, err = envDuration("HTTP_IDLE_TIMEOUT", 60*time.Second)
	collect(err)
	cfg.RequestTimeout, err = envDuration("HTTP_REQUEST_TIMEOUT", 10*time.Second)
	collect(err)
	cfg.ShutdownTimeout, err = envDuration("HTTP_SHUTDOWN_TIMEOUT", 15*time.Second)
	collect(err)
	cfg.MaxBodyBytes, err = envInt64("HTTP_MAX_BODY_BYTES", 1<<20)
	collect(err)

	cfg.AutoMigrate, err = envBool("DB_AUTO_MIGRATE", true)
	collect(err)
	cfg.MaxOpenConns, err = envInt("DB_MAX_OPEN_CONNS", 25)
	collect(err)
	cfg.MaxIdleConns, err = envInt("DB_MAX_IDLE_CONNS", 5)
	collect(err)
	cfg.ConnMaxLifetime, err = envDuration("DB_CONN_MAX_LIFETIME", time.Hour)
	collect(err)
	cfg.ConnMaxIdleTime, err = envDuration("DB_CONN_MAX_IDLE_TIME", 5*time.Minute)
	collect(err)
	cfg.DBPingTimeout, err = envDuration("DB_PING_TIMEOUT", 5*time.Second)
	collect(err)

	cfg.DefaultPageSize, err = envInt("PAGE_SIZE_DEFAULT", 50)
	collect(err)
	cfg.MaxPageSize, err = envInt("PAGE_SIZE_MAX", 500)
	collect(err)

	if len(errs) > 0 {
		return Config{}, fmt.Errorf("config: %s", strings.Join(errs, "; "))
	}
	if err := cfg.validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) validate() error {
	var errs []string

	switch c.StoreDriver {
	case sqlstore.DialectSQLite:
		if strings.TrimSpace(c.SQLiteDSN) == "" {
			errs = append(errs, "SQLITE_DSN must not be empty")
		}
	case sqlstore.DialectPostgres:
		if strings.TrimSpace(c.PostgresDSN) == "" {
			errs = append(errs, "DATABASE_URL must be set when STORE_DRIVER=postgres")
		}
	default:
		errs = append(errs, fmt.Sprintf("STORE_DRIVER must be %q or %q, got %q",
			sqlstore.DialectSQLite, sqlstore.DialectPostgres, c.StoreDriver))
	}

	if strings.TrimSpace(c.HTTPAddr) == "" {
		errs = append(errs, "HTTP_ADDR must not be empty")
	}
	if c.DefaultPageSize <= 0 {
		errs = append(errs, "PAGE_SIZE_DEFAULT must be greater than zero")
	}
	if c.MaxPageSize <= 0 {
		errs = append(errs, "PAGE_SIZE_MAX must be greater than zero")
	}
	if c.DefaultPageSize > c.MaxPageSize {
		errs = append(errs, "PAGE_SIZE_DEFAULT must not exceed PAGE_SIZE_MAX")
	}
	if c.MaxBodyBytes <= 0 {
		errs = append(errs, "HTTP_MAX_BODY_BYTES must be greater than zero")
	}
	switch c.LogFormat {
	case "json", "text":
	default:
		errs = append(errs, fmt.Sprintf("LOG_FORMAT must be \"json\" or \"text\", got %q", c.LogFormat))
	}
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		errs = append(errs, fmt.Sprintf("LOG_LEVEL must be one of debug, info, warn, error, got %q", c.LogLevel))
	}

	if len(errs) > 0 {
		return fmt.Errorf("config: %s", strings.Join(errs, "; "))
	}
	return nil
}

func env(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return fallback
}

func envDuration(key string, fallback time.Duration) (time.Duration, error) {
	raw, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(raw) == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(strings.TrimSpace(raw))
	if err != nil {
		return 0, fmt.Errorf("%s: %q is not a duration (e.g. \"30s\")", key, raw)
	}
	if d < 0 {
		return 0, fmt.Errorf("%s: must not be negative", key)
	}
	return d, nil
}

func envInt(key string, fallback int) (int, error) {
	v, err := envInt64(key, int64(fallback))
	return int(v), err
}

func envInt64(key string, fallback int64) (int64, error) {
	raw, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(raw) == "" {
		return fallback, nil
	}
	v, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: %q is not an integer", key, raw)
	}
	return v, nil
}

func envBool(key string, fallback bool) (bool, error) {
	raw, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(raw) == "" {
		return fallback, nil
	}
	v, err := strconv.ParseBool(strings.TrimSpace(raw))
	if err != nil {
		return false, fmt.Errorf("%s: %q is not a boolean", key, raw)
	}
	return v, nil
}
