// Package main pagination acceptance tests.
//
// These tests verify the opt-in cursor pagination feature added to GET /jobs.
// They use the frozen independent oracle sequence from the goal condition:
//
//	["4","5","3","8","11","2","7","10","1","9","6","12"]
//
// The fixture is submitted in the order specified by the goal document and all
// jobs are allowed to complete before any pagination traversal begins.
//
// Ordering: text ascending (Go bytewise lexicographic), then numeric ID ascending.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Pagination helpers
// ---------------------------------------------------------------------------

// pageEnvelopeRaw is used for decoding paginated GET /jobs responses.
type pageEnvelopeRaw struct {
	Jobs       []map[string]interface{} `json:"jobs"`
	NextCursor *string                  `json:"next_cursor"`
}

// paginatedGet performs GET /jobs?limit=N[&cursor=C] and returns the envelope.
func paginatedGet(t *testing.T, ts *httptest.Server, limit int, cursor string) pageEnvelopeRaw {
	t.Helper()
	u := fmt.Sprintf("%s/jobs?limit=%d", ts.URL, limit)
	if cursor != "" {
		u += "&cursor=" + cursor
	}
	resp, err := http.Get(u)
	if err != nil {
		t.Fatalf("GET %s: %v", u, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET %s: status %d body: %s", u, resp.StatusCode, body)
	}
	var env pageEnvelopeRaw
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode paginated response: %v", err)
	}
	return env
}

// traverseAllPages collects all job IDs by following next_cursor until null.
// It also validates page-size bounds and returns each page for further checks.
func traverseAllPages(t *testing.T, ts *httptest.Server, limit int) ([]string, []pageEnvelopeRaw) {
	t.Helper()
	var allIDs []string
	var pages []pageEnvelopeRaw
	cursor := ""
	for {
		env := paginatedGet(t, ts, limit, cursor)
		pages = append(pages, env)

		// Page size must not exceed limit.
		if len(env.Jobs) > limit {
			t.Errorf("page size %d exceeds limit %d", len(env.Jobs), limit)
		}

		for _, j := range env.Jobs {
			id, _ := j["id"].(string)
			allIDs = append(allIDs, id)
		}

		if env.NextCursor == nil {
			break
		}
		cursor = *env.NextCursor
	}
	return allIDs, pages
}

// buildOracleFixture submits the 12 frozen oracle jobs and waits for all to complete.
// Returns the base URL of the test server.
func buildOracleFixture(t *testing.T) (*httptest.Server, *server) {
	t.Helper()
	srv, ts := newTestServer(t)

	// Frozen fixture from goal condition, submitted in this exact order:
	// ID "1" text "z"; ID "2" text "tie"; ID "3" text "a"; ID "4" text "";
	// ID "5" text "A"; ID "6" text "é"; ID "7" text "tie"; ID "8" text "a";
	// ID "9" text "z"; ID "10" text "tie"; ID "11" text "beta"; ID "12" text "空"
	texts := []string{"z", "tie", "a", "", "A", "é", "tie", "a", "z", "tie", "beta", "空"}

	ids := make([]string, len(texts))
	for i, text := range texts {
		body, _ := json.Marshal(map[string]string{"text": text})
		code, result := postJob(t, ts, string(body))
		if code != http.StatusAccepted {
			t.Fatalf("oracle fixture job[%d] text=%q: POST /jobs got %d", i, text, code)
		}
		ids[i] = result["id"]
		// Verify sequential IDs.
		expected := fmt.Sprintf("%d", i+1)
		if ids[i] != expected {
			t.Fatalf("oracle fixture job[%d]: expected id %s, got %s", i, expected, ids[i])
		}
	}

	// Wait for all jobs to complete before pagination (no concurrent insertion).
	for i, id := range ids {
		waitForSucceeded(t, ts, id, 5*time.Second)
		_ = i
	}

	return ts, srv
}

