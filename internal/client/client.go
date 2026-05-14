package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/klauspost/compress/zstd"
)

const zstdEncoding = "zstd"

type LogFilter struct {
	Address []string `json:"address,omitempty"`
	Topic0  []string `json:"topic0,omitempty"`
}

type Query struct {
	Type             string      `json:"type"`
	FromBlock        uint64      `json:"fromBlock"`
	ToBlock          *uint64     `json:"toBlock,omitempty"`
	IncludeAllBlocks bool        `json:"includeAllBlocks"`
	Logs             []LogFilter `json:"logs,omitempty"`
	Fields           Fields      `json:"fields,omitempty"`
}

type Fields map[string]map[string]bool

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
	q := Query{
		Type: "evm", FromBlock: fromBlock, ToBlock: toBlock,
		IncludeAllBlocks: false, Logs: logs, Fields: DefaultEVMFields(),
	}
	body, err := json.Marshal(q)
	if err != nil {
		return nil, fmt.Errorf("marshal query: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept-Encoding", zstdEncoding)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("execute request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNoContent {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("sqd status %d: %s", resp.StatusCode, strings.TrimSpace(string(detail)))
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if resp.Header.Get("Content-Encoding") != zstdEncoding {
		return raw, nil
	}
	if c.zstdDecoder == nil {
		dec, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(0))
		if err != nil {
			return nil, fmt.Errorf("create zstd decoder: %w", err)
		}
		c.zstdDecoder = dec
	}
	c.decodeBuf = c.decodeBuf[:0]
	decompressed, err := c.zstdDecoder.DecodeAll(raw, c.decodeBuf)
	if err != nil {
		return nil, fmt.Errorf("zstd decode: %w", err)
	}
	c.decodeBuf = decompressed
	return decompressed, nil
}
