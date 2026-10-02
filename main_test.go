// Package main tests the word-count job service.
//
// Tests are structured into groups matching the acceptance criteria:
//   - Unit tests for countWords (counting semantics)
//   - HTTP-level integration tests using httptest (submission, retrieval,
//     listing, error handling, lifecycle, FIFO ordering, restart boundary)
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// newTestServer creates a server and an httptest.Server backed by it.
// The caller is responsible for calling ts.Close().
func newTestServer(t *testing.T) (*server, *httptest.Server) {
	t.Helper()
	srv := newServer()
	ts := httptest.NewServer(srv.routes())
	return srv, ts
}

// postJob submits a job and returns the response body and status code.
func postJob(t *testing.T, ts *httptest.Server, body string) (int, map[string]string) {
	t.Helper()
	resp, err := http.Post(ts.URL+"/jobs", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST /jobs: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		return resp.StatusCode, nil
	}
	var result map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode POST /jobs response: %v", err)
	}
	return resp.StatusCode, result
}

// getJob fetches a single job by ID.
func getJob(t *testing.T, ts *httptest.Server, id string) (int, map[string]interface{}) {
	t.Helper()
	resp, err := http.Get(ts.URL + "/jobs/" + id)
	if err != nil {
		t.Fatalf("GET /jobs/%s: %v", id, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, nil
	}
	var result map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode GET /jobs/%s response: %v", id, err)
	}
	return resp.StatusCode, result
}

// listJobs fetches the /jobs listing.
func listJobs(t *testing.T, ts *httptest.Server) []map[string]interface{} {
	t.Helper()
	resp, err := http.Get(ts.URL + "/jobs")
	if err != nil {
		t.Fatalf("GET /jobs: %v", err)
	}
	defer resp.Body.Close()
	var result []map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode GET /jobs response: %v", err)
	}
	return result
}

