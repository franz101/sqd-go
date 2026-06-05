package subgraph

import (
	"math/big"

	"github.com/ethereum/go-ethereum/common"
)

// OrderEvent is the common interface for order filled events from Exchange and NegRiskExchange.
type OrderEvent interface {
	GetMaker() common.Address
	GetTaker() common.Address
	GetMakerAssetID() *big.Int
	GetTakerAssetID() *big.Int
	GetMakerAmountFilled() *big.Int
	GetTakerAmountFilled() *big.Int
}

// Ensure ExchangeOrderFilled implements OrderEvent.
var _ OrderEvent = (*ExchangeOrderFilled)(nil)

func (e *ExchangeOrderFilled) GetMaker() common.Address             { return e.Maker }
func (e *ExchangeOrderFilled) GetTaker() common.Address             { return e.Taker }
func (e *ExchangeOrderFilled) GetMakerAssetID() *big.Int            { return e.MakerAssetID }
func (e *ExchangeOrderFilled) GetTakerAssetID() *big.Int            { return e.TakerAssetID }
func (e *ExchangeOrderFilled) GetMakerAmountFilled() *big.Int       { return e.MakerAmountFilled }
func (e *ExchangeOrderFilled) GetTakerAmountFilled() *big.Int       { return e.TakerAmountFilled }

// Ensure NegriskexchangeOrderFilled implements OrderEvent.
var _ OrderEvent = (*NegriskexchangeOrderFilled)(nil)

func (e *NegriskexchangeOrderFilled) GetMaker() common.Address       { return e.Maker }
func (e *NegriskexchangeOrderFilled) GetTaker() common.Address       { return e.Taker }
func (e *NegriskexchangeOrderFilled) GetMakerAssetID() *big.Int      { return e.MakerAssetID }
func (e *NegriskexchangeOrderFilled) GetTakerAssetID() *big.Int      { return e.TakerAssetID }
func (e *NegriskexchangeOrderFilled) GetMakerAmountFilled() *big.Int { return e.MakerAmountFilled }
func (e *NegriskexchangeOrderFilled) GetTakerAmountFilled() *big.Int { return e.TakerAmountFilled }

// StakeholderEvent is the common interface for events with a stakeholder address.
type StakeholderEvent interface {
	GetStakeholder() common.Address
}

// Ensure position split/merge events implement StakeholderEvent.
var _ StakeholderEvent = (*ConditionaltokensPositionSplit)(nil)
var _ StakeholderEvent = (*ConditionaltokensPositionsMerge)(nil)
var _ StakeholderEvent = (*NegriskadapterPositionSplit)(nil)
var _ StakeholderEvent = (*NegriskadapterPositionsMerge)(nil)
var _ StakeholderEvent = (*NegriskadapterPositionsConverted)(nil)

func (e *ConditionaltokensPositionSplit) GetStakeholder() common.Address { return e.Stakeholder }
func (e *ConditionaltokensPositionsMerge) GetStakeholder() common.Address { return e.Stakeholder }
func (e *NegriskadapterPositionSplit) GetStakeholder() common.Address    { return e.Stakeholder }
func (e *NegriskadapterPositionsMerge) GetStakeholder() common.Address   { return e.Stakeholder }
func (e *NegriskadapterPositionsConverted) GetStakeholder() common.Address { return e.Stakeholder }
