package subgraph

import (
	"github.com/ethereum/go-ethereum/common"
)

// NegRiskEvent tracks NegRisk market state for PnL calculations.
// Only stores question count per market - no historical data.
type NegRiskEvent struct {
	ID            common.Hash
	QuestionCount uint32
	// QuestionIDs are indexed by question index (bit position in IndexSet).
	// A zero hash entry means the question is not yet known for that index.
	QuestionIDs   []common.Hash
	LastSeenBlock uint64
	LastSeenBatch uint64
}
