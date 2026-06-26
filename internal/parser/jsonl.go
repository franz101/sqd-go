package parser

import (
	"bytes"
	"fmt"

	"github.com/valyala/fastjson"
)

type Block struct {
	Header Header
	Logs   []Log
}

type Header struct {
	Number    uint64
	Hash      string
	Timestamp uint64
}

// Log is a decoded log view valid only for the duration of the onBlock
// callback. The parser reuses the Block, its Logs slice, and each Log's
// Topics backing array across blocks to minimize allocations. Consumers that
// RETAIN a log beyond the callback MUST deep-copy Topics (the strings are
// freshly allocated, but the []string backing array is reused). Address/
// TransactionHash/Data are fresh strings and safe to keep.
type Log struct {
	Address          string
	TransactionHash  string
	Topics           []string
	Data             string
	TransactionIndex uint64
	LogIndex         uint64
}

type FastJSONLParser struct {
	parser fastjson.Parser
	block  Block
}

func NewFastJSONLParser(logCapacity int) *FastJSONLParser {
	p := &FastJSONLParser{}
	if logCapacity > 0 {
		p.block.Logs = make([]Log, 0, logCapacity)
	}
	return p
}

func (p *FastJSONLParser) Parse(data []byte, onBlock func(*Block) error) error {
	return p.ParseWithLine(data, func(block *Block, _ []byte) error {
		return onBlock(block)
	})
}

// ScanHeadersWithLine extracts only block identity and timestamp while skipping
// the logs array. The returned line aliases data; callers retaining it beyond
// the callback must own the input page. The hash is a fresh copy.
//
// It uses a hand-rolled byte scanner (no JSON lexer) that reads the three header
// fields directly and never walks the logs array. The old jlexer path drove
// SkipRecursive over the entire logs array of every line just to find the line's
// closing brace; on log-dense pages that dominated this scan.
func (p *FastJSONLParser) ScanHeadersWithLine(data []byte, onBlock func(number, timestamp uint64, hash string, line []byte) error) error {
	for len(data) > 0 {
		lineData := data
		if idx := bytes.IndexByte(data, '\n'); idx >= 0 {
			lineData = data[:idx]
			data = data[idx+1:]
		} else {
			data = nil
		}
		if len(lineData) == 0 {
			continue
		}
		number, timestamp, hash, ok := scanBlockRefHeader(lineData)
		if !ok {
			return fmt.Errorf("scan header: missing or malformed block header in %q", truncateForError(lineData))
		}
		if err := onBlock(number, timestamp, bytesToString(hash), lineData); err != nil {
			return err
		}
	}
	return nil
}

var (
	scanHeaderKey    = []byte(`"header"`)
	scanNumberKey    = []byte(`"number"`)
	scanTimestampKey = []byte(`"timestamp"`)
	scanHashKey      = []byte(`"hash"`)
)

// scanBlockRefHeader reads number/timestamp/hash from the JSONL block header
// without a JSON parser. It bounds the field scan to the "header" object so a
// "number"/"hash" key appearing inside the logs array can never be misread as a
// block field. The number field is required (genesis block 0 is valid); a
// missing or malformed header object returns ok=false. The returned hash aliases
// line. Field order is irrelevant.
func scanBlockRefHeader(line []byte) (number, timestamp uint64, hash []byte, ok bool) {
	hdr, ok := headerObject(line)
	if !ok {
		return 0, 0, nil, false
	}
	number, ok = scanUintField(hdr, scanNumberKey)
	if !ok {
		return 0, 0, nil, false
	}
	timestamp, _ = scanUintField(hdr, scanTimestampKey)
	hash, _ = scanStringField(hdr, scanHashKey)
	return number, timestamp, hash, true
}

// headerObject returns the byte slice spanning the "header" object's braces
// (inclusive of the outer { }), or ok=false if it is absent or unterminated.
func headerObject(line []byte) ([]byte, bool) {
	hi := bytes.Index(line, scanHeaderKey)
	if hi < 0 {
		return nil, false
	}
	rest := line[hi+len(scanHeaderKey):]
	c := bytes.IndexByte(rest, ':')
	if c < 0 {
		return nil, false
	}
	rest = rest[c+1:]
	for len(rest) > 0 && (rest[0] == ' ' || rest[0] == '\t') {
		rest = rest[1:]
	}
	if len(rest) == 0 || rest[0] != '{' {
		return nil, false
	}
	// The SQD header holds only scalar fields (number/hash/timestamp), so a brace
	// depth walk safely finds its end; header values never contain braces.
	depth := 0
	for i := 0; i < len(rest); i++ {
		switch rest[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return rest[:i+1], true
			}
		}
	}
	return nil, false
}

