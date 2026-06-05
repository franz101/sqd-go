package main

import (
	"encoding/binary"
	"math/big"
	"math/rand"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/franz101/sqd-go/internal/ingestion"
	"github.com/holiman/uint256"
)

// Generator state
type EventGenerator struct {
	rng           *rand.Rand
	chainID       uint64
	numUsers      uint64
	hotUsersCount uint64
	hotPercentage float64 // 0.0 to 1.0
}

func NewEventGenerator(chainID uint64, numUsers uint64, hotUsersCount uint64, hotPercentage float64) *EventGenerator {
	return &EventGenerator{
		rng:           rand.New(rand.NewSource(42)), // Seed for reproducibility
		chainID:       chainID,
		numUsers:      numUsers,
		hotUsersCount: hotUsersCount,
		hotPercentage: hotPercentage,
	}
}

// GenerateBlockLogs generates custom logs simulating ExchangeOrderFilled events in a single block.
func (eg *EventGenerator) GenerateBlockLogs(blockNum uint64, txsPerBlock int) []ingestion.CustomLog {
	logs := make([]ingestion.CustomLog, 0, txsPerBlock)
	blockTS := time.Now().Add(time.Duration(blockNum) * time.Second).UTC()

	var blockHash common.Hash
	binary.BigEndian.PutUint64(blockHash[24:], blockNum)

	for txIndex := 0; txIndex < txsPerBlock; txIndex++ {
		var txHash common.Hash
		binary.BigEndian.PutUint64(txHash[16:], blockNum)
		binary.BigEndian.PutUint64(txHash[24:], uint64(txIndex))

		// Pick a maker (user) based on hot/cold probability
		var userIndex uint64
		if eg.rng.Float64() < eg.hotPercentage && eg.hotUsersCount > 0 {
			// Hot user
			userIndex = eg.rng.Uint64() % eg.hotUsersCount
		} else {
			// Cold user (from the rest of the user pool)
			if eg.numUsers > eg.hotUsersCount {
				userIndex = eg.hotUsersCount + (eg.rng.Uint64() % (eg.numUsers - eg.hotUsersCount))
			} else {
				userIndex = eg.rng.Uint64() % eg.numUsers
			}
		}

		// Each user has 10 positions
		tokenIndex := eg.rng.Uint64() % 10

		maker := GenerateUserAddress(userIndex)
		tokenID := GenerateTokenID(userIndex, tokenIndex)

		// A random taker
		takerIndex := eg.rng.Uint64() % eg.numUsers
		taker := GenerateUserAddress(takerIndex)

		isBuy := eg.rng.Float64() < 0.5

		logItem := eg.buildOrderFilledLog(
			blockNum,
			blockHash.Hex(),
			blockTS,
			txHash.Hex(),
			uint64(txIndex),
			uint64(txIndex*2), // LogIndex
			maker,
			taker,
			tokenID,
			isBuy,
		)
		logs = append(logs, logItem)
	}

	return logs
}

// buildOrderFilledLog encodes the raw ABI payload for ExchangeOrderFilled.
func (eg *EventGenerator) buildOrderFilledLog(
	blockNum uint64,
	blockHash string,
	ts time.Time,
	txHash string,
	txIndex uint64,
	logIndex uint64,
	maker common.Address,
	taker common.Address,
	tokenID common.Hash,
	isBuy bool,
) ingestion.CustomLog {
	// Topics
	topics := make([]string, 4)
	// OrderFilled Topic0
	topics[0] = "0xd0a08e8c493f9c94f29311604c9de1b4e8c8d4c06bd0c789af57f2d65bfec0f6"

	// Mock order hash
	var orderHash common.Hash
	binary.BigEndian.PutUint64(orderHash[24:], logIndex)
	topics[1] = orderHash.Hex()

	// Maker padded to 32 bytes
	var makerPadded [32]byte
	copy(makerPadded[12:], maker.Bytes())
	topics[2] = common.BytesToHash(makerPadded[:]).Hex()

	// Taker padded to 32 bytes
	var takerPadded [32]byte
	copy(takerPadded[12:], taker.Bytes())
	topics[3] = common.BytesToHash(takerPadded[:]).Hex()

	// Data encoding:
	// makerAssetId, takerAssetId, makerAmountFilled, takerAmountFilled, fee
	// Each is 32 bytes (uint256)
	var makerAssetID, takerAssetID [32]byte
	if isBuy {
		// buying tokenID using USDC (0)
		// makerAssetId = 0 (maker buys, so maker gets tokenID and taker gets makerAssetID = USDC)
		// Wait, in custom_processor.go:
		// if makerAssetID.IsZero() { isBuy = true; tokenID = takerAssetID }
		copy(takerAssetID[:], tokenID.Bytes())
	} else {
		// selling tokenID for USDC
		// makerAssetId = tokenID (maker sells, so maker gives tokenID and taker gives USDC)
		copy(makerAssetID[:], tokenID.Bytes())
	}

	var makerAmountFilled, takerAmountFilled, fee [32]byte
	// 100 units of asset
	var valMaker, valTaker uint256.Int
	valMaker.SetUint64(uint64(eg.rng.Intn(1000) + 1))
	valTaker.SetUint64(uint64(eg.rng.Intn(1000) + 1))

	valMaker.WriteToSlice(makerAmountFilled[:])
	valTaker.WriteToSlice(takerAmountFilled[:])

	var dataPayload [160]byte
	copy(dataPayload[0:32], makerAssetID[:])
	copy(dataPayload[32:64], takerAssetID[:])
	copy(dataPayload[64:96], makerAmountFilled[:])
	copy(dataPayload[96:128], takerAmountFilled[:])
	copy(dataPayload[128:160], fee[:]) // Fee remains 0

	return ingestion.CustomLog{
		ChainID:          eg.chainID,
		BlockNumber:      blockNum,
		BlockTimestamp:   ts,
		BlockHash:        blockHash,
		ContractAddress:  "0x4bFb41d5B3570DeFd03C39a9A4D8dE6Bd8B8982E", // Exchange address
		TransactionHash:  txHash,
		TransactionIndex: txIndex,
		LogIndex:         logIndex,
		Topics:           topics,
		Data:             "0x" + common.Bytes2Hex(dataPayload[:]),
	}
}

// Convert big.Int to 32 bytes padded byte slice.
func bigIntTo32Bytes(i *big.Int) []byte {
	var buf [32]byte
	bBytes := i.Bytes()
	copy(buf[32-len(bBytes):], bBytes)
	return buf[:]
}
