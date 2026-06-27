package polymarket

// parserv2.go — a hand-written, zero-allocation byte-scanner parser for the
// polymarket OrderFilled hot path, written DIRECTLY against the generated
// package (no template indirection). It proves the byte-scanner win on the real
// generated multi-event schema: it fills the exact same generated InsertBatches
// (ExchangeOrderFilled / NegRiskExchangeOrderFilled) as the generated jlexer
// parser, decoding via the same abiunpack helpers — only the JSONL extraction
// differs (bytes.Index vs a per-field JSON lexer). See parserv2_test.go for the
// head-to-head benchmark + row-count equivalence check against the generated v1.

import (
	"bytes"
	"time"
	"unsafe"

	"github.com/franz101/sqd-go/abiunpack"
	generated "github.com/franz101/sqd-go/examples/polymarket/generated"
	"github.com/holiman/uint256"
)

var (
	v2Hdr         = []byte(`"header":`)
	v2Logs        = []byte(`"logs":[`)
	v2Number      = []byte(`"number":`)
	v2Timestamp   = []byte(`"timestamp":`)
	v2AddrKey     = []byte(`"address":"`)
	v2DataKey     = []byte(`"data":"`)
	v2TopicsKey   = []byte(`"topics":[`)
	v2TxIndexKey  = []byte(`"transactionIndex":`)
	v2LogIndexKey = []byte(`"logIndex":`)

	// OrderFilled topic0 (V1 Exchange + NegRiskExchange share it); routed by address.
	v2OrderFilledT0 = []byte("0xd0a08e8c493f9c94f29311604c9de1b4e8c8d4c06bd0c789af57f2d65bfec0f6")
	v2ExchangeAddr  = []byte("0x4bfb41d5b3570defd03c39a9a4d8de6bd8b8982e") // lowercase
	v2NegRiskAddr   = []byte("0xc5d563a36ae78145c45a50134d48a1215220f80a") // lowercase
)

// bstr aliases a byte slice as a string with no copy (same contract as jlexer's
// UnsafeString): valid only while the backing page buffer is alive.
func bstr(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	return unsafe.String(&b[0], len(b))
}

func v2ScanUint(b, key []byte) uint64 {
	i := bytes.Index(b, key)
	if i < 0 {
		return 0
	}
	r := b[i+len(key):]
	for len(r) > 0 && (r[0] == ' ' || r[0] == '\t') {
		r = r[1:]
	}
	var n uint64
	for j := 0; j < len(r) && r[j] >= '0' && r[j] <= '9'; j++ {
		n = n*10 + uint64(r[j]-'0')
	}
	return n
}

// scanUintAdv parses the unsigned integer at the start of p (skipping leading
// space) and returns it plus the remainder of p after the digits.
func scanUintAdv(p []byte) (uint64, []byte) {
	for len(p) > 0 && (p[0] == ' ' || p[0] == '\t') {
		p = p[1:]
	}
	var n uint64
	i := 0
	for i < len(p) && p[i] >= '0' && p[i] <= '9' {
		n = n*10 + uint64(p[i]-'0')
		i++
	}
	return n, p[i:]
}

// scanQuotedAdv reads a quoted value (p starts just after the opening quote) and
// returns the value plus the remainder of p after the closing quote.
func scanQuotedAdv(p []byte) ([]byte, []byte) {
	e := bytes.IndexByte(p, '"')
	if e < 0 {
		return nil, p
	}
	return p[:e], p[e+1:]
}

// lowerEq reports whether the ASCII-lowercased s equals lower (lower already
// lowercase). Allocation-free.
func lowerEq(s, lower []byte) bool {
	if len(s) != len(lower) {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		if c != lower[i] {
			return false
		}
	}
	return true
}

