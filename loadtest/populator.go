package main

import (
	"context"
	"encoding/binary"
	"fmt"
	"log"
	"math/rand"
	"runtime"
	"sync"
	"time"

	"github.com/ClickHouse/ch-go"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/franz101/sqd-go/drafts/protomath"
	generated "github.com/franz101/sqd-go/examples/polymarket/generated"
	"github.com/franz101/sqd-go/internal/database"
	"github.com/shopspring/decimal"
)

// GenerateUserAddress generates a deterministic common.Address for a given userIndex.
func GenerateUserAddress(userIndex uint64) common.Address {
	var addr common.Address
	binary.BigEndian.PutUint64(addr[12:], userIndex)
	return addr
}

// GenerateTokenID generates a deterministic common.Hash representing a token ID.
func GenerateTokenID(userIndex uint64, tokenIndex uint64) common.Hash {
	var data [16]byte
	binary.BigEndian.PutUint64(data[0:8], userIndex)
	binary.BigEndian.PutUint64(data[8:16], tokenIndex)
	return crypto.Keccak256Hash(data[:])
}

func decimal256FromDecimal(d decimal.Decimal) protomath.Decimal256 {
	coeff := d.Shift(18).BigInt()
	out, ok := protomath.FromDecimal256ScaledBigInt(coeff)
	if !ok {
		panic(fmt.Sprintf("decimal %v cannot fit Decimal256", d))
	}
	return out
}

// PopulateUserPositions creates and inserts `count` user positions.
// It splits the work among runtime.NumCPU() workers, each writing in parallel.
func PopulateUserPositions(ctx context.Context, chHost string, chPort int, chUser, chPass, chDB string, count uint64, batchSize int) error {
	log.Printf("Starting database population of %d positions...", count)
	startTime := time.Now()

	store, err := database.NewClickHouse(ctx, chHost, chPort, chUser, chPass, chDB)
	if err != nil {
		return err
	}
	defer store.Close()

	log.Printf("Truncating memory_user_positions table...")
	if err := store.Conn().Do(ctx, ch.Query{Body: fmt.Sprintf("TRUNCATE TABLE %s.memory_user_positions", chDB)}); err != nil {
		return fmt.Errorf("truncate memory_user_positions: %w", err)
	}

	var totalInserted uint64
	var progressMu sync.Mutex

	workers := runtime.NumCPU()
	positionsPerWorker := count / uint64(workers)

	log.Printf("Spawning %d worker threads to populate %d positions each...", workers, positionsPerWorker)

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()

			// Create a separate ClickHouse connection per worker for concurrency
			workerStore, err := database.NewClickHouse(ctx, chHost, chPort, chUser, chPass, chDB)
			if err != nil {
				log.Printf("Worker %d failed to connect: %v", workerID, err)
				return
			}
			defer workerStore.Close()
			workerConn := workerStore.Conn()

			batch := generated.NewMemoryUserPositionBatch()
			rng := rand.New(rand.NewSource(int64(workerID * 12345)))

			var batchCount int
			startIdx := uint64(workerID) * positionsPerWorker
			endIdx := startIdx + positionsPerWorker

			for i := startIdx; i < endIdx; i++ {
				userIndex := i / 5 // Avg 5 positions per user
				tokenIndex := i % 5

				userAddr := GenerateUserAddress(userIndex)
				tokenID := GenerateTokenID(userIndex, tokenIndex)

				// Fill values with realistic data
				amount := decimal.NewFromFloat(rng.Float64() * 1000)
				avgPrice := decimal.NewFromFloat(rng.Float64())
				realizedPnL := decimal.NewFromFloat((rng.Float64() - 0.5) * 500)
				totalBought := amount.Add(decimal.NewFromFloat(rng.Float64() * 500))

				pos := generated.MemoryUserPosition{
					User:           userAddr,
					TokenID:        tokenID,
					Amount:         decimal256FromDecimal(amount),
					AvgPrice:       decimal256FromDecimal(avgPrice),
					RealizedPnL:    decimal256FromDecimal(realizedPnL),
					TotalBought:    decimal256FromDecimal(totalBought),
					UpdatedAtBlock: 1,
					UpdatedAt:      time.Now().UTC().UnixMilli(),
					BlockNumber:    1,
					TxIndex:        0,
					LogIndex:       0,
				}

				batch.Append(pos)
				batchCount++

				if batchCount >= batchSize {
					if err := batch.Insert(ctx, workerConn, chDB); err != nil {
						log.Printf("Worker %d failed to insert batch: %v", workerID, err)
						return
					}
					batch.Reset()
					batchCount = 0

					progressMu.Lock()
					totalInserted += uint64(batchSize)
					if totalInserted%1000000 == 0 || totalInserted == count {
						log.Printf("Inserted %d/%d positions (%.1f%%) in %v",
							totalInserted, count, float64(totalInserted)/float64(count)*100.0, time.Since(startTime))
					}
					progressMu.Unlock()
				}
			}

			// Insert remaining
			if batchCount > 0 {
				if err := batch.Insert(ctx, workerConn, chDB); err != nil {
					log.Printf("Worker %d failed to insert final batch: %v", workerID, err)
					return
				}
				progressMu.Lock()
				totalInserted += uint64(batchCount)
				progressMu.Unlock()
			}
		}(w)
	}

	wg.Wait()
	log.Printf("Completed inserting %d user positions in %v.", count, time.Since(startTime))
	return nil
}
