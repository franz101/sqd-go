package parser

import (
	"bytes"
	"fmt"

	"github.com/mailru/easyjson/jlexer"
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

// ScanHeadersWithLine extracts only the block header (number, hash, timestamp)
// from each JSONL line, byte-skipping everything else including the logs array.
// This is the single-parse-mode producer scan: the consumer's one real parse
// decodes the logs, so a full DOM parse here would be pure waste. The hash
// aliases the line's bytes — callers that retain it must strings.Clone it.
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
		var number, timestamp uint64
		var hash string
		l := &jlexer.Lexer{Data: lineData}
		l.Delim('{')
		for !l.IsDelim('}') {
			key := l.UnsafeFieldName(false)
			l.WantColon()
			if key == "header" {
				l.Delim('{')
				for !l.IsDelim('}') {
					hkey := l.UnsafeFieldName(false)
					l.WantColon()
					switch hkey {
					case "number":
						number = l.Uint64()
					case "timestamp":
						timestamp = l.Uint64()
					case "hash":
						hash = l.UnsafeString()
					default:
						l.SkipRecursive()
					}
					l.WantComma()
				}
				l.Delim('}')
			} else {
				l.SkipRecursive()
			}
			l.WantComma()
		}
		if err := l.Error(); err != nil {
			return fmt.Errorf("scan header: %w", err)
		}
		if err := onBlock(number, timestamp, hash, lineData); err != nil {
			return err
		}
	}
	return nil
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
