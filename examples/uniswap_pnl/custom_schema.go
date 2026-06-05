package uniswap_pnl

import (
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/holiman/uint256"
)

// pk: Address
type UserPositionSchema struct {
	Address        common.Address
	Balance        uint256.Int
	TotalIn        uint256.Int
	TotalOut       uint256.Int
	UpdatedAtBlock uint64
	UpdatedAt      time.Time
}