// ParseOrderFilledV2 byte-scans the page and fills the OrderFilled batches,
// identically to the generated parser's OrderFilled path. Returns event count.
func ParseOrderFilledV2(data []byte, batches *generated.InsertBatches) uint64 {
	var events uint64
	var dataBytes []byte
	var topics [4][]byte

	rest := data
	for len(rest) > 0 {
		var line []byte
		if nl := bytes.IndexByte(rest, '\n'); nl >= 0 {
			line, rest = rest[:nl], rest[nl+1:]
		} else {
			line, rest = rest, nil
		}
		if len(line) == 0 {
			continue
		}

		// Header (bounded to the header object — number/timestamp are scalar).
		hi := bytes.Index(line, v2Hdr)
		if hi < 0 {
			continue
		}
		hb := line[hi:]
		if br := bytes.IndexByte(hb, '}'); br >= 0 {
			hb = hb[:br]
		}
		blockNum := v2ScanUint(hb, v2Number)
		blockTime := time.Unix(int64(v2ScanUint(hb, v2Timestamp)), 0).UTC()

		li := bytes.Index(line, v2Logs)
		if li < 0 {
			continue
		}
		p := line[li+len(v2Logs):]
		// Single forward pass: fields appear in a stable order
		// (logIndex, transactionIndex, transactionHash, address, data, topics).
		// Each bytes.Index advances the cursor, so the whole logs region is walked
		// once — no per-field re-scan, and transactionHash is skipped for free.
		for {
			i := bytes.Index(p, v2LogIndexKey)
			if i < 0 {
				break
			}
			p = p[i+len(v2LogIndexKey):]
			logIdx, q := scanUintAdv(p)
			p = q

			i = bytes.Index(p, v2TxIndexKey)
			if i < 0 {
				break
			}
			p = p[i+len(v2TxIndexKey):]
			txIdx, q := scanUintAdv(p)
			p = q

			i = bytes.Index(p, v2AddrKey) // skips transactionHash
			if i < 0 {
				break
			}
			p = p[i+len(v2AddrKey):]
			addr, q := scanQuotedAdv(p)
			p = q

			i = bytes.Index(p, v2DataKey)
			if i < 0 {
				break
			}
			p = p[i+len(v2DataKey):]
			dataHex, q := scanQuotedAdv(p)
			p = q

			i = bytes.Index(p, v2TopicsKey)
			if i < 0 {
				break
			}
			p = p[i+len(v2TopicsKey):]
			topics[0], topics[1], topics[2], topics[3] = nil, nil, nil, nil
			for idx := 0; idx < 4; idx++ {
				rb := bytes.IndexByte(p, ']')
				qs := bytes.IndexByte(p, '"')
				if qs < 0 || (rb >= 0 && rb < qs) {
					break
				}
				p = p[qs+1:]
				qe := bytes.IndexByte(p, '"')
				if qe < 0 {
					break
				}
				topics[idx] = p[:qe]
				p = p[qe+1:]
			}
			if rb := bytes.IndexByte(p, ']'); rb >= 0 {
				p = p[rb+1:] // past the topics array
			}

			if !lowerEq(topics[0], v2OrderFilledT0) {
				continue
			}
			isExchange := lowerEq(addr, v2ExchangeAddr)
			isNegRisk := !isExchange && lowerEq(addr, v2NegRiskAddr)
			if !isExchange && !isNegRisk {
				continue
			}
			meta := generated.EventMeta{BlockNumber: blockNum, BlockTimestamp: blockTime, TransactionIndex: txIdx, LogIndex: logIdx}
			maker := abiunpack.DecodeTopicAddress(bstr(topics[2]))
			taker := abiunpack.DecodeTopicAddress(bstr(topics[3]))
			dataBytes = abiunpack.AppendHexBytes(dataBytes[:0], bstr(dataHex))
			var makerAsset, takerAsset, makerAmt, takerAmt, fee uint256.Int
			if w, ok := abiunpack.Word(dataBytes, 0); ok {
				makerAsset.SetBytes32(w)
			}
			if w, ok := abiunpack.Word(dataBytes, 1); ok {
				takerAsset.SetBytes32(w)
			}
			if w, ok := abiunpack.Word(dataBytes, 2); ok {
				makerAmt.SetBytes32(w)
			}
			if w, ok := abiunpack.Word(dataBytes, 3); ok {
				takerAmt.SetBytes32(w)
			}
			if w, ok := abiunpack.Word(dataBytes, 4); ok {
				fee.SetBytes32(w)
			}
			// The two exchanges share OrderFilled's topic0 but route to distinct,
			// distinctly-TYPED batches — the interface Append type-asserts.
			if isExchange {
				ev := generated.ExchangeOrderFilled{EventMeta: meta, Maker: maker, Taker: taker, MakerAssetID: makerAsset, TakerAssetID: takerAsset, MakerAmountFilled: makerAmt, TakerAmountFilled: takerAmt, Fee: fee}
				batches.ExchangeOrderFilled.Append(meta, &ev)
			} else {
				ev := generated.NegRiskExchangeOrderFilled{EventMeta: meta, Maker: maker, Taker: taker, MakerAssetID: makerAsset, TakerAssetID: takerAsset, MakerAmountFilled: makerAmt, TakerAmountFilled: takerAmt, Fee: fee}
				batches.NegRiskExchangeOrderFilled.Append(meta, &ev)
			}
			events++
		}
	}
	return events
}
