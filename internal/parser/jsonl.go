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
