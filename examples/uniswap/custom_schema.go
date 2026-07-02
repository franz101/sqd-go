package uniswap

import (
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/holiman/uint256"
)

// custom_schema.go declares the DERIVED state your processor maintains.
//
// Each struct whose name ends in "Schema" becomes a hot-state entity reachable
// from the generated State: the "Schema" suffix is stripped, so
// UserPositionSchema -> state.UserPosition with .Get(key) / .Save(value, meta).
// The "// pk:" comment immediately above the struct names the primary-key
// field(s) (falling back to a matching config.yaml "state:" entry's key, then
// an "ID" field, then the first field). Every table is a plain MergeTree
// append-only history — one row per commit, suffixed "_log" — paired with a
// "_live" view that resolves to one row per primary key (latest write wins).
//
// This file MUST use the same package name as custom_processor.go ("uniswap").

// pk: Address
type UserPositionSchema struct {
	Address        common.Address // primary key: the account
	TotalIn        uint256.Int    // cumulative LBTC received (To == Address)
	TotalOut       uint256.Int    // cumulative LBTC sent (From == Address)
	TransferCount  uint64         // number of transfers touching this account
	UpdatedAtBlock uint64         // block number of the last update
	UpdatedAt      time.Time      // block timestamp of the last update
}
