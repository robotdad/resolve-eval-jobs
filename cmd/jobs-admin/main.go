// Command jobs-admin is a read-only administrative CLI for the word-count job service.
//
// Usage:
//
//	jobs-admin [--endpoint URL] [--json]
//
// jobs-admin lists all jobs from the running word-count service. By default it
// prints human-readable output. With --json it emits a single JSON array to
// stdout whose content and element order match GET /jobs exactly.
//
// Flags:
//
//	--endpoint URL   Service base URL (default: http://127.0.0.1:8080)
//	--json           Emit a JSON array to stdout instead of human-readable text
//
// Exit status:
//
//	0   Success
//	1   Error (invalid endpoint, unreachable service, HTTP error, etc.)
//
// Diagnostics are always written to stderr. On error with --json, stdout is
// empty; the JSON array is only written after the full response is validated.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
)

// job mirrors the service Job type for JSON decoding.
type job struct {
	ID     string  `json:"id"`
	Status string  `json:"status"`
	Text   string  `json:"text"`
	Result *result `json:"result,omitempty"`
}

// result mirrors the service Result type.
type result struct {
	WordCount int `json:"word_count"`
}

func main() {
	endpoint := flag.String("endpoint", "http://127.0.0.1:8080", "service base URL")
	jsonOut := flag.Bool("json", false, "emit JSON array to stdout")
	flag.Parse()

	if err := run(*endpoint, *jsonOut); err != nil {
		fmt.Fprintf(os.Stderr, "jobs-admin: %v\n", err)
		os.Exit(1)
	}
}

// run fetches the job list and writes output. It returns a non-nil error on
// any failure; the caller prints to stderr and exits with status 1.
func run(endpoint string, jsonOut bool) error {
	// Validate the endpoint URL before making any network call.
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("invalid endpoint %q: must be an absolute URL (e.g. http://127.0.0.1:8080)", endpoint)
	}

	jobsURL := endpoint + "/jobs"
	resp, err := http.Get(jobsURL) //nolint:noctx
	if err != nil {
		return fmt.Errorf("request to %s failed: %w", jobsURL, err)
	}
	defer resp.Body.Close()

	// Read the full body before deciding what to do with it.
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("reading response from %s: %w", jobsURL, err)
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("service returned HTTP %d from %s: %s", resp.StatusCode, jobsURL, trimBody(body))
	}

	// Parse the JSON array. We do this before writing any output so that a
	// parse error does not leave partial JSON on stdout.
	var jobs []job
	if err := json.Unmarshal(body, &jobs); err != nil {
		return fmt.Errorf("invalid JSON from %s: %w", jobsURL, err)
	}
	if jobs == nil {
		return fmt.Errorf("invalid response from %s: expected a JSON array, got null", jobsURL)
	}

	if jsonOut {
		// Preserve the API response bytes after validating the full JSON array.
		os.Stdout.Write(body)
		// Ensure a trailing newline for shell friendliness.
		if len(body) > 0 && body[len(body)-1] != '\n' {
			os.Stdout.Write([]byte("\n"))
		}
		return nil
	}

	// Human-readable output.
	if len(jobs) == 0 {
		fmt.Println("No jobs found.")
		return nil
	}

	fmt.Printf("%-6s  %-10s  %s\n", "ID", "STATUS", "WORD COUNT")
	fmt.Printf("%-6s  %-10s  %s\n", "------", "----------", "----------")
	for _, j := range jobs {
		wc := "-"
		if j.Result != nil {
			wc = fmt.Sprintf("%d", j.Result.WordCount)
		}
		fmt.Printf("%-6s  %-10s  %s\n", j.ID, j.Status, wc)
	}
	return nil
}

// trimBody returns a short, printable excerpt of a response body for error messages.
func trimBody(b []byte) string {
	s := string(b)
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return s
}
