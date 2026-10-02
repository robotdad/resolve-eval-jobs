// Package main implements a local word-count job service.
//
// The service exposes a loopback-only JSON HTTP API for submitting and
// querying word-count jobs. Jobs are stored in memory and processed by a
// single in-process FIFO worker goroutine. The service is intentionally
// ephemeral: all state is lost on restart.
//
// Endpoints:
//
//	POST /jobs          – submit a new word-count job
//	GET  /jobs          – list all jobs in submission order
//	GET  /jobs/{id}     – retrieve a single job by ID
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
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
func (srv *server) handleList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	jobs := srv.store.list()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(jobs)
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