// ---------------------------------------------------------------------------
// Oracle sequence validation
// ---------------------------------------------------------------------------

// oracleSequence is the literal expected concatenated paged ID sequence.
// This is an independent oracle, not computed by the production comparator.
var oracleSequence = []string{"4", "5", "3", "8", "11", "2", "7", "10", "1", "9", "6", "12"}

// TestPaginationOracleLimit1 traverses the 12-job oracle fixture with limit=1.
func TestPaginationOracleLimit1(t *testing.T) {
	ts, _ := buildOracleFixture(t)
	defer ts.Close()

	allIDs, pages := traverseAllPages(t, ts, 1)

	// Each page should have exactly 1 job (except possibly the last).
	for i, page := range pages {
		if i < len(pages)-1 && len(page.Jobs) != 1 {
			t.Errorf("page[%d] with limit=1: got %d jobs, want 1", i, len(page.Jobs))
		}
	}

	// Verify oracle sequence.
	if len(allIDs) != len(oracleSequence) {
		t.Fatalf("limit=1: collected %d IDs, want %d; got %v", len(allIDs), len(oracleSequence), allIDs)
	}
	for i, want := range oracleSequence {
		if allIDs[i] != want {
			t.Errorf("limit=1: position %d: got ID %s, want %s (full: %v)", i, allIDs[i], want, allIDs)
		}
	}

	// Last page must have next_cursor = null.
	if pages[len(pages)-1].NextCursor != nil {
		t.Error("limit=1: last page next_cursor is not null")
	}

	t.Logf("limit=1: %d pages, IDs=%v", len(pages), allIDs)
}

// TestPaginationOracleLimit3 traverses the 12-job oracle fixture with limit=3.
func TestPaginationOracleLimit3(t *testing.T) {
	ts, _ := buildOracleFixture(t)
	defer ts.Close()

	allIDs, pages := traverseAllPages(t, ts, 3)

	// Verify oracle sequence.
	if len(allIDs) != len(oracleSequence) {
		t.Fatalf("limit=3: collected %d IDs, want %d; got %v", len(allIDs), len(oracleSequence), allIDs)
	}
	for i, want := range oracleSequence {
		if allIDs[i] != want {
			t.Errorf("limit=3: position %d: got ID %s, want %s (full: %v)", i, allIDs[i], want, allIDs)
		}
	}

	// Last page must have next_cursor = null.
	if pages[len(pages)-1].NextCursor != nil {
		t.Error("limit=3: last page next_cursor is not null")
	}

	t.Logf("limit=3: %d pages, IDs=%v", len(pages), allIDs)
}

// TestPaginationOracleLimit10 traverses the 12-job oracle fixture with limit=10.
func TestPaginationOracleLimit10(t *testing.T) {
	ts, _ := buildOracleFixture(t)
	defer ts.Close()

	allIDs, pages := traverseAllPages(t, ts, 10)

	// Verify oracle sequence.
	if len(allIDs) != len(oracleSequence) {
		t.Fatalf("limit=10: collected %d IDs, want %d; got %v", len(allIDs), len(oracleSequence), allIDs)
	}
	for i, want := range oracleSequence {
		if allIDs[i] != want {
			t.Errorf("limit=10: position %d: got ID %s, want %s (full: %v)", i, allIDs[i], want, allIDs)
		}
	}

	// Last page must have next_cursor = null.
	if pages[len(pages)-1].NextCursor != nil {
		t.Error("limit=10: last page next_cursor is not null")
	}

	t.Logf("limit=10: %d pages, IDs=%v", len(pages), allIDs)
}

