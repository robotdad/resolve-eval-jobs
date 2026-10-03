// Package main implements a local word-count job service.
//
// The service exposes a loopback-only JSON HTTP API for submitting and
// querying word-count jobs. Jobs are stored in memory and processed by a
// single in-process FIFO worker goroutine. The service is intentionally
// ephemeral: all state is lost on restart.
//
// Endpoints:
//
//	POST /jobs              – submit a new word-count job
//	GET  /jobs              – list all jobs in submission order
//	GET  /jobs?limit=N      – paginate jobs by text asc, id asc; returns envelope
//	GET  /jobs?limit=N&cursor=C – continue pagination from opaque cursor C
//	GET  /jobs/{id}         – retrieve a single job by ID
//
// Pagination:
//
// When the "limit" query parameter is present, GET /jobs returns a JSON
// object envelope instead of a bare array:
//
//	{"jobs": [...], "next_cursor": "<opaque>" | null}
//
// Jobs are ordered by text ascending (Go bytewise lexicographic), then by
// numeric ID ascending. The "limit" parameter must be a positive decimal
// integer between 1 and 100 (inclusive). The "cursor" parameter is opaque
// and must not be supplied without "limit".
package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"unicode"
)

// Status values for a job lifecycle.
const (
	StatusQueued    = "queued"
	StatusRunning   = "running"
	StatusSucceeded = "succeeded"
)

// paginationMaxLimit is the maximum value accepted for the "limit" parameter.
const paginationMaxLimit = 100

// pageEnvelope is the response body for a paginated GET /jobs request.
type pageEnvelope struct {
	Jobs       []*Job  `json:"jobs"`
	NextCursor *string `json:"next_cursor"`
}

// cursorTuple is the decoded form of the opaque pagination cursor.
// It encodes the (text, id) tuple of the last returned job.
type cursorTuple struct {
	Text string `json:"t"`
	ID   int64  `json:"i"`
}

// encodeCursor encodes a cursorTuple into an opaque base64url string.
func encodeCursor(text string, id int64) string {
	ct := cursorTuple{Text: text, ID: id}
	b, _ := json.Marshal(ct)
	return base64.RawURLEncoding.EncodeToString(b)
}

// decodeCursor decodes an opaque cursor string into a cursorTuple.
// Returns an error if the cursor is malformed, has wrong shape/types, or
// contains an invalid (non-positive) numeric ID.
func decodeCursor(s string) (cursorTuple, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return cursorTuple{}, fmt.Errorf("invalid cursor encoding: %w", err)
	}
	var ct struct {
		Text *string `json:"t"`
		ID   int64   `json:"i"`
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&ct); err != nil {
		return cursorTuple{}, fmt.Errorf("invalid cursor shape: %w", err)
	}
	// Require exactly one JSON value.
	if err := dec.Decode(new(any)); err != io.EOF {
		return cursorTuple{}, fmt.Errorf("invalid cursor: trailing data")
	}
	if ct.ID <= 0 {
		return cursorTuple{}, fmt.Errorf("invalid cursor: id must be positive")
	}
	if ct.Text == nil {
		return cursorTuple{}, fmt.Errorf("invalid cursor: text must be a string")
	}
	return cursorTuple{Text: *ct.Text, ID: ct.ID}, nil
}

// jobSortKey returns the sort key for a job: (text, numericID).
// numericID is the integer value of Job.ID; IDs are always sequential positive
// integers so this conversion is always valid.
func jobSortKey(j *Job) (string, int64) {
	n, _ := strconv.ParseInt(j.ID, 10, 64)
	return j.Text, n
}

