package ingestion

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/franz101/sqd-go/abiunpack"
	"github.com/franz101/sqd-go/internal/client"
	"github.com/franz101/sqd-go/internal/config"
	"github.com/franz101/sqd-go/internal/parser"
	"github.com/holiman/uint256"
)

func TestNextRequestRangeCursorCapsToLocalEnd(t *testing.T) {
	end := uint64(20)

	toBlock, label, ok := nextRequestRange(10, 0, &end, true)

	if !ok {
		t.Fatal("range should be fetchable")
	}
	if toBlock == nil || *toBlock != 20 {
		t.Fatalf("cursor request toBlock = %v, want 20", toBlock)
	}
	if label != "[10-20]" {
		t.Fatalf("label = %q, want [10-20]", label)
	}
}

func TestNextProducerRequestRangeAdaptiveCursorCapsToLocalEnd(t *testing.T) {
	end := uint64(6259530)

	toBlock, label, ok := nextProducerRequestRange(6254531, 0, 5000, 0, &end, true)

	if !ok {
		t.Fatal("range should be fetchable")
	}
	if toBlock == nil || *toBlock != 6259530 {
		t.Fatalf("adaptive cursor toBlock = %v, want 6259530", toBlock)
	}
	if label != "[6254531-6259530]" {
		t.Fatalf("label = %q, want [6254531-6259530]", label)
	}
}

func TestNextProducerRequestRangeStopsPastLocalEnd(t *testing.T) {
	end := uint64(6259530)

	_, _, ok := nextProducerRequestRange(6259531, 0, 5000, 10000000, &end, true)

	if ok {
		t.Fatal("range past local end should stop")
	}
}

func TestNextRequestRangeBoundedUsesPageSizeAndEnd(t *testing.T) {
	end := uint64(20)

	toBlock, label, ok := nextRequestRange(10, 250, &end, false)

	if !ok {
		t.Fatal("range should be fetchable")
	}
	if toBlock == nil || *toBlock != 20 {
		t.Fatalf("bounded request toBlock = %v, want 20", toBlock)
	}
	if label != "[10-20]" {
		t.Fatalf("label = %q, want [10-20]", label)
	}
}

func TestWaitForNextCursorPollReturnsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	err := waitForNextCursorPoll(ctx, time.Hour)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("wait returned after %v, want immediate context cancellation", elapsed)
	}
}

func TestShouldWaitForEmptyCursorResponseWithoutEndBlock(t *testing.T) {
	if !shouldWaitForEmptyCursorResponse(nil) {
		t.Fatal("empty cursor response without end block should wait for new blocks")
	}
}

func TestShouldWaitForEmptyCursorResponseWithEndBlock(t *testing.T) {
	end := uint64(20)

	if shouldWaitForEmptyCursorResponse(&end) {
		t.Fatal("empty cursor response with end block should stop")
	}
}

func TestEmptyCursorCheckpointUsesFinalizedHead(t *testing.T) {
	checkpoint, ok := emptyCursorCheckpoint(10, client.Head{
		Finalized: &client.BlockRef{Number: 12, Hash: "0x12"},
	})

	if !ok {
		t.Fatal("checkpoint should be available")
	}
	if checkpoint != 12 {
		t.Fatalf("checkpoint = %d, want 12", checkpoint)
	}
}

func TestEmptyCursorCheckpointIgnoresFinalizedHeadBeforeCurrentBlock(t *testing.T) {
	checkpoint, ok := emptyCursorCheckpoint(10, client.Head{
		Finalized: &client.BlockRef{Number: 9, Hash: "0x9"},
	})

	if ok {
		t.Fatalf("checkpoint = %d, want no checkpoint", checkpoint)
	}
}

func TestPrintProfilePrintsWithoutParseIterations(t *testing.T) {
	var buf bytes.Buffer
	oldOutput := log.Writer()
	oldFlags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	defer func() {
		log.SetOutput(oldOutput)
		log.SetFlags(oldFlags)
	}()

	var before, after runtime.MemStats
	printProfile(10*time.Millisecond, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, time.Now().Add(-time.Second), before, after)

	got := buf.String()
	if !strings.Contains(got, "PROFILE") {
		t.Fatalf("profile output missing header:\n%s", got)
	}
	if !strings.Contains(got, "0 iterations") {
		t.Fatalf("profile output missing zero-iteration parse line:\n%s", got)
	}
}