// TestPaginationOracleFullJobData verifies the full Job data from the completed
// reference snapshot matches what pagination returns.
func TestPaginationOracleFullJobData(t *testing.T) {
	ts, _ := buildOracleFixture(t)
	defer ts.Close()

	// Collect all paginated jobs (limit=3 as representative).
	allIDs, _ := traverseAllPages(t, ts, 3)

	// Also fetch each job individually to compare full data.
	for i, id := range allIDs {
		code, job := getJob(t, ts, id)
		if code != http.StatusOK {
			t.Fatalf("oracle position %d ID %s: GET /jobs/%s returned %d", i, id, id, code)
		}
		if job["id"] != id {
			t.Errorf("oracle position %d: paginated id=%s but GET /jobs/%s returned id=%s", i, id, id, job["id"])
		}
		if job["status"] != StatusSucceeded {
			t.Errorf("oracle position %d ID %s: status=%v, want succeeded", i, id, job["status"])
		}
		if job["result"] == nil {
			t.Errorf("oracle position %d ID %s: result is nil", i, id)
		}
	}
}

// TestPaginationStrictCursorBoundaryTies verifies strict cursor boundaries
// across the tied IDs 2, 7, and 10 (all text "tie").
func TestPaginationStrictCursorBoundaryTies(t *testing.T) {
	ts, _ := buildOracleFixture(t)
	defer ts.Close()

	// With limit=1, each page has exactly one item. We collect all pages and
	// verify that the tied IDs appear in the right order (2, 7, 10) with no
	// duplicates and no gaps.
	allIDs, pages := traverseAllPages(t, ts, 1)

	// Find positions of tie IDs.
	tiePositions := map[string]int{}
	for i, id := range allIDs {
		if id == "2" || id == "7" || id == "10" {
			tiePositions[id] = i
		}
	}

	// All three must appear.
	for _, id := range []string{"2", "7", "10"} {
		if _, ok := tiePositions[id]; !ok {
			t.Errorf("tie ID %s not found in paginated output", id)
		}
	}

	// They must appear in order: 2 before 7 before 10.
	if tiePositions["2"] >= tiePositions["7"] {
		t.Errorf("tie order: ID 2 (pos %d) should precede ID 7 (pos %d)", tiePositions["2"], tiePositions["7"])
	}
	if tiePositions["7"] >= tiePositions["10"] {
		t.Errorf("tie order: ID 7 (pos %d) should precede ID 10 (pos %d)", tiePositions["7"], tiePositions["10"])
	}

	// Verify no duplicates.
	seen := map[string]int{}
	for i, id := range allIDs {
		if prev, ok := seen[id]; ok {
			t.Errorf("duplicate ID %s at positions %d and %d", id, prev, i)
		}
		seen[id] = i
	}

	// Each page with limit=1 must have exactly 1 job and a non-nil cursor
	// (except the last page).
	for i, page := range pages {
		if len(page.Jobs) != 1 {
			t.Errorf("page[%d] limit=1: got %d jobs, want 1", i, len(page.Jobs))
		}
		if i < len(pages)-1 && page.NextCursor == nil {
			t.Errorf("page[%d] limit=1: next_cursor is nil but not last page", i)
		}
	}

	t.Logf("tie boundary: ID 2 at pos %d, ID 7 at pos %d, ID 10 at pos %d",
		tiePositions["2"], tiePositions["7"], tiePositions["10"])
}

// ---------------------------------------------------------------------------
// Empty and single-job dataset tests
// ---------------------------------------------------------------------------

// TestPaginationEmptyDataset verifies that an empty server returns [] and null cursor.
func TestPaginationEmptyDataset(t *testing.T) {
	_, ts := newTestServer(t)
	defer ts.Close()

	env := paginatedGet(t, ts, 10, "")
	if env.Jobs == nil || len(env.Jobs) != 0 {
		t.Errorf("empty dataset: jobs=%v, want []", env.Jobs)
	}
	if env.NextCursor != nil {
		t.Errorf("empty dataset: next_cursor=%v, want null", *env.NextCursor)
	}
}

