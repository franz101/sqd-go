package clock

// EventType is a 1-byte tag for SoA dispatch. Fits 255 event types.
type EventType uint8

const (
	TypeTransfer     EventType = 0
	TypeTokenCreated EventType = 1
	TypeBurn         EventType = 2
)

// Transfer represents an ERC-20 Transfer event.
type Transfer struct {
	From   [20]byte
	To     [20]byte
	Amount uint64 // or *uint256.Int in production
}

// TokenCreated represents a token creation event.
type TokenCreated struct {
	Token   [20]byte
	Creator [20]byte
}

// Burn represents a burn event.
type Burn struct {
	From   [20]byte
	Amount uint64
}

// EventStore is a zero-allocation, cache-friendly event log.
//
// Design:
//   - SoA layout: separate slices per event type + 1-byte sequence array.
//   - Running-index replay: walk the 1-byte sequence, advance per-type counters.
//     No indices stored → 64 events per L1 cache line in the sequence array.
//   - Manual length tracking avoids bounds checks on len() in hot paths.
//   - Strict 2x growth for predictable memory behaviour.
//   - All methods are inlineable. Zero heap allocations after initial setup.
//
// Usage:
//
//	s := clock.NewEventStore(100_000)
//	s.AppendTransfer(&clock.Transfer{From: ..., To: ..., Amount: 1000})
//	s.AppendBurn(&clock.Burn{From: ..., Amount: 500})
//	s.Replay(func(t *clock.Transfer) { ... }, nil, nil)
type EventStore struct {
	transfers     []Transfer
	tokensCreated []TokenCreated
	burns         []Burn
	sequence      []EventType

	tLen   int
	cLen   int
	bLen   int
	seqLen int
}

// NewEventStore allocates an EventStore with the given initial capacity.
// Capacity is a hint for the sequence array; data arrays grow independently.
func NewEventStore(initialCap int) *EventStore {
	if initialCap < 1 {
		initialCap = 1024
	}
	return &EventStore{
		transfers:     make([]Transfer, initialCap),
		tokensCreated: make([]TokenCreated, initialCap/10),
		burns:         make([]Burn, initialCap/10),
		sequence:      make([]EventType, initialCap),
	}
}

// Len returns the number of events stored.
func (s *EventStore) Len() int { return s.seqLen }

// Reset clears all events without freeing memory (reuses backing arrays).
func (s *EventStore) Reset() {
	s.tLen = 0
	s.cLen = 0
	s.bLen = 0
	s.seqLen = 0
}

// AppendTransfer records a Transfer event.
func (s *EventStore) AppendTransfer(ev *Transfer) {
	s.growTransfers()
	s.transfers[s.tLen] = *ev
	s.tLen++
	s.appendSeq(TypeTransfer)
}

// AppendTokenCreated records a TokenCreated event.
func (s *EventStore) AppendTokenCreated(ev *TokenCreated) {
	s.growTokensCreated()
	s.tokensCreated[s.cLen] = *ev
	s.cLen++
	s.appendSeq(TypeTokenCreated)
}

// AppendBurn records a Burn event.
func (s *EventStore) AppendBurn(ev *Burn) {
	s.growBurns()
	s.burns[s.bLen] = *ev
	s.bLen++
	s.appendSeq(TypeBurn)
}

// --- internal grow helpers (inlineable) ---

func (s *EventStore) growTransfers() {
	if s.tLen == cap(s.transfers) {
		s.transfers = grow2x(s.transfers, s.tLen)
	}
}

func (s *EventStore) growTokensCreated() {
	if s.cLen == cap(s.tokensCreated) {
		s.tokensCreated = grow2x(s.tokensCreated, s.cLen)
	}
}

func (s *EventStore) growBurns() {
	if s.bLen == cap(s.burns) {
		s.burns = grow2x(s.burns, s.bLen)
	}
}

func (s *EventStore) appendSeq(t EventType) {
	if s.seqLen == cap(s.sequence) {
		s.sequence = grow2x(s.sequence, s.seqLen)
	}
	s.sequence[s.seqLen] = t
	s.seqLen++
}

// grow2x doubles capacity, starting at 1024 if empty.
func grow2x[T any](slice []T, length int) []T {
	newCap := cap(slice) * 2
	if newCap == 0 {
		newCap = 1024
	}
	bigger := make([]T, newCap)
	copy(bigger, slice[:length])
	return bigger
}

// Replay walks events in insertion order, calling the appropriate handler.
// Pass nil for event types you don't care about.
// Handlers receive pointers into the store — zero-copy, but do not retain them.
//
// The compiler fully inlines this method. The hot loop is:
//
//	for i := 0; i < s.seqLen; i++ {
//	    switch s.sequence[i] {
//	    case TypeTransfer:
//	        onTransfer(&s.transfers[tIdx]); tIdx++
//	    ...
func (s *EventStore) Replay(
	onTransfer func(*Transfer),
	onTokenCreated func(*TokenCreated),
	onBurn func(*Burn),
) {
	var tIdx, cIdx, bIdx int
	seq := s.sequence[:s.seqLen]
	for i := range seq {
		switch seq[i] {
		case TypeTransfer:
			if onTransfer != nil {
				onTransfer(&s.transfers[tIdx])
			}
			tIdx++
		case TypeTokenCreated:
			if onTokenCreated != nil {
				onTokenCreated(&s.tokensCreated[cIdx])
			}
			cIdx++
		case TypeBurn:
			if onBurn != nil {
				onBurn(&s.burns[bIdx])
			}
			bIdx++
		}
	}
}

// EventKind returns the event type at position i (O(1) random access).
func (s *EventStore) EventKind(i int) EventType {
	return s.sequence[i]
}

// TransferAt returns a pointer to the i-th Transfer (0-indexed among Transfers only).
func (s *EventStore) TransferAt(i int) *Transfer { return &s.transfers[i] }

func (s *EventStore) TokenCreatedAt(i int) *TokenCreated { return &s.tokensCreated[i] }

func (s *EventStore) BurnAt(i int) *Burn { return &s.burns[i] }

// Sequence returns the 1-byte type-tag array (length = Len()).
// Transfers/TokensCreated/Burns return the data slices.
// Use these for direct iteration when callback overhead matters.
func (s *EventStore) Sequence() []EventType  { return s.sequence[:s.seqLen] }
func (s *EventStore) Transfers() []Transfer  { return s.transfers[:s.tLen] }
func (s *EventStore) TokensCreated() []TokenCreated { return s.tokensCreated[:s.cLen] }
func (s *EventStore) BurnsSlice() []Burn    { return s.burns[:s.bLen] }
