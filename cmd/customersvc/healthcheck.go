package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/thinkovation/service_example/internal/buildinfo"
)

// usage describes the subcommands the binary accepts. With none it runs the
// service, which is what a container entrypoint does.
const usage = `customersvc - customer microservice

Usage:
  customersvc              Run the service (configuration comes from the environment)
  customersvc healthcheck  Probe /zhealth on the local instance; exit 0 when healthy
  customersvc version      Print build information
`

// healthcheck probes /zhealth so that a container runtime can judge readiness
// without a shell in the image. The default target is derived from HTTP_ADDR,
// and HEALTHCHECK_URL overrides it outright.
func healthcheck(args []string) int {
	target, err := healthTarget(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck:", err)
		return 2
	}

	timeout := 3 * time.Second
	if raw := os.Getenv("HEALTHCHECK_TIMEOUT"); raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil {
			fmt.Fprintf(os.Stderr, "healthcheck: HEALTHCHECK_TIMEOUT: %q is not a duration\n", raw)
			return 2
		}
		timeout = parsed
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck:", err)
		return 2
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck:", err)
		return 1
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "healthcheck: %s returned %d: %s\n", target, resp.StatusCode, strings.TrimSpace(string(body)))
		return 1
	}
	fmt.Println(strings.TrimSpace(string(body)))
	return 0
}

// healthTarget resolves the URL to probe: an explicit argument, then
// HEALTHCHECK_URL, then the local address the service is configured to listen
// on. A wildcard or empty listen host becomes a loopback address, since the
// probe always runs beside the process.
func healthTarget(args []string) (string, error) {
	if len(args) > 0 && args[0] != "" {
		return args[0], nil
	}
	if raw := os.Getenv("HEALTHCHECK_URL"); raw != "" {
		return raw, nil
	}

	addr := os.Getenv("HTTP_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", fmt.Errorf("HTTP_ADDR %q is not a host:port address", addr)
	}
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		host = "127.0.0.1"
	}

	return (&url.URL{Scheme: "http", Host: net.JoinHostPort(host, port), Path: "/zhealth"}).String(), nil
}

// printVersion reports the build identity, for `docker run <image> version`.
func printVersion() {
	info := buildinfo.Get()
	fmt.Printf("customersvc %s\ncommit:     %s\nbuilt:      %s\ngo:         %s\n",
		info.Version, orDash(info.Commit), orDash(info.BuildDate), info.GoVersion)
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
