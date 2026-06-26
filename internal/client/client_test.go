package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
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

func TestFetchIncludesIndexedTopicFilters(t *testing.T) {
	var got Query
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	c := New(server.URL)
	defer c.Close()
	toBlock := uint64(20)
	filters := []LogFilter{{
		Topic0: []string{"0xtopic0"},
		Topic1: []string{"0xtopic1"},
		Topic2: []string{"0xtopic2"},
		Topic3: []string{"0xtopic3"},
	}}
	if _, err := c.FetchRaw(context.Background(), 10, &toBlock, filters); err != nil {
		t.Fatal(err)
	}
	if len(got.Logs) != 1 {
		t.Fatalf("logs = %#v, want one filter", got.Logs)
	}
	if got.Logs[0].Topic1[0] != "0xtopic1" ||
		got.Logs[0].Topic2[0] != "0xtopic2" ||
		got.Logs[0].Topic3[0] != "0xtopic3" {
		t.Fatalf("indexed topics were not preserved: %#v", got.Logs[0])
	}
}

func TestFetchWithParentIncludesForkCursorAndAllBlocks(t *testing.T) {
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
	if _, err := c.FetchWithParent(context.Background(), 11, nil, "0xparent", true, nil); err != nil {
		t.Fatal(err)
	}
	if got["parentBlockHash"] != "0xparent" {
		t.Fatalf("parentBlockHash = %#v, want 0xparent", got["parentBlockHash"])
	}
	if got["includeAllBlocks"] != true {
		t.Fatalf("includeAllBlocks = %#v, want true", got["includeAllBlocks"])
	}
}

func TestFetchParsesForkConflict(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"previousBlocks":[{"number":10,"hash":"0x10"},{"number":11,"hash":"0x11a"}]}`))
	}))
	defer server.Close()

	c := New(server.URL)
	defer c.Close()
	_, err := c.FetchWithParent(context.Background(), 12, nil, "0x11", true, nil)
	var fork *ForkError
	if !errors.As(err, &fork) {
		t.Fatalf("err = %v, want ForkError", err)
	}
	if len(fork.PreviousBlocks) != 2 || fork.PreviousBlocks[1].Number != 11 || fork.PreviousBlocks[1].Hash != "0x11a" {
		t.Fatalf("previous blocks = %#v", fork.PreviousBlocks)
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

func TestFetchExposesHeadHeadersOnNoContent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Sqd-Finalized-Head-Number", "87085859")
		w.Header().Set("X-Sqd-Finalized-Head-Hash", "0xcd047368a59f4b6aaa386107e79e69b6dcefcffc25d91a6faf94337d103bf8db")
		w.Header().Set("X-Sqd-Head-Number", "87089173")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	c := New(server.URL)
	defer c.Close()
	response, err := c.Fetch(context.Background(), 10, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if response.Raw != nil {
		t.Fatalf("raw = %q, want nil", response.Raw)
	}
	if response.Head.Finalized == nil {
		t.Fatal("finalized head is nil")
	}
	if response.Head.Finalized.Number != 87085859 {
		t.Fatalf("finalized number = %d, want 87085859", response.Head.Finalized.Number)
	}
	if response.Head.Finalized.Hash != "0xcd047368a59f4b6aaa386107e79e69b6dcefcffc25d91a6faf94337d103bf8db" {
		t.Fatalf("finalized hash = %q", response.Head.Finalized.Hash)
	}
	if response.Head.Latest == nil || response.Head.Latest.Number != 87089173 {
		t.Fatalf("latest head = %#v, want number 87089173", response.Head.Latest)
	}
}

func TestParseHeadFromStoredPolygonFinalizedStreamHeaderExample(t *testing.T) {
	data, err := os.ReadFile("testdata/polygon_finalized_stream_headers.txt")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasSuffix(data, []byte("\n\n")) {
		data = append(data, '\n')
	}
	mimeHeader, err := textproto.NewReader(bufio.NewReader(bytes.NewReader(data))).ReadMIMEHeader()
	if err != nil {
		t.Fatal(err)
	}

	head := parseHeadFromHeaders(http.Header(mimeHeader))

	if head.Finalized == nil {
		t.Fatal("finalized head is nil")
	}
	if head.Finalized.Number != 87085859 {
		t.Fatalf("finalized number = %d, want 87085859", head.Finalized.Number)
	}
	if head.Finalized.Hash != "0xcd047368a59f4b6aaa386107e79e69b6dcefcffc25d91a6faf94337d103bf8db" {
		t.Fatalf("finalized hash = %q", head.Finalized.Hash)
	}
	if head.Latest == nil || head.Latest.Number != 87089173 {
		t.Fatalf("latest head = %#v, want number 87089173", head.Latest)
	}
}
