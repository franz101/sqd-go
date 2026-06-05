package locklessgenericringbuffer

import (
	"fmt"
	"sync"
	"testing"
)

// BenchmarkRingBufferScaling runs the RingBuffer with multiple concurrent consumers.
func BenchmarkRingBufferScaling(b *testing.B) {
	consumersList := []int{2, 4, 8, 16}
	for _, numConsumers := range consumersList {
		b.Run(fmt.Sprintf("Consumers-%d", numConsumers), func(b *testing.B) {
			rb, err := CreateBuffer[DummyBlock](8192, uint32(numConsumers))
			if err != nil {
				b.Fatalf("failed to create buffer: %v", err)
			}

			consumers := make([]Consumer[DummyBlock], numConsumers)
			for i := 0; i < numConsumers; i++ {
				c, err := rb.CreateConsumer()
				if err != nil {
					b.Fatalf("failed to create consumer: %v", err)
				}
				consumers[i] = c
			}

			header := BlockHeader{Number: 1, Hash: "hash", Timestamp: 123}
			txs := []Transaction{{Hash: "tx", Value: 100}}
			block := DummyBlock{Num: 1, Header: header, Txs: txs}

			var wg sync.WaitGroup
			wg.Add(numConsumers + 1)

			b.ResetTimer()

			// Start consumers
			for i := 0; i < numConsumers; i++ {
				c := consumers[i]
				go func() {
					defer wg.Done()
					for j := 0; j < b.N; j++ {
						_ = c.Get()
					}
				}()
			}

			// Start producer
			go func() {
				defer wg.Done()
				for j := 0; j < b.N; j++ {
					rb.Write(block)
				}
			}()

			wg.Wait()
		})
	}
}

// BenchmarkHistoricBlockBufferScaling runs the HistoricBlockBuffer with multiple concurrent consumers.
func BenchmarkHistoricBlockBufferScaling(b *testing.B) {
	consumersList := []int{2, 4, 8, 16}
	for _, numConsumers := range consumersList {
		b.Run(fmt.Sprintf("Consumers-%d", numConsumers), func(b *testing.B) {
			hb, err := CreateHistoricBuffer(8192, uint32(numConsumers))
			if err != nil {
				b.Fatalf("failed to create buffer: %v", err)
			}

			consumers := make([]HistoricConsumer, numConsumers)
			for i := 0; i < numConsumers; i++ {
				c, err := hb.CreateConsumer()
				if err != nil {
					b.Fatalf("failed to create consumer: %v", err)
				}
				consumers[i] = c
			}

			header := BlockHeader{Number: 1, Hash: "hash", Timestamp: 123}
			txs := []Transaction{{Hash: "tx", Value: 100}}
			positions := []UserPosition{{UserAddress: "addr", TokenID: "tok", Amount: 10}}
			fills := []OrderFilled{{OrderHash: "hash", Maker: "maker", Amount: 50}}

			var wg sync.WaitGroup
			wg.Add(numConsumers + 1)

			b.ResetTimer()

			// Start consumers
			for i := 0; i < numConsumers; i++ {
				c := consumers[i]
				go func() {
					defer wg.Done()
					outPositions := make([]UserPosition, 10)
					outFills := make([]OrderFilled, 10)
					outMarkets := make([]Market, 10)

					for j := 0; j < b.N; j++ {
						_, _, _, _, _, _ = c.Read(outPositions, outFills, outMarkets)
					}
				}()
			}

			// Start producer
			go func() {
				defer wg.Done()
				for j := 0; j < b.N; j++ {
					hb.Write(1, header, txs, positions, fills, nil)
				}
			}()

			wg.Wait()
		})
	}
}

// BenchmarkChannelBroadcastScaling runs the standard channel broadcast model (N channels, N consumers).
func BenchmarkChannelBroadcastScaling(b *testing.B) {
	consumersList := []int{2, 4, 8, 16}
	for _, numConsumers := range consumersList {
		b.Run(fmt.Sprintf("Consumers-%d", numConsumers), func(b *testing.B) {
			chans := make([]chan DummyBlock, numConsumers)
			for i := 0; i < numConsumers; i++ {
				chans[i] = make(chan DummyBlock, 8192)
			}

			header := BlockHeader{Number: 1, Hash: "hash", Timestamp: 123}
			txs := []Transaction{{Hash: "tx", Value: 100}}
			block := DummyBlock{Num: 1, Header: header, Txs: txs}

			var wg sync.WaitGroup
			wg.Add(numConsumers + 1)

			b.ResetTimer()

			// Start consumers
			for i := 0; i < numConsumers; i++ {
				ch := chans[i]
				go func() {
					defer wg.Done()
					for j := 0; j < b.N; j++ {
						_ = <-ch
					}
				}()
			}

			// Start producer
			go func() {
				defer wg.Done()
				for j := 0; j < b.N; j++ {
					for c := 0; c < numConsumers; c++ {
						chans[c] <- block
					}
				}
			}()

			wg.Wait()
		})
	}
}

// BenchmarkChannelCompetingScaling runs a single shared channel with N competing consumers (work queue).
func BenchmarkChannelCompetingScaling(b *testing.B) {
	consumersList := []int{2, 4, 8, 16}
	for _, numConsumers := range consumersList {
		b.Run(fmt.Sprintf("Consumers-%d", numConsumers), func(b *testing.B) {
			ch := make(chan DummyBlock, 8192)

			header := BlockHeader{Number: 1, Hash: "hash", Timestamp: 123}
			txs := []Transaction{{Hash: "tx", Value: 100}}
			block := DummyBlock{Num: 1, Header: header, Txs: txs}

			var wg sync.WaitGroup
			wg.Add(numConsumers)

			b.ResetTimer()

			// Start consumers
			for i := 0; i < numConsumers; i++ {
				go func() {
					defer wg.Done()
					for range ch {
						// consume
					}
				}()
			}

			// Start producer
			producerWg := sync.WaitGroup{}
			producerWg.Add(1)
			go func() {
				defer producerWg.Done()
				for j := 0; j < b.N; j++ {
					ch <- block
				}
				close(ch)
			}()

			producerWg.Wait()
			wg.Wait()
		})
	}
}
