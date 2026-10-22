// Command smoke exercises every endpoint of a running service and reports what
// answered. It is written in Go rather than as a shell script so that it runs
// the same on Windows, WSL, Linux and in CI, with nothing installed but Go.
//
//	go run ./cmd/smoke [base-url]      default: http://localhost:8080
//
// It exits non-zero if any check fails, which makes it usable as a deploy gate.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

func main() {
	base := "http://localhost:8080"
	if len(os.Args) > 1 {
		base = strings.TrimRight(os.Args[1], "/")
	}
	if err := run(base); err != nil {
		fmt.Fprintln(os.Stderr, "smoke:", err)
		os.Exit(1)
	}
}

func run(base string) error {
	fmt.Println("smoke:", base)
	client := &http.Client{Timeout: 10 * time.Second}
	failures := 0

	// check reports one expectation and remembers whether it held.
	check := func(name, method, path string, want int, body string) []byte {
		status, payload, err := call(client, method, base+path, body)
		switch {
		case err != nil:
			fmt.Printf("  FAIL  %-26s %s %s: %v\n", name, method, path, err)
			failures++
		case status != want:
			fmt.Printf("  FAIL  %-26s %s %s: got %d, want %d\n", name, method, path, status, want)
			fmt.Printf("        %s\n", firstLine(payload))
			failures++
		default:
			fmt.Printf("  ok    %-26s %s %s\n", name, method, path)
		}
		return payload
	}

	check("status", http.MethodGet, "/status", http.StatusOK, "")
	check("health", http.MethodGet, "/zhealth", http.StatusOK, "")

	created := check("create", http.MethodPost, "/customers", http.StatusCreated,
		`{"name":"Smoke Test Co","status":"pending"}`)
	var customer struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(created, &customer); err != nil || customer.ID == "" {
		return fmt.Errorf("create returned no usable id: %s", firstLine(created))
	}
	item := "/customers/" + customer.ID

	check("read", http.MethodGet, item, http.StatusOK, "")
	check("patch", http.MethodPatch, item, http.StatusOK, `{"status":"active"}`)
	check("replace", http.MethodPut, item, http.StatusOK, `{"name":"Smoke Test Company","status":"active"}`)
	check("list", http.MethodGet, "/customers?limit=5", http.StatusOK, "")
	check("query", "QUERY", "/customers", http.StatusOK, `{"name_contains":"smoke","limit":5}`)
	check("query (POST alias)", http.MethodPost, "/customers/query", http.StatusOK, `{"name_contains":"smoke","limit":5}`)
	check("unknown id is 404", http.MethodGet, "/customers/00000000-0000-7000-8000-000000000000", http.StatusNotFound, "")
	check("bad id is 400", http.MethodGet, "/customers/not-a-uuid", http.StatusBadRequest, "")
	check("bad body is 400", http.MethodPost, "/customers", http.StatusBadRequest, `{"name":""}`)
	check("unknown route is 404", http.MethodGet, "/nope", http.StatusNotFound, "")
	check("bad method is 405", http.MethodDelete, "/customers", http.StatusMethodNotAllowed, "")
	check("docs page", http.MethodGet, "/swagger/", http.StatusOK, "")
	check("openapi description", http.MethodGet, "/swagger/openapi.yaml", http.StatusOK, "")

	if failures > 0 {
		return fmt.Errorf("%d check(s) failed", failures)
	}
	fmt.Println("smoke: all checks passed")
	return nil
}

func call(client *http.Client, method, url, body string) (int, []byte, error) {
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		return 0, nil, err
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()

	payload, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, payload, err
}

func firstLine(payload []byte) string {
	text := strings.TrimSpace(string(payload))
	if i := strings.IndexByte(text, '\n'); i >= 0 {
		text = text[:i]
	}
	if len(text) > 200 {
		text = text[:200] + "..."
	}
	return text
}