// scanUintField finds key inside b and parses the unsigned integer following its
// colon. ok is false if the key is absent or no digits follow.
func scanUintField(b, key []byte) (uint64, bool) {
	i := bytes.Index(b, key)
	if i < 0 {
		return 0, false
	}
	rest := b[i+len(key):]
	c := bytes.IndexByte(rest, ':')
	if c < 0 {
		return 0, false
	}
	rest = rest[c+1:]
	for len(rest) > 0 && (rest[0] == ' ' || rest[0] == '\t') {
		rest = rest[1:]
	}
	var n uint64
	j := 0
	for j < len(rest) && rest[j] >= '0' && rest[j] <= '9' {
		n = n*10 + uint64(rest[j]-'0')
		j++
	}
	if j == 0 {
		return 0, false
	}
	return n, true
}

// scanStringField finds key inside b and returns the quoted string value that
// follows its colon, aliasing b. ok is false if the key or value is absent.
func scanStringField(b, key []byte) ([]byte, bool) {
	i := bytes.Index(b, key)
	if i < 0 {
		return nil, false
	}
	rest := b[i+len(key):]
	c := bytes.IndexByte(rest, ':')
	if c < 0 {
		return nil, false
	}
	rest = rest[c+1:]
	for len(rest) > 0 && (rest[0] == ' ' || rest[0] == '\t') {
		rest = rest[1:]
	}
	if len(rest) == 0 || rest[0] != '"' {
		return nil, false
	}
	rest = rest[1:]
	end := bytes.IndexByte(rest, '"')
	if end < 0 {
		return nil, false
	}
	return rest[:end], true
}

// truncateForError bounds the byte slice embedded in scan-header error messages.
func truncateForError(b []byte) []byte {
	const max = 120
	if len(b) > max {
		return b[:max]
	}
	return b
}

func (p *FastJSONLParser) ParseWithLine(data []byte, onBlock func(*Block, []byte) error) error {
	for len(data) > 0 {
		lineData := data
		if idx := bytes.IndexByte(data, '\n'); idx >= 0 {
			lineData = data[:idx]
			data = data[idx+1:]
		} else {
			data = nil
		}
		if len(lineData) == 0 {
			continue
		}
		v, err := p.parser.ParseBytes(lineData)
		if err != nil {
			return fmt.Errorf("parse line: %w", err)
		}
		header := v.Get("header")
		p.block.Header.Number = header.GetUint64("number")
		p.block.Header.Hash = bytesToString(header.GetStringBytes("hash"))
		p.block.Header.Timestamp = header.GetUint64("timestamp")

		logsArr := v.GetArray("logs")
		if cap(p.block.Logs) < len(logsArr) {
			p.block.Logs = make([]Log, len(logsArr))
		} else {
			p.block.Logs = p.block.Logs[:len(logsArr)]
		}
		for i, lv := range logsArr {
			lg := &p.block.Logs[i]
			lg.Address = bytesToString(lv.GetStringBytes("address"))
			lg.TransactionHash = bytesToString(lv.GetStringBytes("transactionHash"))
			lg.Data = bytesToString(lv.GetStringBytes("data"))
			lg.TransactionIndex = lv.GetUint64("transactionIndex")
			lg.LogIndex = lv.GetUint64("logIndex")
			topics := lv.GetArray("topics")
			if cap(lg.Topics) < len(topics) {
				lg.Topics = make([]string, len(topics))
			} else {
				lg.Topics = lg.Topics[:len(topics)]
			}
			for j, t := range topics {
				lg.Topics[j] = bytesToString(t.GetStringBytes())
			}
		}
		if err := onBlock(&p.block, lineData); err != nil {
			return err
		}
	}
	return nil
}

func bytesToString(b []byte) string {
	return string(b)
}