// TestPaginationSingleJobDataset verifies single-job pagination.
func TestPaginationSingleJobDataset(t *testing.T) {
	_, ts := newTestServer(t)
	defer ts.Close()

	code, result := postJob(t, ts, `{"text":"solo"}`)
	if code != http.StatusAccepted {
		t.Fatalf("POST /jobs: got %d", code)
	}
	id := result["id"]
	waitForSucceeded(t, ts, id, 5*time.Second)

	env := paginatedGet(t, ts, 10, "")
	if len(env.Jobs) != 1 {
		t.Fatalf("single job: got %d jobs, want 1", len(env.Jobs))
	}
	if env.Jobs[0]["id"] != id {
		t.Errorf("single job: got id=%v, want %s", env.Jobs[0]["id"], id)
	}
	if env.NextCursor != nil {
		t.Errorf("single job: next_cursor=%v, want null", *env.NextCursor)
	}
}

// ---------------------------------------------------------------------------
// Invalid request tests
// ---------------------------------------------------------------------------

// getJobsRaw performs a raw GET /jobs with the given query string and returns
// the status code and body.
func getJobsRaw(t *testing.T, ts *httptest.Server, query string) (int, string) {
	t.Helper()
	u := ts.URL + "/jobs"
	if query != "" {
		u += "?" + query
	}
	resp, err := http.Get(u)
	if err != nil {
		t.Fatalf("GET %s: %v", u, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, strings.TrimSpace(string(body))
}

// TestPaginationInvalidLimitMalformed verifies that malformed limit values return 400.
func TestPaginationInvalidLimitMalformed(t *testing.T) {
	_, ts := newTestServer(t)
	defer ts.Close()

	cases := []struct {
		name  string
		query string
	}{
		{"not a number", "limit=abc"},
		{"float", "limit=1.5"},
		{"empty string", "limit="},
		{"space", "limit= "},
		{"plus sign", "limit=+1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, body := getJobsRaw(t, ts, tc.query)
			if code != http.StatusBadRequest {
				t.Errorf("limit=%q: got %d, want 400; body: %s", tc.query, code, body)
			}
		})
	}
}

// TestPaginationInvalidLimitZeroNegative verifies zero and negative limits return 400.
func TestPaginationInvalidLimitZeroNegative(t *testing.T) {
	_, ts := newTestServer(t)
	defer ts.Close()

	for _, limit := range []string{"0", "-1", "-100"} {
		t.Run("limit="+limit, func(t *testing.T) {
			code, body := getJobsRaw(t, ts, "limit="+limit)
			if code != http.StatusBadRequest {
				t.Errorf("limit=%s: got %d, want 400; body: %s", limit, code, body)
			}
		})
	}
}

// TestPaginationInvalidLimitOverflow verifies overflowing limit values return 400.
func TestPaginationInvalidLimitOverflow(t *testing.T) {
	_, ts := newTestServer(t)
	defer ts.Close()

	// Above cap (paginationMaxLimit = 100).
	for _, limit := range []string{"101", "1000", "99999999999999999999"} {
		t.Run("limit="+limit, func(t *testing.T) {
			code, body := getJobsRaw(t, ts, "limit="+limit)
			if code != http.StatusBadRequest {
				t.Errorf("limit=%s: got %d, want 400; body: %s", limit, code, body)
			}
		})
	}
}

// TestPaginationCursorWithoutLimit verifies cursor without limit returns 400.
func TestPaginationCursorWithoutLimit(t *testing.T) {
	_, ts := newTestServer(t)
	defer ts.Close()

	code, body := getJobsRaw(t, ts, "cursor=somecursor")
	if code != http.StatusBadRequest {
		t.Errorf("cursor without limit: got %d, want 400; body: %s", code, body)
	}
}

// TestPaginationMalformedCursor verifies malformed cursor values return 400.
func TestPaginationMalformedCursor(t *testing.T) {
	_, ts := newTestServer(t)
	defer ts.Close()

	// Submit a job so there's something to paginate.
	code, result := postJob(t, ts, `{"text":"hello"}`)
	if code != http.StatusAccepted {
		t.Fatalf("POST /jobs: got %d", code)
	}
	waitForSucceeded(t, ts, result["id"], 5*time.Second)

	cases := []struct {
		name   string
		cursor string
	}{
		{"random string", "notbase64!!!"},
		{"valid base64 invalid JSON", "aW52YWxpZA"}, // "invalid" in base64
		{"empty cursor", ""},
		{"wrong shape", "eyJ4IjoieSJ9"}, // {"x":"y"} - unknown fields
		{"negative id", "eyJ0IjoiYSIsImkiOi0xfQ"}, // {"t":"a","i":-1}
		{"zero id", "eyJ0IjoiYSIsImkiOjB9"},         // {"t":"a","i":0}
	}
	for _, tc := range cases {
		if tc.cursor == "" {
			continue // empty cursor is the "no cursor" case, tested separately
		}
		t.Run(tc.name, func(t *testing.T) {
			code, body := getJobsRaw(t, ts, fmt.Sprintf("limit=10&cursor=%s", tc.cursor))
			if code != http.StatusBadRequest {
				t.Errorf("cursor=%q: got %d, want 400; body: %s", tc.cursor, code, body)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Read-only behavior and inventory preservation
// ---------------------------------------------------------------------------

// TestPaginationReadOnly verifies that pagination requests do not create or
// mutate jobs. Compares full legacy inventory before and after traversal.
func TestPaginationReadOnly(t *testing.T) {
	ts, _ := buildOracleFixture(t)
	defer ts.Close()

	// Snapshot the legacy (non-paginated) inventory before traversal.
	beforeList := listJobs(t, ts)

	// Perform traversals with all three oracle limits.
	for _, limit := range []int{1, 3, 10} {
		traverseAllPages(t, ts, limit)
	}

	// Also exercise invalid requests.
	getJobsRaw(t, ts, "cursor=badcursor")
	getJobsRaw(t, ts, "limit=0")
	getJobsRaw(t, ts, "limit=abc")

	// Snapshot legacy inventory after traversal.
	afterList := listJobs(t, ts)

	if len(afterList) != len(beforeList) {
		t.Fatalf("inventory changed: before=%d after=%d", len(beforeList), len(afterList))
	}
	for i := range beforeList {
		if beforeList[i]["id"] != afterList[i]["id"] {
			t.Errorf("job[%d] id changed: before=%v after=%v", i, beforeList[i]["id"], afterList[i]["id"])
		}
		if beforeList[i]["status"] != afterList[i]["status"] {
			t.Errorf("job[%d] status changed: before=%v after=%v", i, beforeList[i]["status"], afterList[i]["status"])
		}
	}
}

// TestPaginationLegacyInventoryOrder verifies that GET /jobs without limit
// still returns jobs in submission order (IDs 1-12) after the oracle fixture.
func TestPaginationLegacyInventoryOrder(t *testing.T) {
	ts, _ := buildOracleFixture(t)
	defer ts.Close()

	jobs := listJobs(t, ts)
	if len(jobs) != 12 {
		t.Fatalf("legacy GET /jobs: got %d jobs, want 12", len(jobs))
	}
	for i, job := range jobs {
		expectedID := fmt.Sprintf("%d", i+1)
		if job["id"] != expectedID {
			t.Errorf("legacy order: jobs[%d].id = %v, want %s", i, job["id"], expectedID)
		}
	}
}

// TestPaginationRepeatableAfterInvalidRequests verifies that a successful
// traversal after invalid requests still produces the oracle sequence.
func TestPaginationRepeatableAfterInvalidRequests(t *testing.T) {
	ts, _ := buildOracleFixture(t)
	defer ts.Close()

	// Fire invalid requests.
	getJobsRaw(t, ts, "cursor=notvalid")
	getJobsRaw(t, ts, "limit=-5")
	getJobsRaw(t, ts, "limit=9999")

	// Now do a successful traversal.
	allIDs, _ := traverseAllPages(t, ts, 3)

	if len(allIDs) != len(oracleSequence) {
		t.Fatalf("after invalid requests: collected %d IDs, want %d", len(allIDs), len(oracleSequence))
	}
	for i, want := range oracleSequence {
		if allIDs[i] != want {
			t.Errorf("after invalid requests: position %d: got %s, want %s", i, allIDs[i], want)
		}
	}
}

// TestPaginationStableRepeatedTraversals verifies that repeated traversals
// produce the same oracle sequence (stable results).
func TestPaginationStableRepeatedTraversals(t *testing.T) {
	ts, _ := buildOracleFixture(t)
	defer ts.Close()

	for rep := 0; rep < 3; rep++ {
		allIDs, _ := traverseAllPages(t, ts, 3)
		if len(allIDs) != len(oracleSequence) {
			t.Fatalf("rep %d: collected %d IDs, want %d", rep, len(allIDs), len(oracleSequence))
		}
		for i, want := range oracleSequence {
			if allIDs[i] != want {
				t.Errorf("rep %d: position %d: got %s, want %s", rep, i, allIDs[i], want)
			}
		}
	}
}

// TestPaginationEnvelopeShape verifies the response is a JSON object with
// "jobs" array and "next_cursor" field (not a bare array).
func TestPaginationEnvelopeShape(t *testing.T) {
	_, ts := newTestServer(t)
	defer ts.Close()

	// Submit one job.
	code, result := postJob(t, ts, `{"text":"shape test"}`)
	if code != http.StatusAccepted {
		t.Fatalf("POST /jobs: got %d", code)
	}
	waitForSucceeded(t, ts, result["id"], 5*time.Second)

	// Raw decode to verify shape.
	resp, err := http.Get(ts.URL + "/jobs?limit=10")
	if err != nil {
		t.Fatalf("GET /jobs?limit=10: %v", err)
	}
	defer resp.Body.Close()

	var raw map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// Must have "jobs" key.
	if _, ok := raw["jobs"]; !ok {
		t.Error("paginated response missing 'jobs' key")
	}
	// Must have "next_cursor" key (even if null).
	if _, ok := raw["next_cursor"]; !ok {
		t.Error("paginated response missing 'next_cursor' key")
	}
	// "jobs" must be an array.
	jobs, ok := raw["jobs"].([]interface{})
	if !ok {
		t.Errorf("'jobs' field is not an array: %T", raw["jobs"])
	}
	if len(jobs) != 1 {
		t.Errorf("'jobs' length: got %d, want 1", len(jobs))
	}
}

// TestPaginationValidLimitBoundaries verifies that limits 1, 3, and 10 are accepted
// and that the maximum cap (100) is accepted while 101 is rejected.
func TestPaginationValidLimitBoundaries(t *testing.T) {
	_, ts := newTestServer(t)
	defer ts.Close()

	// Valid limits.
	for _, limit := range []int{1, 3, 10, 100} {
		t.Run(fmt.Sprintf("limit=%d", limit), func(t *testing.T) {
			code, body := getJobsRaw(t, ts, fmt.Sprintf("limit=%d", limit))
			if code != http.StatusOK {
				t.Errorf("limit=%d: got %d, want 200; body: %s", limit, code, body)
			}
		})
	}

	// Just above cap.
	t.Run("limit=101", func(t *testing.T) {
		code, body := getJobsRaw(t, ts, "limit=101")
		if code != http.StatusBadRequest {
			t.Errorf("limit=101: got %d, want 400; body: %s", code, body)
		}
	})
}

// TestPaginationRaceDetector runs concurrent paginated reads to verify no data races.
func TestPaginationRaceDetector(t *testing.T) {
	ts, _ := buildOracleFixture(t)
	defer ts.Close()

	// Run concurrent paginated reads.
	done := make(chan struct{})
	for i := 0; i < 5; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			traverseAllPages(t, ts, 3)
		}()
	}
	for i := 0; i < 5; i++ {
		<-done
	}
}
