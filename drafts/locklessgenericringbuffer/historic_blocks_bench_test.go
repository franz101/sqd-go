package locklessgenericringbuffer

import (
	"sync"
	"testing"
)

type DummyBlock struct {
	Num       uint64
	Header    BlockHeader
	Txs       []Transaction
}

// 1. Channel-based benchmark (Control Baseline)
func BenchmarkChannelBuffer(b *testing.B) {
	ch := make(chan DummyBlock, 5000)
	
	var wg sync.WaitGroup
	wg.Add(2)

	header := BlockHeader{Number: 1, Hash: "hash", Timestamp: 123}
	txs := []Transaction{{Hash: "tx", Value: 100}}
	block := DummyBlock{Num: 1, Header: header, Txs: txs}

	b.ResetTimer()

	// Consumer
	go func() {
		defer wg.Done()
		for i := 0; i < b.N; i++ {
			_ = <-ch
		}
	}()

	// Producer
	go func() {
		defer wg.Done()
		for i := 0; i < b.N; i++ {
			ch <- block
		}
	}()

	wg.Wait()
}

// 2. Generic lockless ring buffer benchmark
func BenchmarkGenericRingBuffer(b *testing.B) {
	rb, _ := CreateBuffer[DummyBlock](8192, 2)
	c, _ := rb.CreateConsumer()

	var wg sync.WaitGroup
	wg.Add(2)

	header := BlockHeader{Number: 1, Hash: "hash", Timestamp: 123}
	txs := []Transaction{{Hash: "tx", Value: 100}}
	block := DummyBlock{Num: 1, Header: header, Txs: txs}

	b.ResetTimer()

	// Consumer
	go func() {
		defer wg.Done()
		for i := 0; i < b.N; i++ {
			_ = c.Get()
		}
	}()

	// Producer
	go func() {
		defer wg.Done()
		for i := 0; i < b.N; i++ {
			rb.Write(block)
		}
	}()

	wg.Wait()
}

// 3. Columnar historic block ring buffer benchmark
func BenchmarkHistoricBlockBuffer(b *testing.B) {
	hb, _ := CreateHistoricBuffer(8192, 2)
	c, _ := hb.CreateConsumer()

	var wg sync.WaitGroup
	wg.Add(2)

	header := BlockHeader{Number: 1, Hash: "hash", Timestamp: 123}
	txs := []Transaction{{Hash: "tx", Value: 100}}
	positions := []UserPosition{{UserAddress: "addr", TokenID: "tok", Amount: 10}}
	fills := []OrderFilled{{OrderHash: "hash", Maker: "maker", Amount: 50}}

	b.ResetTimer()

	// Consumer
	go func() {
		defer wg.Done()
		outPositions := make([]UserPosition, 10)
		outFills := make([]OrderFilled, 10)
		outMarkets := make([]Market, 10)

		for i := 0; i < b.N; i++ {
			_, _, _, _, _, _ = c.Read(outPositions, outFills, outMarkets)
		}
	}()

	// Producer
	go func() {
		defer wg.Done()
		for i := 0; i < b.N; i++ {
			hb.Write(1, header, txs, positions, fills, nil)
		}
	}()

	wg.Wait()
}

// 4. High-Throughput Simulation: 300 blocks/sec, 333 events/block (100,000 events/second scale)
func BenchmarkHighThroughputFlatBuffer(b *testing.B) {
	hb, _ := CreateHistoricBuffer(8192, 2)
	c, _ := hb.CreateConsumer()

	var wg sync.WaitGroup
	wg.Add(2)

	header := BlockHeader{Number: 1, Hash: "hash", Timestamp: 123}
	txs := make([]Transaction, 10)
	
	// Create exactly 333 events per block to simulate 100,000 events/second
	positions := make([]UserPosition, 166)
	fills := make([]OrderFilled, 167) // 166 + 167 = 333 events

	b.ResetTimer()

	// Consumer (Preallocates once to ensure 0 dynamic allocations on hot path)
	go func() {
		defer wg.Done()
		outPositions := make([]UserPosition, 500)
		outFills := make([]OrderFilled, 500)
		outMarkets := make([]Market, 10)

		for i := 0; i < b.N; i++ {
			_, _, _, _, _, _ = c.Read(outPositions, outFills, outMarkets)
		}
	}()

	// Producer
	go func() {
		defer wg.Done()
		for i := 0; i < b.N; i++ {
			hb.Write(uint64(i), header, txs, positions, fills, nil)
		}
	}()

	wg.Wait()
}
