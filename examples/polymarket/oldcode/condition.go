package subgraph

import (
	"github.com/ethereum/go-ethereum/common"
	"github.com/holiman/uint256"
)

type Condition struct {
	ID               common.Hash
	Oracle           common.Address
	QuestionID       common.Hash
	OutcomeSlotCount int
	Resolved         bool
	Payouts          []*uint256.Int
	LastSeenBlock    uint64
	LastSeenBatch    uint64

	// Relations
	Markets   []*Market
	Positions []*Position
}

type Position struct {
	ID           common.Hash
	ConditionID  common.Hash
	OutcomeIndex uint8

	// Relations
	Condition *Condition
	Market    *Market
}

type Market struct {
	ID          common.Address
	ConditionID common.Hash
	QuestionID  common.Hash
	Questions   []common.Hash

	// Relations
	Condition *Condition
}