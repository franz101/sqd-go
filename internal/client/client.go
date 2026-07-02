package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
)

const zstdEncoding = "zstd"

type LogFilter struct {
	Address []string `json:"address,omitempty"`
	Topic0  []string `json:"topic0,omitempty"`
	Topic1  []string `json:"topic1,omitempty"`
	Topic2  []string `json:"topic2,omitempty"`
	Topic3  []string `json:"topic3,omitempty"`
}

type Query struct {
	Type             string      `json:"type"`
	FromBlock        uint64      `json:"fromBlock"`
	ToBlock          *uint64     `json:"toBlock,omitempty"`
	ParentBlockHash  string      `json:"parentBlockHash,omitempty"`
	IncludeAllBlocks bool        `json:"includeAllBlocks"`
	Logs             []LogFilter `json:"logs,omitempty"`
	Fields           Fields      `json:"fields,omitempty"`
}

type Fields map[string]map[string]bool

type BlockRef struct {
	Number uint64
	Hash   string
}

type Head struct {
	Finalized *BlockRef
	Latest    *BlockRef
}

type Response struct {
	Raw  []byte
	Head Head
	// Profiling fields for granular timing
	NetworkDuration time.Duration
	ZstdDuration    time.Duration
}

type ForkError struct {
	PreviousBlocks []BlockRef
}

func (e *ForkError) Error() string {
	if len(e.PreviousBlocks) == 0 {
		return "sqd fork detected"
	}
	last := e.PreviousBlocks[len(e.PreviousBlocks)-1]
	return fmt.Sprintf("sqd fork detected, latest portal block sample is %d/%s", last.Number, last.Hash)
}

// RateLimitError is returned when the portal responds 429 Too Many Requests.
// RetryAfter carries the server's requested back-off (0 if the Retry-After
// header was absent or unparseable). The fetch loop honors it verbatim and,
// unlike a generic fetch error, must NOT shrink the adaptive page size — the
// request was well-formed, it was only throttled.
type RateLimitError struct {
	RetryAfter time.Duration
}

func (e *RateLimitError) Error() string {
	if e.RetryAfter > 0 {
		return fmt.Sprintf("sqd rate limited (429), retry after %s", e.RetryAfter)
	}
	return "sqd rate limited (429)"
}

