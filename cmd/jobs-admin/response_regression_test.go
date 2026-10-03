package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRejectNullJobArray(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("null\n"))
	}))
	defer srv.Close()
	for _, jsonOut := range []bool{false, true} {
		if err := run(srv.URL, jsonOut); err == nil || !strings.Contains(err.Error(), "expected a JSON array") {
			t.Fatalf("json=%v: expected null-array error, got %v", jsonOut, err)
		}
	}
}
