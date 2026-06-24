package polymarket

import (
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/franz101/sqd-go/protomath"
	"github.com/holiman/uint256"
)

// pk: ID
type MemoryConditionSchema struct {
	ID               common.Hash
	Oracle           common.Address
	QuestionID       common.Hash
	OutcomeSlotCount uint8
	Resolved         bool
	Payouts          []uint256.Int
	UpdatedAtBlock   uint64
	UpdatedAt        time.Time
}

// pk: User, TokenID
type MemoryUserPositionSchema struct {
	User           common.Address
	TokenID        common.Hash
	Amount         protomath.Decimal256
	AvgPrice       protomath.Decimal256
	RealizedPnL    protomath.Decimal256
	TotalBought    protomath.Decimal256
	UpdatedAtBlock uint64
	UpdatedAt      time.Time
}

// pk: ID
type MemoryMarketSchema struct {
	ID             common.Hash
	QuestionCount  uint32
	QuestionIDs    []common.Hash
	UpdatedAtBlock uint64
	UpdatedAt      time.Time
}

// pk: ID
type MemoryNegRiskEventSchema struct {
	ID             common.Hash
	QuestionCount  uint32
	QuestionIDs    []common.Hash
	UpdatedAtBlock uint64
	UpdatedAt      time.Time
}

// pk: ID
type MemoryFixedProductMarketMakerSchema struct {
	ID              common.Address
	ConditionID     common.Hash
	CollateralToken common.Address
	UpdatedAtBlock  uint64
	UpdatedAt       time.Time
}
