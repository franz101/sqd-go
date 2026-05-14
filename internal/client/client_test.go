package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFetchRawOmitsToBlockInCursorMode(t *testing.T) {
	var got map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"header":{"number":1,"hash":"0x1","timestamp":1},"logs":[]}` + "\n"))
	}))
	defer server.Close()

	c := New(server.URL)
	defer c.Close()
	if _, err := c.FetchRaw(context.Background(), 10, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := got["toBlock"]; ok {
		t.Fatalf("request included toBlock in cursor mode: %#v", got)
	}
	if got["includeAllBlocks"] != false {
		t.Fatalf("includeAllBlocks = %#v, want false", got["includeAllBlocks"])
	}
}

func TestFetchRawIncludesBoundedToBlock(t *testing.T) {
	var got map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	c := New(server.URL)
	defer c.Close()
	toBlock := uint64(20)
	if _, err := c.FetchRaw(context.Background(), 10, &toBlock, nil); err != nil {
		t.Fatal(err)
	}
	if got["toBlock"] != float64(20) {
		t.Fatalf("toBlock = %#v, want 20", got["toBlock"])
	}
}

func TestFetchRawNoContentIsEmpty(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	c := New(server.URL)
	defer c.Close()
	raw, err := c.FetchRaw(context.Background(), 10, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if raw != nil {
		t.Fatalf("raw = %q, want nil", raw)
	}
}
