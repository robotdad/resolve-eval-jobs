package main

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestCursorRequiresTextButAllowsEmptyText(t *testing.T) {
	for _, raw := range []string{`{"i":2}`, `{"t":null,"i":2}`} {
		if _, err := decodeCursor(base64.RawURLEncoding.EncodeToString([]byte(raw))); err == nil {
			t.Errorf("accepted invalid cursor %s", raw)
		}
	}
	got, err := decodeCursor(encodeCursor("", 4))
	if err != nil || got.Text != "" || got.ID != 4 {
		t.Fatalf("empty text cursor rejected: %+v %v", got, err)
	}
}

func TestInvalidCursorQueryDoesNotMutateInventory(t *testing.T) {
	srv := &server{store: newStore(), queue: make(chan *Job, 1)}
	srv.store.add(&Job{ID: "1", Text: "kept", Status: StatusSucceeded, Result: &Result{WordCount: 1}})
	before := srv.store.list()
	for _, target := range []string{
		"/jobs?limit=10&cursor=eyJpIjoyfQ",
		"/jobs?limit=10&cursor=eyJ0IjpudWxsLCJpIjoyfQ",
		"/jobs?limit=%ZZ",
		"/jobs?cursor=%ZZ",
		"/jobs?limit=3&cursor=%ZZ",
	} {
		w := httptest.NewRecorder()
		srv.handleList(w, httptest.NewRequest(http.MethodGet, target, nil))
		if w.Code != http.StatusBadRequest || w.Body.Len() == 0 {
			t.Errorf("%s: status=%d body=%q", target, w.Code, w.Body.String())
		}
		if !reflect.DeepEqual(before, srv.store.list()) {
			t.Fatalf("%s mutated inventory", target)
		}
	}
}
