package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestRejectTrailingJSONWithoutMutation(t *testing.T) {
	srv := &server{store: newStore(), queue: make(chan *Job, 10)}
	for _, body := range []string{`{"text":"hi"}garbage`, `{"text":"hi"}{"text":"extra"}`, `{"text":"hi"}null`} {
		w := httptest.NewRecorder()
		srv.handleSubmit(w, httptest.NewRequest(http.MethodPost, "/jobs", strings.NewReader(body)))
		if w.Code != http.StatusBadRequest || len(srv.store.list()) != 0 || len(srv.queue) != 0 {
			t.Fatalf("invalid body %q: status %d, inventory %d, queue %d", body, w.Code, len(srv.store.list()), len(srv.queue))
		}
	}
	w := httptest.NewRecorder()
	srv.handleSubmit(w, httptest.NewRequest(http.MethodPost, "/jobs", strings.NewReader("{\"text\":\"hi\"} \t\n")))
	if w.Code != http.StatusAccepted || len(srv.store.list()) != 1 {
		t.Fatalf("valid trailing whitespace: status %d", w.Code)
	}
}

func TestListSnapshotDoesNotShareMutableJobs(t *testing.T) {
	s := newStore()
	job := &Job{ID: "1", Text: "hello", Status: StatusSucceeded, Result: &Result{WordCount: 1}}
	s.add(job)
	snapshot := s.list()
	s.mu.Lock()
	job.Status = StatusRunning
	job.Result.WordCount = 2
	s.mu.Unlock()
	if snapshot[0].Status != StatusSucceeded || snapshot[0].Result.WordCount != 1 {
		t.Fatal("list snapshot changed with stored job")
	}
}

func TestConcurrentListWhileProcessing(t *testing.T) {
	srv := newServer()
	defer close(srv.queue)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			w := httptest.NewRecorder()
			srv.handleSubmit(w, httptest.NewRequest(http.MethodPost, "/jobs", strings.NewReader(`{"text":"one two three"}`)))
			if w.Code != http.StatusAccepted {
				t.Errorf("submit status %d", w.Code)
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			w := httptest.NewRecorder()
			srv.handleList(w, httptest.NewRequest(http.MethodGet, "/jobs", nil))
			if w.Code != http.StatusOK {
				t.Errorf("list status %d", w.Code)
			}
		}
	}()
	wg.Wait()
}