// sortedJobsSnapshot returns a sorted snapshot of all jobs ordered by
// (text asc, numericID asc) using Go bytewise lexicographic ordering on text.
func (s *store) sortedJobsSnapshot() []*Job {
	s.mu.RLock()
	snapshot := make([]*Job, len(s.jobs))
	for i, job := range s.jobs {
		value := *job
		if job.Result != nil {
			result := *job.Result
			value.Result = &result
		}
		snapshot[i] = &value
	}
	s.mu.RUnlock()

	sort.SliceStable(snapshot, func(i, j int) bool {
		ti, ni := jobSortKey(snapshot[i])
		tj, nj := jobSortKey(snapshot[j])
		if ti != tj {
			return ti < tj
		}
		return ni < nj
	})
	return snapshot
}

// Job represents a single word-count job.
type Job struct {
	ID     string  `json:"id"`
	Status string  `json:"status"`
	Text   string  `json:"text"`
	Result *Result `json:"result,omitempty"`
}

// Result holds the outcome of a completed job.
type Result struct {
	WordCount int `json:"word_count"`
}

// submitRequest is the expected JSON body for POST /jobs.
type submitRequest struct {
	Text *string `json:"text"`
}

// store holds all jobs in submission order, protected by a mutex.
type store struct {
	mu   sync.RWMutex
	jobs []*Job
	byID map[string]*Job
}

func newStore() *store {
	return &store{byID: make(map[string]*Job)}
}

// add appends a new job to the store and returns it.
func (s *store) add(job *Job) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.jobs = append(s.jobs, job)
	s.byID[job.ID] = job
}

// get returns the job with the given ID, or nil if not found.
func (s *store) get(id string) *Job {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.byID[id]
}

// list returns a snapshot of all jobs in submission order.
func (s *store) list() []*Job {
	s.mu.RLock()
	defer s.mu.RUnlock()
	snapshot := make([]*Job, len(s.jobs))
	for i, job := range s.jobs {
		value := *job
		if job.Result != nil {
			result := *job.Result
			value.Result = &result
		}
		snapshot[i] = &value
	}
	return snapshot
}

// server wires together the store, ID counter, and job queue.
type server struct {
	store   *store
	counter atomic.Int64
	queue   chan *Job
}

func newServer() *server {
	srv := &server{
		store: newStore(),
		queue: make(chan *Job, 1024),
	}
	go srv.worker()
	return srv
}

// nextID returns the next sequential, process-scoped job ID (1-based).
func (srv *server) nextID() string {
	n := srv.counter.Add(1)
	return fmt.Sprintf("%d", n)
}

// worker is the single in-process FIFO goroutine that processes jobs.
func (srv *server) worker() {
	for job := range srv.queue {
		// Transition: queued -> running
		srv.store.mu.Lock()
		job.Status = StatusRunning
		srv.store.mu.Unlock()

		// Perform word count.
		count := countWords(job.Text)

		// Transition: running -> succeeded
		srv.store.mu.Lock()
		job.Status = StatusSucceeded
		job.Result = &Result{WordCount: count}
		srv.store.mu.Unlock()
	}
}

// countWords counts nonempty runs of non-whitespace runes, where whitespace
// is defined by unicode.IsSpace (covers space, tab, newline, U+00A0, etc.).
// Punctuation is retained within words.
func countWords(text string) int {
	count := 0
	inWord := false
	for _, r := range text {
		if unicode.IsSpace(r) {
			inWord = false
		} else {
			if !inWord {
				count++
				inWord = true
			}
		}
	}
	return count
}

// handleSubmit handles POST /jobs.
func (srv *server) handleSubmit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Decode and validate the request body.
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	var req submitRequest
	if err := dec.Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.Text == nil {
		http.Error(w, "bad request: missing text field", http.StatusBadRequest)
		return
	}
	// Require exactly one JSON value, allowing only trailing whitespace.
	if err := dec.Decode(new(any)); err != io.EOF {
		http.Error(w, "bad request: expected one JSON object", http.StatusBadRequest)
		return
	}

	// Create and enqueue the job.
	job := &Job{
		ID:     srv.nextID(),
		Status: StatusQueued,
		Text:   *req.Text,
	}
	srv.store.add(job)
	srv.queue <- job

	// Respond with 202 Accepted.
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]string{"id": job.ID})
}

