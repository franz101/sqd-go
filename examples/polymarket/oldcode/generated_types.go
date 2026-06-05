package subgraph

import (
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/holiman/uint256"
	"github.com/shopspring/decimal"
)

// Event types for ConditionalTokens contract.
// All bytes32 fields use common.Hash for consistency with go-ethereum idioms.

type ConditionaltokensPositionSplit struct {
	Stakeholder        common.Address
	CollateralToken    common.Address `abi:"collateralToken"`
	ParentCollectionID common.Hash
	ConditionID        common.Hash
	Partition          []*big.Int `abi:"partition"`
	Amount             *big.Int
}

type ConditionaltokensPositionsMerge struct {
	Stakeholder        common.Address
	CollateralToken    common.Address `abi:"collateralToken"`
	ParentCollectionID common.Hash
	ConditionID        common.Hash
	Partition          []*big.Int `abi:"partition"`
	Amount             *big.Int
}

type ConditionaltokensConditionPreparation struct {
	Oracle           common.Address
	QuestionID       common.Hash
	ConditionID      common.Hash
	OutcomeSlotCount *big.Int `abi:"outcomeSlotCount"`
}

type ConditionaltokensConditionResolution struct {
	Oracle           common.Address
	QuestionID       common.Hash
	ConditionID      common.Hash
	OutcomeSlotCount *big.Int   `abi:"outcomeSlotCount"`
	PayoutNumerators []*big.Int `abi:"payoutNumerators"`
}

type ConditionaltokensPayoutRedemption struct {
	Redeemer           common.Address
	CollateralToken    common.Address
	ParentCollectionID common.Hash
	ConditionID        common.Hash `abi:"conditionId"`
	IndexSets          []*big.Int  `abi:"indexSets"`
	Payout             *big.Int
}

// Event types for Exchange contract.

type ExchangeOrderFilled struct {
	Maker             common.Address
	Taker             common.Address
	MakerAssetID      *big.Int `abi:"makerAssetId"`
	TakerAssetID      *big.Int `abi:"takerAssetId"`
	MakerAmountFilled *big.Int
	TakerAmountFilled *big.Int
	Fee               *big.Int
}

// Event types for NegRiskAdapter contract.

type NegriskadapterPositionSplit struct {
	Stakeholder common.Address
	ConditionID common.Hash
	Amount      *big.Int
}

type NegriskadapterPositionsMerge struct {
	Stakeholder common.Address
	ConditionID common.Hash
	Amount      *big.Int
}

type NegriskadapterPositionsConverted struct {
	Stakeholder common.Address
	MarketID    common.Hash
	IndexSet    *big.Int
	Amount      *big.Int
}

type NegriskadapterMarketPrepared struct {
	MarketID common.Hash
	Oracle   common.Address
	FeeBips  *big.Int `abi:"feeBips"`
	Data     []byte
}

type NegriskadapterQuestionPrepared struct {
	MarketID   common.Hash
	QuestionID common.Hash
	Index      *big.Int
	Data       []byte
}

type NegriskadapterPayoutRedemption struct {
	Redeemer    common.Address
	ConditionID common.Hash
	Amounts     []*big.Int
	Payout      *big.Int
}

// Event types for NegRiskExchange contract.

type NegriskexchangeOrderFilled struct {
	Maker             common.Address
	Taker             common.Address
	MakerAssetID      *big.Int `abi:"makerAssetId"`
	TakerAssetID      *big.Int `abi:"takerAssetId"`
	MakerAmountFilled *big.Int
	TakerAmountFilled *big.Int
	Fee               *big.Int
}

// Utils

func BigIntToDecimal(i *big.Int) decimal.Decimal {
	if i == nil {
		return decimal.Zero
	}
	return decimal.NewFromBigInt(i, 0)
}

func Uint256ToDecimal(i *uint256.Int) decimal.Decimal {
	if i == nil {
		return decimal.Zero
	}
	return decimal.NewFromBigInt(i.ToBig(), 0)
}
