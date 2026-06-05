package locklessgenericringbuffer

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestGenericRingBuffer(t *testing.T) {
	size := uint32(16)
	rb, err := CreateBuffer[int](size, 4)
	assert.NoError(t, err)
	assert.NotNil(t, rb)

	consumer, err := rb.CreateConsumer()
	assert.NoError(t, err)

	// Write and read single item
	rb.Write(42)
	val := consumer.Get()
	assert.Equal(t, 42, val)

	// Write and read multiple items
	for i := 1; i <= 10; i++ {
		rb.Write(i)
	}

	for i := 1; i <= 10; i++ {
		assert.Equal(t, i, consumer.Get())
	}
}

func TestHistoricBlockBuffer(t *testing.T) {
	size := uint32(8)
	hb, err := CreateHistoricBuffer(size, 4)
	assert.NoError(t, err)
	assert.NotNil(t, hb)

	c, err := hb.CreateConsumer()
	assert.NoError(t, err)

	header := BlockHeader{
		Number:    100,
		Hash:      "0x123",
		Parent:    "0x111",
		Timestamp: 1620000000,
	}
	txs := []Transaction{
		{Hash: "0xabc", From: "0x1", To: "0x2", Value: 1000, GasUsed: 21000},
	}
	positions := []UserPosition{
		{UserAddress: "0xabc", TokenID: "0x123", Amount: 500},
	}
	fills := []OrderFilled{
		{OrderHash: "0xfill", Maker: "0x1", Taker: "0x2", Amount: 100, Price: 50},
	}
	markets := []Market{
		{ID: "0xmkt", ConditionID: "0xcond", Outcomes: [2]string{"yes", "no"}},
	}

	hb.Write(100, header, txs, positions, fills, markets)

	outPositions := make([]UserPosition, 10)
	outFills := make([]OrderFilled, 10)
	outMarkets := make([]Market, 10)

	blockNum, gotHeader, gotTxs, numPositions, numFills, numMarkets := c.Read(outPositions, outFills, outMarkets)

	assert.Equal(t, uint64(100), blockNum)
	assert.Equal(t, header, gotHeader)
	assert.Equal(t, txs, gotTxs)
	
	assert.Equal(t, 1, numPositions)
	assert.Equal(t, positions[0], outPositions[0])

	assert.Equal(t, 1, numFills)
	assert.Equal(t, fills[0], outFills[0])

	assert.Equal(t, 1, numMarkets)
	assert.Equal(t, markets[0], outMarkets[0])
}

func TestConcurrentHistoricBuffer(t *testing.T) {
	size := uint32(1024)
	hb, err := CreateHistoricBuffer(size, 2)
	assert.NoError(t, err)

	c, err := hb.CreateConsumer()
	assert.NoError(t, err)

	numBlocks := 500
	var wg sync.WaitGroup
	wg.Add(2)

	// Producer
	go func() {
		defer wg.Done()
		for i := 0; i < numBlocks; i++ {
			h := BlockHeader{
				Number:    uint64(i),
				Hash:      "hash",
				Timestamp: uint64(i * 10),
			}
			txs := []Transaction{
				{Hash: "tx", Value: uint64(i)},
			}
			positions := []UserPosition{
				{UserAddress: "addr", TokenID: "tok", Amount: uint64(i)},
			}
			fills := []OrderFilled{
				{OrderHash: "fill", Maker: "maker", Amount: uint64(i)},
			}
			hb.Write(uint64(i), h, txs, positions, fills, nil)
		}
	}()

	// Consumer
	go func() {
		defer wg.Done()
		outPositions := make([]UserPosition, 10)
		outFills := make([]OrderFilled, 10)
		outMarkets := make([]Market, 10)

		for i := 0; i < numBlocks; i++ {
			blockNum, h, txs, numPositions, numFills, _ := c.Read(outPositions, outFills, outMarkets)
			assert.Equal(t, uint64(i), blockNum)
			assert.Equal(t, uint64(i*10), h.Timestamp)
			assert.Equal(t, uint64(i), txs[0].Value)
			assert.Equal(t, 1, numPositions)
			assert.Equal(t, uint64(i), outPositions[0].Amount)
			assert.Equal(t, 1, numFills)
			assert.Equal(t, uint64(i), outFills[0].Amount)
		}
	}()

	// Wait with timeout to prevent deadlock in case of lockless bugs
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		// Success
	case <-time.After(5 * time.Second):
		t.Fatal("Timeout waiting for concurrent execution (likely deadlock or block)")
	}
}
