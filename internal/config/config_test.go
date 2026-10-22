package config

import (
	"strings"
	"testing"
	"time"
)

func TestLoadDefaults(t *testing.T) {
	cfg, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if cfg.StoreDriver != "sqlite" {
		t.Errorf("StoreDriver = %q, want \"sqlite\"", cfg.StoreDriver)
	}
	if cfg.HTTPAddr != ":8080" {
		t.Errorf("HTTPAddr = %q, want \":8080\"", cfg.HTTPAddr)
	}
	if cfg.SQLiteDSN == "" {
		t.Error("SQLiteDSN is empty, want a default development path")
	}
	if !cfg.AutoMigrate {
		t.Error("AutoMigrate = false, want true by default")
	}
	if cfg.DefaultPageSize > cfg.MaxPageSize {
		t.Errorf("default page size %d exceeds the maximum %d", cfg.DefaultPageSize, cfg.MaxPageSize)
	}
}

func TestLoadOverrides(t *testing.T) {
	t.Setenv("SERVICE_NAME", "customers")
	t.Setenv("STORE_DRIVER", "postgres")
	t.Setenv("DATABASE_URL", "postgres://user:pass@db:5432/customers?sslmode=disable")
	t.Setenv("HTTP_ADDR", "127.0.0.1:9090")
	t.Setenv("HTTP_REQUEST_TIMEOUT", "3s")
	t.Setenv("DB_AUTO_MIGRATE", "false")
	t.Setenv("PAGE_SIZE_DEFAULT", "10")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if cfg.ServiceName != "customers" || cfg.HTTPAddr != "127.0.0.1:9090" {
		t.Errorf("config = %+v, want the overridden identity and address", cfg)
	}
	if cfg.PostgresDSN != "postgres://user:pass@db:5432/customers?sslmode=disable" {
		t.Errorf("PostgresDSN = %q, want the configured URL", cfg.PostgresDSN)
	}
	if cfg.RequestTimeout != 3*time.Second {
		t.Errorf("RequestTimeout = %s, want 3s", cfg.RequestTimeout)
	}
	if cfg.AutoMigrate {
		t.Error("AutoMigrate = true, want false")
	}
	if cfg.DefaultPageSize != 10 {
		t.Errorf("DefaultPageSize = %d, want 10", cfg.DefaultPageSize)
	}
}

func TestLoadRejectsBadValues(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"postgres without a DSN", map[string]string{"STORE_DRIVER": "postgres"}, "DATABASE_URL"},
		{"unknown driver", map[string]string{"STORE_DRIVER": "mysql"}, "STORE_DRIVER"},
		{"bad duration", map[string]string{"HTTP_READ_TIMEOUT": "soon"}, "HTTP_READ_TIMEOUT"},
		{"bad integer", map[string]string{"DB_MAX_OPEN_CONNS": "lots"}, "DB_MAX_OPEN_CONNS"},
		{"bad boolean", map[string]string{"DB_AUTO_MIGRATE": "maybe"}, "DB_AUTO_MIGRATE"},
		{"bad log level", map[string]string{"LOG_LEVEL": "shout"}, "LOG_LEVEL"},
		{"page size inversion", map[string]string{"PAGE_SIZE_DEFAULT": "100", "PAGE_SIZE_MAX": "10"}, "PAGE_SIZE_DEFAULT"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			_, err := Load()
			if err == nil {
				t.Fatal("load: err = nil, want an error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %s", err, tc.want)
			}
		})
	}
}