// parseRetryAfter reads the Retry-After header, which is either an integer
// number of seconds or an HTTP-date. Returns 0 when absent or unparseable.
func parseRetryAfter(h http.Header) time.Duration {
	v := strings.TrimSpace(h.Get("Retry-After"))
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs < 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

func DefaultEVMFields() Fields {
	return Fields{
		"block": {
			"number": true, "timestamp": true, "hash": true,
		},
		"log": {
			"address": true, "topics": true, "data": true,
			"transactionIndex": true, "logIndex": true, "transactionHash": true,
		},
	}
}

type Client struct {
	endpoint    string
	httpClient  *http.Client
	zstdDecoder *zstd.Decoder
	decodeBuf   []byte
}

func New(endpoint string) *Client {
	return &Client{
		endpoint: endpoint,
		httpClient: &http.Client{
			Transport: &http.Transport{DisableCompression: true},
		},
	}
}

func (c *Client) Close() {
	if c.zstdDecoder != nil {
		c.zstdDecoder.Close()
		c.zstdDecoder = nil
	}
}

func (c *Client) FetchRaw(ctx context.Context, fromBlock uint64, toBlock *uint64, logs []LogFilter) ([]byte, error) {
	response, err := c.Fetch(ctx, fromBlock, toBlock, logs)
	if err != nil {
		return nil, err
	}
	return response.Raw, nil
}

func (c *Client) Fetch(ctx context.Context, fromBlock uint64, toBlock *uint64, logs []LogFilter) (Response, error) {
	return c.FetchWithParent(ctx, fromBlock, toBlock, "", false, logs)
}

func (c *Client) FetchWithParent(ctx context.Context, fromBlock uint64, toBlock *uint64, parentBlockHash string, includeAllBlocks bool, logs []LogFilter) (Response, error) {
	q := Query{
		Type: "evm", FromBlock: fromBlock, ToBlock: toBlock,
		ParentBlockHash: parentBlockHash, IncludeAllBlocks: includeAllBlocks,
		Logs: logs, Fields: DefaultEVMFields(),
	}
	body, err := json.Marshal(q)
	if err != nil {
		return Response{}, fmt.Errorf("marshal query: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return Response{}, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept-Encoding", zstdEncoding)

	networkStart := time.Now()
	resp, err := c.httpClient.Do(req)
	networkDuration := time.Since(networkStart)
	if err != nil {
		return Response{}, fmt.Errorf("execute request: %w", err)
	}
	defer resp.Body.Close()

	response := Response{Head: parseHeadFromHeaders(resp.Header), NetworkDuration: networkDuration}
	if resp.StatusCode == http.StatusNoContent {
		return response, nil
	}
	if resp.StatusCode == http.StatusConflict {
		fork, err := parseForkError(resp.Body)
		if err != nil {
			return Response{}, err
		}
		return Response{}, fork
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return Response{}, &RateLimitError{RetryAfter: parseRetryAfter(resp.Header)}
	}
	if resp.StatusCode != http.StatusOK {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return Response{}, fmt.Errorf("sqd status %d: %s", resp.StatusCode, strings.TrimSpace(string(detail)))
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return Response{}, fmt.Errorf("read body: %w", err)
	}
	if resp.Header.Get("Content-Encoding") != zstdEncoding {
		response.Raw = raw
		return response, nil
	}
	zstdStart := time.Now()
	if c.zstdDecoder == nil {
		// One decoder per Client, one Client per parallel-fetch worker: with
		// WithDecoderConcurrency(0) (=GOMAXPROCS) each decoder tries to shard a
		// single response frame across all cores, which just adds goroutine
		// fan-out overhead on frames this size (measured ~1.3-1.6x slower and
		// ~16x more allocation than concurrency=1 on a real captured page).
		// Matches klauspost's own gzhttp.Transport zstd client default.
		dec, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1), zstd.WithDecoderLowmem(true))
		if err != nil {
			return Response{}, fmt.Errorf("create zstd decoder: %w", err)
		}
		c.zstdDecoder = dec
	}
	c.decodeBuf = c.decodeBuf[:0]
	decompressed, err := c.zstdDecoder.DecodeAll(raw, c.decodeBuf)
	if err != nil {
		return Response{}, fmt.Errorf("zstd decode: %w", err)
	}
	response.ZstdDuration = time.Since(zstdStart)
	c.decodeBuf = decompressed
	response.Raw = decompressed
	return response, nil
}

func parseForkError(r io.Reader) (*ForkError, error) {
	var body struct {
		PreviousBlocks []BlockRef `json:"previousBlocks"`
	}
	if err := json.NewDecoder(io.LimitReader(r, 1<<20)).Decode(&body); err != nil {
		return nil, fmt.Errorf("decode sqd fork response: %w", err)
	}
	return &ForkError{PreviousBlocks: body.PreviousBlocks}, nil
}

func parseHeadFromHeaders(headers http.Header) Head {
	finalizedHeadHash := headers.Get("X-Sqd-Finalized-Head-Hash")
	finalizedHeadNumber := headers.Get("X-Sqd-Finalized-Head-Number")
	headNumber := headers.Get("X-Sqd-Head-Number")

	return Head{
		Finalized: parseBlockRef(finalizedHeadNumber, finalizedHeadHash),
		Latest:    parseBlockRef(headNumber, ""),
	}
}

func parseBlockRef(number, hash string) *BlockRef {
	if number == "" {
		return nil
	}
	parsed, err := strconv.ParseUint(number, 10, 64)
	if err != nil {
		return nil
	}
	return &BlockRef{Number: parsed, Hash: hash}
}