func TestProfileTotalsDelta(t *testing.T) {
	previous := profileTotals{
		fetch:                10 * time.Millisecond,
		parse:                20 * time.Millisecond,
		decode:               5 * time.Millisecond,
		marshal:              3 * time.Millisecond,
		insert:               7 * time.Millisecond,
		custom:               11 * time.Millisecond,
		consumerWait:         13 * time.Millisecond,
		producerBackpressure: 17 * time.Millisecond,
		iterations:           4,
	}
	current := profileTotals{
		fetch:                14 * time.Millisecond,
		parse:                29 * time.Millisecond,
		decode:               8 * time.Millisecond,
		marshal:              5 * time.Millisecond,
		insert:               12 * time.Millisecond,
		custom:               18 * time.Millisecond,
		consumerWait:         15 * time.Millisecond,
		producerBackpressure: 23 * time.Millisecond,
		iterations:           7,
	}

	got := current.delta(previous)
	want := profileTotals{
		fetch:                4 * time.Millisecond,
		parse:                9 * time.Millisecond,
		decode:               3 * time.Millisecond,
		marshal:              2 * time.Millisecond,
		insert:               5 * time.Millisecond,
		custom:               7 * time.Millisecond,
		consumerWait:         2 * time.Millisecond,
		producerBackpressure: 6 * time.Millisecond,
		iterations:           3,
	}
	if got != want {
		t.Fatalf("delta = %+v, want %+v", got, want)
	}

	if got := previous.delta(current); got != (profileTotals{}) {
		t.Fatalf("counter reset delta = %+v, want zero", got)
	}
}

func TestProcessorProfileDelta(t *testing.T) {
	previous := ProcessorProfile{
		ConditionResolveDuration: 5 * time.Millisecond,
		ConditionRoundTrips:      7,
		FPMMResolveDuration:      11 * time.Millisecond,
		FPMMRoundTrips:           13,
	}
	current := ProcessorProfile{
		ConditionResolveDuration: 17 * time.Millisecond,
		ConditionRoundTrips:      10,
		FPMMResolveDuration:      30 * time.Millisecond,
		FPMMRoundTrips:           18,
	}
	want := ProcessorProfile{
		ConditionResolveDuration: 12 * time.Millisecond,
		ConditionRoundTrips:      3,
		FPMMResolveDuration:      19 * time.Millisecond,
		FPMMRoundTrips:           5,
	}

	if got := current.Delta(previous); got != want {
		t.Fatalf("delta = %+v, want %+v", got, want)
	}
	if got := previous.Delta(current); got != (ProcessorProfile{}) {
		t.Fatalf("counter reset delta = %+v, want zero", got)
	}
}

func TestFastJSONLParserRetainedStringsSurviveParserReuse(t *testing.T) {
	p := parser.NewFastJSONLParser(2)

	firstRaw := []byte(`{"header":{"number":1,"hash":"0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","timestamp":1},"logs":[{"address":"0x1111111111111111111111111111111111111111","transactionHash":"0xbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","transactionIndex":1,"logIndex":2,"topics":["0xcccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"],"data":"0x01"}]}` + "\n")
	var retained parser.Block
	if err := p.Parse(firstRaw, func(block *parser.Block) error {
		retained.Header = block.Header
		retained.Logs = append(retained.Logs[:0], block.Logs...)
		retained.Logs[0].Topics = append([]string(nil), block.Logs[0].Topics...)
		return nil
	}); err != nil {
		t.Fatalf("parse first JSONL: %v", err)
	}

	secondRaw := []byte(`{"header":{"number":2,"hash":"0xdddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd","timestamp":2},"logs":[{"address":"0x2222222222222222222222222222222222222222","transactionHash":"0xeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee","transactionIndex":3,"logIndex":4,"topics":["0xffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"],"data":"0x02"}]}` + "\n")
	if err := p.Parse(secondRaw, func(block *parser.Block) error {
		return nil
	}); err != nil {
		t.Fatalf("parse second JSONL: %v", err)
	}

	if retained.Header.Hash != "0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Fatalf("retained hash changed after parser reuse: %s", retained.Header.Hash)
	}
	if retained.Logs[0].Address != "0x1111111111111111111111111111111111111111" {
		t.Fatalf("retained address changed after parser reuse: %s", retained.Logs[0].Address)
	}
	if retained.Logs[0].Data != "0x01" {
		t.Fatalf("retained data changed after parser reuse: %s", retained.Logs[0].Data)
	}
	if retained.Logs[0].Topics[0] != "0xcccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc" {
		t.Fatalf("retained topic changed after parser reuse: %s", retained.Logs[0].Topics[0])
	}
}

