package subgraph

import (
	"github.com/ethereum/go-ethereum/common"
	"github.com/holiman/uint256"
	"github.com/shopspring/decimal"
)

// UserPositionKey is a composite key for UserPosition lookups
type UserPositionKey struct {
	User    common.Address
	TokenID common.Hash // uint256 tokenID converted to Hash for map key
}

type UserPosition struct {
	ID            UserPositionKey
	TokenID       uint256.Int
	Amount        decimal.Decimal
	AvgPrice      decimal.Decimal // Signed, no offset encoding
	RealizedPnL   decimal.Decimal // Signed, no offset encoding
	TotalBought   decimal.Decimal
	LastSeenBlock uint64
	LastSeenBatch uint64
}