// handleList handles GET /jobs.
//
// Without "limit": returns a bare JSON array of all jobs in submission order.
//
// With "limit=N": returns a paginated JSON envelope:
//
//	{"jobs": [...], "next_cursor": "<opaque>" | null}
//
// Jobs are ordered by text asc (Go bytewise), then numeric ID asc.
// "cursor" may be provided alongside "limit" to continue from a prior page.
// "cursor" without "limit" is rejected with HTTP 400.
func (srv *server) handleList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		http.Error(w, "bad request: invalid query encoding", http.StatusBadRequest)
		return
	}
	hasLimit := q.Has("limit")
	limitStr := q.Get("limit")
	hasCursorParam := q.Has("cursor")
	cursorStr := q.Get("cursor")

	// Reject cursor without limit.
	if hasCursorParam && !hasLimit {
		http.Error(w, "bad request: cursor requires limit", http.StatusBadRequest)
		return
	}

	// Legacy (non-paginated) path: no limit parameter.
	if !hasLimit {
		jobs := srv.store.list()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(jobs)
		return
	}

	// Parse and validate limit.
	limit64, err := strconv.ParseInt(limitStr, 10, 64)
	if err != nil || limit64 <= 0 {
		http.Error(w, "bad request: limit must be a positive integer", http.StatusBadRequest)
		return
	}
	if limit64 > paginationMaxLimit {
		http.Error(w, fmt.Sprintf("bad request: limit exceeds maximum of %d", paginationMaxLimit), http.StatusBadRequest)
		return
	}
	limit := int(limit64)

	// Parse and validate cursor (if provided).
	var hasCursor bool
	var cursor cursorTuple
	if hasCursorParam {
		cursor, err = decodeCursor(cursorStr)
		if err != nil {
			http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
			return
		}
		hasCursor = true
	}

	// Get sorted snapshot.
	sorted := srv.store.sortedJobsSnapshot()

	// Apply cursor: skip all records up to and including the cursor tuple.
	start := 0
	if hasCursor {
		for start < len(sorted) {
			text, id := jobSortKey(sorted[start])
			// Strictly after: skip if (text, id) <= cursor.
			if text < cursor.Text || (text == cursor.Text && id <= cursor.ID) {
				start++
				continue
			}
			break
		}
	}

	// Slice the page.
	remaining := sorted[start:]
	var page []*Job
	if len(remaining) <= limit {
		page = remaining
	} else {
		page = remaining[:limit]
	}

	// Build next_cursor.
	var nextCursor *string
	if len(page) > 0 && len(remaining) > limit {
		last := page[len(page)-1]
		lastText, lastID := jobSortKey(last)
		encoded := encodeCursor(lastText, lastID)
		nextCursor = &encoded
	}

	// Ensure non-nil slice for JSON encoding.
	if page == nil {
		page = []*Job{}
	}

	env := pageEnvelope{
		Jobs:       page,
		NextCursor: nextCursor,
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(env)
}

// handleGetJob handles GET /jobs/{id}.
func (srv *server) handleGetJob(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// Extract ID from path: /jobs/{id}
	path := strings.TrimPrefix(r.URL.Path, "/jobs/")
	if path == "" {
		http.Error(w, "missing job id", http.StatusBadRequest)
		return
	}
	job := srv.store.get(path)
	if job == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	srv.store.mu.RLock()
	defer srv.store.mu.RUnlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(job)
}

// routes returns an http.ServeMux configured with all endpoints.
func (srv *server) routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/jobs", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			srv.handleSubmit(w, r)
		case http.MethodGet:
			srv.handleList(w, r)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
	mux.HandleFunc("/jobs/", srv.handleGetJob)
	return mux
}

func main() {
	srv := newServer()
	addr := "127.0.0.1:8080"
	log.Printf("word-count service listening on %s", addr)
	if err := http.ListenAndServe(addr, srv.routes()); err != nil {
		log.Fatalf("server error: %v", err)
	}
}