func TestRetainReplayJSONLPageOwnsResponseBytes(t *testing.T) {
	response := []byte("{\"header\":{\"number\":1}}\n{\"header\":{\"number\":2}}\n")
	retained := retainReplayJSONLPage(response)

	var lines [][]byte
	p := parser.NewFastJSONLParser(2)
	if err := p.ParseWithLine(retained, func(_ *parser.Block, rawLine []byte) error {
		lines = append(lines, rawLine)
		return nil
	}); err != nil {
		t.Fatalf("parse retained page: %v", err)
	}

	for i := range response {
		response[i] = 'x'
	}
	if got, want := string(lines[0]), `{"header":{"number":1}}`; got != want {
		t.Fatalf("first retained line changed after source reuse: got %q, want %q", got, want)
	}
	if got, want := string(lines[1]), `{"header":{"number":2}}`; got != want {
		t.Fatalf("second retained line changed after source reuse: got %q, want %q", got, want)
	}
}

func TestIngestionDecodeScratchDoesNotCorruptSecondLogData(t *testing.T) {
	contracts := []config.ChainContractConfig{{
		Name:    "Scratch",
		Address: config.Address{"0x1111111111111111111111111111111111111111"},
		Events:  []config.EventConfig{{Event: "Value(uint256 value)"}},
	}}
	decoders, filters, err := parser.BuildEventDecoder(contracts)
	if err != nil {
		t.Fatalf("build decoder: %v", err)
	}
	topic0 := filters[0].Topic0[0]
	raw := []byte(`{"header":{"number":100,"hash":"0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","timestamp":1},"logs":[` +
		`{"address":"0x1111111111111111111111111111111111111111","transactionHash":"0xbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","transactionIndex":0,"logIndex":0,"topics":["` + topic0 + `"],"data":"` + abiWordHex(1) + `"},` +
		`{"address":"0x1111111111111111111111111111111111111111","transactionHash":"0xcccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc","transactionIndex":0,"logIndex":1,"topics":["` + topic0 + `"],"data":"` + abiWordHex(2) + `"}` +
		`]}` + "\n")

	var values []string
	var dataScratch []byte
	p := parser.NewFastJSONLParser(2)
	if err := p.ParseWithLine(raw, func(block *parser.Block, rawLine []byte) error {
		for _, lg := range block.Logs {
			def, ok := decoders[abiunpack.DecodeTopicHash(lg.Topics[0])]
			if !ok {
				t.Fatalf("missing decoder for topic %s", lg.Topics[0])
			}
			dataScratch = abiunpack.AppendHexBytes(dataScratch, lg.Data)
			ev, err := def.Decode(lg.Address, lg.Topics, dataScratch)
			if err != nil {
				t.Fatalf("decode log %d: %v", lg.LogIndex, err)
			}
			// Handle both native uint256.Int and normalized string types
			var valStr string
			switch v := ev.Params["value"].(type) {
			case string:
				valStr = v
			case *uint256.Int:
				valStr = v.Dec()
			case uint256.Int:
				valStr = v.Dec()
			default:
				valStr = fmt.Sprintf("%v", v)
			}
			values = append(values, valStr)
		}
		return nil
	}); err != nil {
		t.Fatalf("parse response: %v", err)
	}

	if len(values) != 2 {
		t.Fatalf("decoded values = %v, want two values", values)
	}
	if values[0] != "1" || values[1] != "2" {
		t.Fatalf("decoded values = %v, want [1 2]", values)
	}
	if len(dataScratch) != 32 {
		t.Fatalf("dataScratch length = %d, want one ABI word", len(dataScratch))
	}
}

func abiWordHex(v uint64) string {
	return "0x" + strings.Repeat("0", 64-len(strconv.FormatUint(v, 16))) + strconv.FormatUint(v, 16)
}