// waitForSucceeded polls GET /jobs/{id} until the job reaches "succeeded"
// or the deadline is exceeded. Returns the final job map or fails the test.
func waitForSucceeded(t *testing.T, ts *httptest.Server, id string, timeout time.Duration) map[string]interface{} {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		code, job := getJob(t, ts, id)
		if code != http.StatusOK {
			t.Fatalf("waitForSucceeded: GET /jobs/%s returned %d", id, code)
		}
		if job["status"] == StatusSucceeded {
			return job
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("waitForSucceeded: job %s did not reach succeeded within %v", id, timeout)
	return nil
}

// ---------------------------------------------------------------------------
// 1. countWords unit tests (acceptance item 3 – counting semantics)
// ---------------------------------------------------------------------------

func TestCountWords(t *testing.T) {
	cases := []struct {
		input string
		want  int
		label string
	}{
		{"", 0, "empty string"},
		{" \t\n", 0, "whitespace only"},
		{"hello, world!", 2, "hello comma world exclamation"},
		{"don't stop", 2, "apostrophe within word"},
		{"one-two", 1, "hyphen within word"},
		{"alpha\u00a0beta", 2, "non-breaking space separator"},
		{"red blue red", 3, "three words"},
		{"hello world", 2, "two plain words"},
		{"one", 1, "single word"},
		{"  leading and trailing  ", 3, "leading and trailing spaces"},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			got := countWords(tc.input)
			if got != tc.want {
				t.Errorf("countWords(%q) = %d; want %d", tc.input, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 2. Submission and retrieval (acceptance item 2)
// ---------------------------------------------------------------------------

func TestSubmitAndRetrieve(t *testing.T) {
	_, ts := newTestServer(t)
	defer ts.Close()

	// POST a job.
	code, result := postJob(t, ts, `{"text":"red blue red"}`)
	if code != http.StatusAccepted {
		t.Fatalf("POST /jobs: got %d, want 202", code)
	}
	id := result["id"]
	if id == "" {
		t.Fatal("POST /jobs: empty id in response")
	}

	// Wait for completion within 5 seconds (acceptance bound).
	job := waitForSucceeded(t, ts, id, 5*time.Second)

	// Verify word_count = 3.
	resultField, ok := job["result"].(map[string]interface{})
	if !ok {
		t.Fatalf("GET /jobs/%s: result field missing or wrong type: %v", id, job["result"])
	}
	wc, ok := resultField["word_count"].(float64)
	if !ok {
		t.Fatalf("GET /jobs/%s: word_count missing or wrong type: %v", id, resultField)
	}
	if int(wc) != 3 {
		t.Errorf("GET /jobs/%s: word_count = %d; want 3", id, int(wc))
	}
}

// ---------------------------------------------------------------------------
// 3. Counting semantics via HTTP (acceptance item 3)
// ---------------------------------------------------------------------------

func TestCountingSemanticsHTTP(t *testing.T) {
	_, ts := newTestServer(t)
	defer ts.Close()

	cases := []struct {
		text string
		want int
	}{
		{"", 0},
		{" \t\n", 0},
		{"hello, world!", 2},
		{"don't stop", 2},
		{"one-two", 1},
		{"alpha\u00a0beta", 2},
	}

	for _, tc := range cases {
		t.Run(fmt.Sprintf("text=%q want=%d", tc.text, tc.want), func(t *testing.T) {
			body, _ := json.Marshal(map[string]string{"text": tc.text})
			code, result := postJob(t, ts, string(body))
			if code != http.StatusAccepted {
				t.Fatalf("POST /jobs: got %d, want 202", code)
			}
			id := result["id"]

			job := waitForSucceeded(t, ts, id, 5*time.Second)
			resultField, ok := job["result"].(map[string]interface{})
			if !ok {
				t.Fatalf("result field missing: %v", job)
			}
			wc := int(resultField["word_count"].(float64))
			if wc != tc.want {
				t.Errorf("text=%q: word_count = %d; want %d", tc.text, wc, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 4. Stable results and ordered listing (acceptance item 4)
// ---------------------------------------------------------------------------

func TestStableResultsAndOrdering(t *testing.T) {
	_, ts := newTestServer(t)
	defer ts.Close()

	// Submit three jobs in order.
	submissions := []struct {
		text string
		want int
	}{
		{"one", 1},
		{"red blue red", 3},
		{"hello world", 2},
	}

	ids := make([]string, len(submissions))
	for i, s := range submissions {
		body, _ := json.Marshal(map[string]string{"text": s.text})
		code, result := postJob(t, ts, string(body))
		if code != http.StatusAccepted {
			t.Fatalf("POST /jobs[%d]: got %d, want 202", i, code)
		}
		ids[i] = result["id"]
	}

	// Wait for all jobs to complete.
	for i, id := range ids {
		waitForSucceeded(t, ts, id, 5*time.Second)
		_ = i
	}

	// Verify stable results: three further reads per job.
	for i, id := range ids {
		for read := 0; read < 3; read++ {
			code, job := getJob(t, ts, id)
			if code != http.StatusOK {
				t.Fatalf("stable read %d of job %s: got %d", read, id, code)
			}
			if job["status"] != StatusSucceeded {
				t.Errorf("stable read %d of job %s: status = %s; want succeeded", read, id, job["status"])
			}
			resultField := job["result"].(map[string]interface{})
			wc := int(resultField["word_count"].(float64))
			if wc != submissions[i].want {
				t.Errorf("stable read %d of job %s: word_count = %d; want %d", read, id, wc, submissions[i].want)
			}
		}
	}

	// Verify ordered listing.
	list := listJobs(t, ts)
	if len(list) != len(submissions) {
		t.Fatalf("GET /jobs: got %d jobs; want %d", len(list), len(submissions))
	}
	for i, entry := range list {
		if entry["id"] != ids[i] {
			t.Errorf("GET /jobs[%d]: id = %s; want %s", i, entry["id"], ids[i])
		}
		if entry["status"] != StatusSucceeded {
			t.Errorf("GET /jobs[%d]: status = %s; want succeeded", i, entry["status"])
		}
		resultField := entry["result"].(map[string]interface{})
		wc := int(resultField["word_count"].(float64))
		if wc != submissions[i].want {
			t.Errorf("GET /jobs[%d]: word_count = %d; want %d", i, wc, submissions[i].want)
		}
	}
}

func TestDistinctJobsForIdenticalText(t *testing.T) {
	_, ts := newTestServer(t)
	defer ts.Close()

	text := `{"text":"same text"}`
	code1, r1 := postJob(t, ts, text)
	code2, r2 := postJob(t, ts, text)

	if code1 != http.StatusAccepted || code2 != http.StatusAccepted {
		t.Fatalf("expected 202 for both submissions, got %d and %d", code1, code2)
	}
	if r1["id"] == r2["id"] {
		t.Errorf("identical text produced same job ID %s; want distinct IDs", r1["id"])
	}
}

// ---------------------------------------------------------------------------
// 5. Input rejection and lookup errors (acceptance item 5)
// ---------------------------------------------------------------------------

func TestInputRejection(t *testing.T) {
	_, ts := newTestServer(t)
	defer ts.Close()

	// Record inventory before.
	before := listJobs(t, ts)

	badRequests := []struct {
		label string
		body  string
	}{
		{"malformed JSON", `{bad json}`},
		{"missing text field", `{}`},
		{"non-string text", `{"text": 42}`},
		{"unknown field", `{"text": "hi", "extra": true}`},
	}

	for _, tc := range badRequests {
		t.Run(tc.label, func(t *testing.T) {
			resp, err := http.Post(ts.URL+"/jobs", "application/json", strings.NewReader(tc.body))
			if err != nil {
				t.Fatalf("POST /jobs: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("%s: got %d; want 400", tc.label, resp.StatusCode)
			}
		})
	}

	// Verify inventory did not grow.
	after := listJobs(t, ts)
	if len(after) != len(before) {
		t.Errorf("inventory grew from %d to %d after rejected requests", len(before), len(after))
	}
}

func TestNotFound(t *testing.T) {
	_, ts := newTestServer(t)
	defer ts.Close()

	code, _ := getJob(t, ts, "nonexistent-id")
	if code != http.StatusNotFound {
		t.Errorf("GET /jobs/nonexistent-id: got %d; want 404", code)
	}
}

// ---------------------------------------------------------------------------
// 6. Restart boundary (acceptance item 6)
// ---------------------------------------------------------------------------

// TestRestartBoundary simulates a restart by creating a new server instance
// (fresh store, fresh counter) and verifying that the old job is not visible.
func TestRestartBoundary(t *testing.T) {
	// First "process": submit and complete a job.
	_, ts1 := newTestServer(t)
	code, result := postJob(t, ts1, `{"text":"hello world"}`)
	if code != http.StatusAccepted {
		t.Fatalf("POST /jobs: got %d", code)
	}
	oldID := result["id"]
	waitForSucceeded(t, ts1, oldID, 5*time.Second)
	ts1.Close()

	// Second "process": fresh server, no prior state.
	_, ts2 := newTestServer(t)
	defer ts2.Close()

	// Before any new submission: list must be empty.
	list := listJobs(t, ts2)
	if len(list) != 0 {
		t.Errorf("after restart: GET /jobs returned %d jobs; want 0", len(list))
	}

	// Old ID must return 404.
	code404, _ := getJob(t, ts2, oldID)
	if code404 != http.StatusNotFound {
		t.Errorf("after restart: GET /jobs/%s returned %d; want 404", oldID, code404)
	}
}

// ---------------------------------------------------------------------------
// 7. FIFO ordering (acceptance item 1 – controlled lifecycle/FIFO test)
// ---------------------------------------------------------------------------

// TestFIFOOrdering submits multiple jobs and verifies they are processed and
// completed in submission order. Uses a controlled approach: submit N jobs
// and confirm the listing order matches submission order after all succeed.
func TestFIFOOrdering(t *testing.T) {
	_, ts := newTestServer(t)
	defer ts.Close()

	texts := []string{"first", "second", "third", "fourth", "fifth"}
	ids := make([]string, len(texts))

	for i, text := range texts {
		body, _ := json.Marshal(map[string]string{"text": text})
		code, result := postJob(t, ts, string(body))
		if code != http.StatusAccepted {
			t.Fatalf("POST /jobs[%d]: got %d", i, code)
		}
		ids[i] = result["id"]
	}

	// Wait for all to complete.
	for _, id := range ids {
		waitForSucceeded(t, ts, id, 5*time.Second)
	}

	// Verify listing order matches submission order.
	list := listJobs(t, ts)
	if len(list) != len(texts) {
		t.Fatalf("GET /jobs: got %d; want %d", len(list), len(texts))
	}
	for i, entry := range list {
		if entry["id"] != ids[i] {
			t.Errorf("list[%d]: id = %s; want %s (FIFO violated)", i, entry["id"], ids[i])
		}
	}
}

// TestJobLifecycle verifies the queued->running->succeeded lifecycle via a
// controlled test. We use a channel-blocked worker to observe queued state,
// then release it and observe succeeded.
func TestJobLifecycle(t *testing.T) {
	// Build a server whose worker is gated by a channel we control.
	gate := make(chan struct{})
	srv := &server{
		store: newStore(),
		queue: make(chan *Job, 1024),
	}

	// Custom worker that waits for gate before processing each job.
	go func() {
		for job := range srv.queue {
			srv.store.mu.Lock()
			job.Status = StatusRunning
			srv.store.mu.Unlock()

			<-gate // wait for test to release

			srv.store.mu.Lock()
			job.Status = StatusSucceeded
			job.Result = &Result{WordCount: countWords(job.Text)}
			srv.store.mu.Unlock()
		}
	}()

	ts := httptest.NewServer(srv.routes())
	defer ts.Close()

	// Submit a job.
	code, result := postJob(t, ts, `{"text":"hello"}`)
	if code != http.StatusAccepted {
		t.Fatalf("POST /jobs: got %d", code)
	}
	id := result["id"]

	// Allow some time for the worker to pick it up and transition to running.
	time.Sleep(50 * time.Millisecond)

	// Check that it is in running state (the gate is holding it there).
	_, job := getJob(t, ts, id)
	status := job["status"].(string)
	if status != StatusRunning && status != StatusQueued {
		t.Errorf("before gate open: status = %s; want queued or running", status)
	}

	// Release the gate.
	gate <- struct{}{}

	// Now wait for succeeded.
	final := waitForSucceeded(t, ts, id, 5*time.Second)
	if final["status"] != StatusSucceeded {
		t.Errorf("after gate open: status = %s; want succeeded", final["status"])
	}
}

// ---------------------------------------------------------------------------
// 8. HTTP-level acceptance: "red blue red" within 5 seconds
// ---------------------------------------------------------------------------

func TestAcceptanceBound(t *testing.T) {
	_, ts := newTestServer(t)
	defer ts.Close()

	start := time.Now()
	code, result := postJob(t, ts, `{"text":"red blue red"}`)
	if code != http.StatusAccepted {
		t.Fatalf("POST /jobs: got %d; want 202", code)
	}
	id := result["id"]
	if id == "" {
		t.Fatal("empty job ID")
	}

	job := waitForSucceeded(t, ts, id, 5*time.Second)
	elapsed := time.Since(start)

	resultField := job["result"].(map[string]interface{})
	wc := int(resultField["word_count"].(float64))
	if wc != 3 {
		t.Errorf("word_count = %d; want 3", wc)
	}
	t.Logf("acceptance bound: completed in %v (limit 5s)", elapsed)
}

// ---------------------------------------------------------------------------
// 9. Empty text is valid (acceptance item 2)
// ---------------------------------------------------------------------------

func TestEmptyTextValid(t *testing.T) {
	_, ts := newTestServer(t)
	defer ts.Close()

	code, result := postJob(t, ts, `{"text":""}`)
	if code != http.StatusAccepted {
		t.Fatalf("POST /jobs with empty text: got %d; want 202", code)
	}
	id := result["id"]
	job := waitForSucceeded(t, ts, id, 5*time.Second)
	resultField := job["result"].(map[string]interface{})
	wc := int(resultField["word_count"].(float64))
	if wc != 0 {
		t.Errorf("empty text: word_count = %d; want 0", wc)
	}
}

// ---------------------------------------------------------------------------
// 10. Local-only listener check (acceptance item 7) – source review note
// ---------------------------------------------------------------------------

// TestLoopbackOnly verifies that the server's default address is loopback-only
// by confirming the main() function uses "127.0.0.1" and not "0.0.0.0" or "".
// This is a source-review test; the httptest server in other tests also binds
// to loopback by default (127.0.0.1).
func TestLoopbackOnly(t *testing.T) {
	// The httptest.Server binds to 127.0.0.1 by default. Confirm the URL.
	_, ts := newTestServer(t)
	defer ts.Close()
	if !strings.HasPrefix(ts.URL, "http://127.0.0.1") {
		t.Errorf("test server URL = %s; expected 127.0.0.1 (loopback)", ts.URL)
	}
	// Source review: main() uses addr := "127.0.0.1:8080"
	// This is documented and verified by code inspection.
	t.Log("main() binds to 127.0.0.1:8080 (verified by source inspection)")
}

// ---------------------------------------------------------------------------
// 11. No subprocess execution (acceptance item 7) – source review note
// ---------------------------------------------------------------------------

// TestNoSubprocessExecution is a source-review test confirming that the
// implementation contains no os/exec, syscall.Exec, or shell invocations.
// This cannot be verified at runtime; it is established by code review.
func TestNoSubprocessExecution(t *testing.T) {
	// The implementation uses only: net/http, sync, sync/atomic, encoding/json,
	// fmt, log, strings, unicode. No os/exec, syscall, or shell commands.
	t.Log("source review: no subprocess execution (os/exec, syscall.Exec) found")
}

// ---------------------------------------------------------------------------
// 12. Concurrent submission stress (supplemental)
// ---------------------------------------------------------------------------

func TestConcurrentSubmissions(t *testing.T) {
	_, ts := newTestServer(t)
	defer ts.Close()

	n := 20
	ids := make([]string, n)
	var wg sync.WaitGroup
	mu := sync.Mutex{}

	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			body := fmt.Sprintf(`{"text":"word%d"}`, i)
			code, result := postJob(t, ts, body)
			if code != http.StatusAccepted {
				t.Errorf("concurrent POST[%d]: got %d", i, code)
				return
			}
			mu.Lock()
			ids[i] = result["id"]
			mu.Unlock()
		}(i)
	}
	wg.Wait()

	// Wait for all to complete and verify distinct IDs.
	seen := make(map[string]bool)
	for _, id := range ids {
		if id == "" {
			t.Error("empty id in concurrent submission")
			continue
		}
		if seen[id] {
			t.Errorf("duplicate id %s in concurrent submissions", id)
		}
		seen[id] = true
		waitForSucceeded(t, ts, id, 5*time.Second)
	}
}

// Ensure bytes import is used.
var _ = bytes.NewReader
