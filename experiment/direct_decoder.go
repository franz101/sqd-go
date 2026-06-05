//go:build ignore
// +build ignore

package experiment

import (
	"time"

	"github.com/ethereum/go-ethereum/common"
	generated "github.com/franz101/sqd-go/examples/polymarket/generated"
	"github.com/holiman/uint256"
	"github.com/mailru/easyjson/jlexer"
)

var hexTable [256]uint8

func init() {
	for i := range hexTable {
		hexTable[i] = 0xFF
	}
	for c := byte('0'); c <= '9'; c++ {
		hexTable[c] = c - '0'
	}
	for c := byte('a'); c <= 'f'; c++ {
		hexTable[c] = c - 'a' + 10
	}
	for c := byte('A'); c <= 'F'; c++ {
		hexTable[c] = c - 'A' + 10
	}
}

func hexDecode32(dst *[32]byte, src string, off int) {
	_ = src[off+63]
	dst[0] = hexTable[src[off+0]]<<4 | hexTable[src[off+1]]
	dst[1] = hexTable[src[off+2]]<<4 | hexTable[src[off+3]]
	dst[2] = hexTable[src[off+4]]<<4 | hexTable[src[off+5]]
	dst[3] = hexTable[src[off+6]]<<4 | hexTable[src[off+7]]
	dst[4] = hexTable[src[off+8]]<<4 | hexTable[src[off+9]]
	dst[5] = hexTable[src[off+10]]<<4 | hexTable[src[off+11]]
	dst[6] = hexTable[src[off+12]]<<4 | hexTable[src[off+13]]
	dst[7] = hexTable[src[off+14]]<<4 | hexTable[src[off+15]]
	dst[8] = hexTable[src[off+16]]<<4 | hexTable[src[off+17]]
	dst[9] = hexTable[src[off+18]]<<4 | hexTable[src[off+19]]
	dst[10] = hexTable[src[off+20]]<<4 | hexTable[src[off+21]]
	dst[11] = hexTable[src[off+22]]<<4 | hexTable[src[off+23]]
	dst[12] = hexTable[src[off+24]]<<4 | hexTable[src[off+25]]
	dst[13] = hexTable[src[off+26]]<<4 | hexTable[src[off+27]]
	dst[14] = hexTable[src[off+28]]<<4 | hexTable[src[off+29]]
	dst[15] = hexTable[src[off+30]]<<4 | hexTable[src[off+31]]
	dst[16] = hexTable[src[off+32]]<<4 | hexTable[src[off+33]]
	dst[17] = hexTable[src[off+34]]<<4 | hexTable[src[off+35]]
	dst[18] = hexTable[src[off+36]]<<4 | hexTable[src[off+37]]
	dst[19] = hexTable[src[off+38]]<<4 | hexTable[src[off+39]]
	dst[20] = hexTable[src[off+40]]<<4 | hexTable[src[off+41]]
	dst[21] = hexTable[src[off+42]]<<4 | hexTable[src[off+43]]
	dst[22] = hexTable[src[off+44]]<<4 | hexTable[src[off+45]]
	dst[23] = hexTable[src[off+46]]<<4 | hexTable[src[off+47]]
	dst[24] = hexTable[src[off+48]]<<4 | hexTable[src[off+49]]
	dst[25] = hexTable[src[off+50]]<<4 | hexTable[src[off+51]]
	dst[26] = hexTable[src[off+52]]<<4 | hexTable[src[off+53]]
	dst[27] = hexTable[src[off+54]]<<4 | hexTable[src[off+55]]
	dst[28] = hexTable[src[off+56]]<<4 | hexTable[src[off+57]]
	dst[29] = hexTable[src[off+58]]<<4 | hexTable[src[off+59]]
	dst[30] = hexTable[src[off+60]]<<4 | hexTable[src[off+61]]
	dst[31] = hexTable[src[off+62]]<<4 | hexTable[src[off+63]]
}

func hexDecode20(dst *[20]byte, src string, off int) {
	_ = src[off+39]
	dst[0] = hexTable[src[off+0]]<<4 | hexTable[src[off+1]]
	dst[1] = hexTable[src[off+2]]<<4 | hexTable[src[off+3]]
	dst[2] = hexTable[src[off+4]]<<4 | hexTable[src[off+5]]
	dst[3] = hexTable[src[off+6]]<<4 | hexTable[src[off+7]]
	dst[4] = hexTable[src[off+8]]<<4 | hexTable[src[off+9]]
	dst[5] = hexTable[src[off+10]]<<4 | hexTable[src[off+11]]
	dst[6] = hexTable[src[off+12]]<<4 | hexTable[src[off+13]]
	dst[7] = hexTable[src[off+14]]<<4 | hexTable[src[off+15]]
	dst[8] = hexTable[src[off+16]]<<4 | hexTable[src[off+17]]
	dst[9] = hexTable[src[off+18]]<<4 | hexTable[src[off+19]]
	dst[10] = hexTable[src[off+20]]<<4 | hexTable[src[off+21]]
	dst[11] = hexTable[src[off+22]]<<4 | hexTable[src[off+23]]
	dst[12] = hexTable[src[off+24]]<<4 | hexTable[src[off+25]]
	dst[13] = hexTable[src[off+26]]<<4 | hexTable[src[off+27]]
	dst[14] = hexTable[src[off+28]]<<4 | hexTable[src[off+29]]
	dst[15] = hexTable[src[off+30]]<<4 | hexTable[src[off+31]]
	dst[16] = hexTable[src[off+32]]<<4 | hexTable[src[off+33]]
	dst[17] = hexTable[src[off+34]]<<4 | hexTable[src[off+35]]
	dst[18] = hexTable[src[off+36]]<<4 | hexTable[src[off+37]]
	dst[19] = hexTable[src[off+38]]<<4 | hexTable[src[off+39]]
}

func decodeWordToUint256(dst *uint256.Int, hexStr string, wordIdx int) bool {
	off := 2 + wordIdx*64
	if off+64 > len(hexStr) {
		return false
	}
	var word [32]byte
	hexDecode32(&word, hexStr, off)
	dst.SetBytes32(word[:])
	return true
}

func decodeWordToAddress(dst *common.Address, hexStr string, wordIdx int) bool {
	off := 2 + wordIdx*64
	if off+64 > len(hexStr) {
		return false
	}
	hexDecode20((*[20]byte)(dst), hexStr, off+24)
	return true
}

func decodeWordToHash(dst *common.Hash, hexStr string, wordIdx int) bool {
	off := 2 + wordIdx*64
	if off+64 > len(hexStr) {
		return false
	}
	hexDecode32((*[32]byte)(dst), hexStr, off)
	return true
}

func decodeWordToUint256Array(hexStr string, offsetWordIdx int, alloc Allocator) ([]uint256.Int, bool) {
	var offsetWord [32]byte
	off := 2 + offsetWordIdx*64
	if off+64 > len(hexStr) {
		return nil, false
	}
	hexDecode32(&offsetWord, hexStr, off)
	offsetBytes := int(offsetWord[31]) | int(offsetWord[30])<<8 | int(offsetWord[29])<<16 | int(offsetWord[28])<<24
	arrayStartWordIdx := offsetBytes / 32

	lenOff := 2 + arrayStartWordIdx*64
	if lenOff+64 > len(hexStr) {
		return nil, false
	}
	var lenWord [32]byte
	hexDecode32(&lenWord, hexStr, lenOff)
	length := int(lenWord[31]) | int(lenWord[30])<<8 | int(lenWord[29])<<16 | int(lenWord[28])<<24

	if length == 0 {
		return nil, true
	}

	if 2+(arrayStartWordIdx+1+length)*64 > len(hexStr) {
		return nil, false
	}

	arr := alloc.MakeUint256Slice(length)
	for i := 0; i < length; i++ {
		var word [32]byte
		hexDecode32(&word, hexStr, 2+(arrayStartWordIdx+1+i)*64)
		arr[i].SetBytes32(word[:])
	}
	return arr, true
}

func decodeWordToBytes(hexStr string, offsetWordIdx int, alloc Allocator) ([]byte, bool) {
	var offsetWord [32]byte
	off := 2 + offsetWordIdx*64
	if off+64 > len(hexStr) {
		return nil, false
	}
	hexDecode32(&offsetWord, hexStr, off)
	offsetBytes := int(offsetWord[31]) | int(offsetWord[30])<<8 | int(offsetWord[29])<<16 | int(offsetWord[28])<<24
	arrayStartWordIdx := offsetBytes / 32

	lenOff := 2 + arrayStartWordIdx*64
	if lenOff+64 > len(hexStr) {
		return nil, false
	}
	var lenWord [32]byte
	hexDecode32(&lenWord, hexStr, lenOff)
	length := int(lenWord[31]) | int(lenWord[30])<<8 | int(lenWord[29])<<16 | int(lenWord[28])<<24

	if length == 0 {
		return nil, true
	}

	paddedLength := ((length + 31) / 32) * 32
	if 2+(arrayStartWordIdx+1)*64+paddedLength*2 > len(hexStr) {
		return nil, false
	}

	buf := alloc.MakeByteSlice(length)
	hexStartOff := 2 + (arrayStartWordIdx+1)*64
	for i := 0; i < length; i++ {
		buf[i] = hexTable[hexStr[hexStartOff+i*2]]<<4 | hexTable[hexStr[hexStartOff+i*2+1]]
	}
	return buf, true
}

var (
	addrConditionalTokens = common.HexToAddress("0x4D97DCd97eC945f40cF65F87097ACe5EA0476045")
	addrExchange          = common.HexToAddress("0x4bFb41d5B3570DeFd03C39a9A4D8dE6Bd8B8982E")
	addrNegRiskAdapter    = common.HexToAddress("0xd91E80cF2E7be2e162c6513ceD06f1dD0dA35296")

	topicCTConditionPreparation = common.HexToHash("0xab3760c3bd2bb38b5bcf54dc79802ed67338b4cf29f3054ded67ed24661e4177")
	topicCTConditionResolution  = common.HexToHash("0xb44d84d3289691f71497564b85d4233648d9dbae8cbdbb4329f301c3a0185894")
	topicCTPositionSplit        = common.HexToHash("0x2e6bb91f8cbcda0c93623c54d0403a43514fabc40084ec96b6d5379a74786298")
	topicCTPositionsMerge       = common.HexToHash("0x6f13ca62553fcc2bcd2372180a43949c1e4cebba603901ede2f4e14f36b282ca")
	topicCTPayoutRedemption     = common.HexToHash("0x2682012a4a4f1973119f1c9b90745d1bd91fa2bab387344f044cb3586864d18d")

	topicExchangeOrderFilled = common.HexToHash("0xd0a08e8c493f9c94f29311604c9de1b4e8c8d4c06bd0c789af57f2d65bfec0f6")

	topicNRAMarketPrepared     = common.HexToHash("0xf059ab16d1ca60e123eab60e3c02b68faf060347c701a5d14885a8e1def7b3a8")
	topicNRAQuestionPrepared   = common.HexToHash("0xaac410f87d423a922a7b226ac68f0c2eaf5bf6d15e644ac0758c7f96e2c253f7")
	topicNRAPositionSplit      = common.HexToHash("0xbbed930dbfb7907ae2d60ddf78345610214f26419a0128df39b6cc3d9e5df9b0")
	topicNRAPositionsMerge     = common.HexToHash("0xba33ac50d8894676597e6e35dc09cff59854708b642cd069d21eb9c7ca072a04")
	topicNRAPositionsConverted = common.HexToHash("0xb03d19dddbc72a87e735ff0ea3b57bef133ebe44e1894284916a84044deb367e")
	topicNRAPayoutRedemption   = common.HexToHash("0x9140a6a270ef945260c03894b3c6b3b2695e9d5101feef0ff24fec960cfd3224")
)

func decodeConditionalTokensConditionPreparation(ev *generated.ConditionalTokensConditionPreparation, topics []common.Hash, data string) bool {
	if len(topics) < 4 {
		return false
	}
	ev.ConditionID = topics[1]
	copy(ev.Oracle[:], topics[2][12:])
	ev.QuestionID = topics[3]
	return decodeWordToUint256(&ev.OutcomeSlotCount, data, 0)
}

func decodeConditionalTokensConditionResolution(ev *generated.ConditionalTokensConditionResolution, topics []common.Hash, data string, alloc Allocator) bool {
	if len(topics) < 4 {
		return false
	}
	ev.ConditionID = topics[1]
	copy(ev.Oracle[:], topics[2][12:])
	ev.QuestionID = topics[3]
	if !decodeWordToUint256(&ev.PayoutDenominator, data, 0) {
		return false
	}
	var ok bool
	ev.PayoutNumerators, ok = decodeWordToUint256Array(data, 1, alloc)
	return ok
}

func decodeConditionalTokensPositionSplit(ev *generated.ConditionalTokensPositionSplit, topics []common.Hash, data string, alloc Allocator) bool {
	if len(topics) < 4 {
		return false
	}
	copy(ev.Stakeholder[:], topics[1][12:])
	decodeWordToAddress(&ev.CollateralToken, data, 0)
	ev.ParentCollectionID = topics[2]
	ev.ConditionID = topics[3]
	if !decodeWordToUint256(&ev.Amount, data, 2) {
		return false
	}
	var ok bool
	ev.Partition, ok = decodeWordToUint256Array(data, 1, alloc)
	return ok
}

func decodeConditionalTokensPositionsMerge(ev *generated.ConditionalTokensPositionsMerge, topics []common.Hash, data string, alloc Allocator) bool {
	if len(topics) < 4 {
		return false
	}
	copy(ev.Stakeholder[:], topics[1][12:])
	decodeWordToAddress(&ev.CollateralToken, data, 0)
	ev.ParentCollectionID = topics[2]
	ev.ConditionID = topics[3]
	if !decodeWordToUint256(&ev.Amount, data, 2) {
		return false
	}
	var ok bool
	ev.Partition, ok = decodeWordToUint256Array(data, 1, alloc)
	return ok
}

func decodeConditionalTokensPayoutRedemption(ev *generated.ConditionalTokensPayoutRedemption, topics []common.Hash, data string, alloc Allocator) bool {
	if len(topics) < 4 {
		return false
	}
	copy(ev.Redeemer[:], topics[1][12:])
	copy(ev.CollateralToken[:], topics[2][12:])
	ev.ParentCollectionID = topics[3]
	decodeWordToHash(&ev.ConditionID, data, 0)
	if !decodeWordToUint256(&ev.Payout, data, 2) {
		return false
	}
	var ok bool
	ev.IndexSets, ok = decodeWordToUint256Array(data, 1, alloc)
	return ok
}

func decodeExchangeOrderFilled(ev *generated.ExchangeOrderFilled, topics []common.Hash, data string) bool {
	if len(topics) < 4 {
		return false
	}
	copy(ev.Maker[:], topics[2][12:])
	copy(ev.Taker[:], topics[3][12:])
	return decodeWordToUint256(&ev.MakerAssetID, data, 0) &&
		decodeWordToUint256(&ev.TakerAssetID, data, 1) &&
		decodeWordToUint256(&ev.MakerAmountFilled, data, 2) &&
		decodeWordToUint256(&ev.TakerAmountFilled, data, 3) &&
		decodeWordToUint256(&ev.Fee, data, 4)
}

func decodeNegRiskAdapterMarketPrepared(ev *generated.NegRiskAdapterMarketPrepared, topics []common.Hash, data string, alloc Allocator) bool {
	if len(topics) < 3 {
		return false
	}
	ev.MarketID = topics[1]
	copy(ev.Creator[:], topics[2][12:])
	if !decodeWordToUint256(&ev.FeeBips, data, 0) {
		return false
	}
	var ok bool
	ev.Data, ok = decodeWordToBytes(data, 1, alloc)
	return ok
}

func decodeNegRiskAdapterQuestionPrepared(ev *generated.NegRiskAdapterQuestionPrepared, topics []common.Hash, data string, alloc Allocator) bool {
	if len(topics) < 3 {
		return false
	}
	ev.MarketID = topics[1]
	ev.QuestionID = topics[2]
	if !decodeWordToUint256(&ev.Index, data, 0) {
		return false
	}
	var ok bool
	ev.Data, ok = decodeWordToBytes(data, 1, alloc)
	return ok
}

func decodeNegRiskAdapterPositionSplit(ev *generated.NegRiskAdapterPositionSplit, topics []common.Hash, data string) bool {
	if len(topics) < 3 {
		return false
	}
	copy(ev.Stakeholder[:], topics[1][12:])
	ev.ConditionID = topics[2]
	return decodeWordToUint256(&ev.Amount, data, 0)
}

func decodeNegRiskAdapterPositionsMerge(ev *generated.NegRiskAdapterPositionsMerge, topics []common.Hash, data string) bool {
	if len(topics) < 3 {
		return false
	}
	copy(ev.Stakeholder[:], topics[1][12:])
	ev.ConditionID = topics[2]
	return decodeWordToUint256(&ev.Amount, data, 0)
}

func decodeNegRiskAdapterPositionsConverted(ev *generated.NegRiskAdapterPositionsConverted, topics []common.Hash, data string) bool {
	if len(topics) < 4 {
		return false
	}
	copy(ev.Stakeholder[:], topics[1][12:])
	ev.MarketID = topics[2]
	ev.IndexSet.SetBytes32(topics[3][:])
	return decodeWordToUint256(&ev.Amount, data, 0)
}

func decodeNegRiskAdapterPayoutRedemption(ev *generated.NegRiskAdapterPayoutRedemption, topics []common.Hash, data string, alloc Allocator) bool {
	if len(topics) < 3 {
		return false
	}
	copy(ev.Redeemer[:], topics[1][12:])
	ev.ConditionID = topics[2]
	if !decodeWordToUint256(&ev.Payout, data, 1) {
		return false
	}
	var ok bool
	ev.Amounts, ok = decodeWordToUint256Array(data, 0, alloc)
	return ok
}

func ParseBlockJSONLDirect(line []byte, rb *OrderedHistoricRingBuffer) error {
	lexer := &jlexer.Lexer{Data: line}
	lexer.Delim('{')

	var blockNum uint64
	var blockHash string
	var blockTimestamp uint64
	var slot *generated.BlockEventsSlot
	var alloc Allocator

	var topicsBuf [4]common.Hash
	topics := topicsBuf[:0]

	for !lexer.IsDelim('}') {
		key := lexer.UnsafeFieldName(false)
		lexer.WantColon()
		switch key {
		case "header":
			lexer.Delim('{')
			for !lexer.IsDelim('}') {
				hkey := lexer.UnsafeFieldName(false)
				lexer.WantColon()
				switch hkey {
				case "number":
					blockNum = lexer.Uint64()
				case "hash":
					blockHash = lexer.UnsafeString()
				case "timestamp":
					blockTimestamp = lexer.Uint64()
				default:
					lexer.Skip()
				}
				lexer.WantComma()
			}
			lexer.Delim('}')
		case "logs":
			slot, alloc = rb.NextSlot(blockNum, blockHash)
			lexer.Delim('[')
			for !lexer.IsDelim(']') {
				lexer.Delim('{')

				var address string
				var dataHex string
				var transactionIndex uint64
				var logIndex uint64
				topics = topics[:0]

				for !lexer.IsDelim('}') {
					lkey := lexer.UnsafeFieldName(false)
					lexer.WantColon()
					switch lkey {
					case "address":
						address = lexer.UnsafeString()
					case "transactionHash":
						_ = lexer.UnsafeString()
					case "transactionIndex":
						transactionIndex = lexer.Uint64()
					case "logIndex":
						logIndex = lexer.Uint64()
					case "data":
						dataHex = lexer.UnsafeString()
					case "topics":
						lexer.Delim('[')
						for !lexer.IsDelim(']') {
							topicStr := lexer.UnsafeString()
							var topic common.Hash
							hexDecode32((*[32]byte)(&topic), topicStr, 2)
							topics = append(topics, topic)
							lexer.WantComma()
						}
						lexer.Delim(']')
					default:
						lexer.Skip()
					}
					lexer.WantComma()
				}
				lexer.Delim('}')

				// Filter by address and topic0
				if len(topics) > 0 {
					var logAddr common.Address
					hexDecode20((*[20]byte)(&logAddr), address, 2)
					topic0 := topics[0]

					meta := generated.EventMeta{
						BlockNumber:      blockNum,
						BlockTimestamp:   time.Unix(int64(blockTimestamp), 0),
						TransactionIndex: transactionIndex,
						LogIndex:         logIndex,
					}

					switch logAddr {
					case addrConditionalTokens:
						switch topic0 {
						case topicCTConditionPreparation:
							slot.ConditionalTokensConditionPreparations = append(slot.ConditionalTokensConditionPreparations, generated.ConditionalTokensConditionPreparation{EventMeta: meta})
							ev := &slot.ConditionalTokensConditionPreparations[len(slot.ConditionalTokensConditionPreparations)-1]
							if decodeConditionalTokensConditionPreparation(ev, topics, dataHex) {
								slot.Sequence = append(slot.Sequence, uint8(generated.EventTypeConditionalTokensConditionPreparation))
							} else {
								slot.ConditionalTokensConditionPreparations = slot.ConditionalTokensConditionPreparations[:len(slot.ConditionalTokensConditionPreparations)-1]
							}
						case topicCTConditionResolution:
							slot.ConditionalTokensConditionResolutions = append(slot.ConditionalTokensConditionResolutions, generated.ConditionalTokensConditionResolution{EventMeta: meta})
							ev := &slot.ConditionalTokensConditionResolutions[len(slot.ConditionalTokensConditionResolutions)-1]
							if decodeConditionalTokensConditionResolution(ev, topics, dataHex, alloc) {
								slot.Sequence = append(slot.Sequence, uint8(generated.EventTypeConditionalTokensConditionResolution))
							} else {
								slot.ConditionalTokensConditionResolutions = slot.ConditionalTokensConditionResolutions[:len(slot.ConditionalTokensConditionResolutions)-1]
							}
						case topicCTPositionSplit:
							slot.ConditionalTokensPositionSplits = append(slot.ConditionalTokensPositionSplits, generated.ConditionalTokensPositionSplit{EventMeta: meta})
							ev := &slot.ConditionalTokensPositionSplits[len(slot.ConditionalTokensPositionSplits)-1]
							if decodeConditionalTokensPositionSplit(ev, topics, dataHex, alloc) {
								slot.Sequence = append(slot.Sequence, uint8(generated.EventTypeConditionalTokensPositionSplit))
							} else {
								slot.ConditionalTokensPositionSplits = slot.ConditionalTokensPositionSplits[:len(slot.ConditionalTokensPositionSplits)-1]
							}
						case topicCTPositionsMerge:
							slot.ConditionalTokensPositionsMerges = append(slot.ConditionalTokensPositionsMerges, generated.ConditionalTokensPositionsMerge{EventMeta: meta})
							ev := &slot.ConditionalTokensPositionsMerges[len(slot.ConditionalTokensPositionsMerges)-1]
							if decodeConditionalTokensPositionsMerge(ev, topics, dataHex, alloc) {
								slot.Sequence = append(slot.Sequence, uint8(generated.EventTypeConditionalTokensPositionsMerge))
							} else {
								slot.ConditionalTokensPositionsMerges = slot.ConditionalTokensPositionsMerges[:len(slot.ConditionalTokensPositionsMerges)-1]
							}
						case topicCTPayoutRedemption:
							slot.ConditionalTokensPayoutRedemptions = append(slot.ConditionalTokensPayoutRedemptions, generated.ConditionalTokensPayoutRedemption{EventMeta: meta})
							ev := &slot.ConditionalTokensPayoutRedemptions[len(slot.ConditionalTokensPayoutRedemptions)-1]
							if decodeConditionalTokensPayoutRedemption(ev, topics, dataHex, alloc) {
								slot.Sequence = append(slot.Sequence, uint8(generated.EventTypeConditionalTokensPayoutRedemption))
							} else {
								slot.ConditionalTokensPayoutRedemptions = slot.ConditionalTokensPayoutRedemptions[:len(slot.ConditionalTokensPayoutRedemptions)-1]
							}
						}

					case addrExchange:
						if topic0 == topicExchangeOrderFilled {
							slot.ExchangeOrderFilleds = append(slot.ExchangeOrderFilleds, generated.ExchangeOrderFilled{EventMeta: meta})
							ev := &slot.ExchangeOrderFilleds[len(slot.ExchangeOrderFilleds)-1]
							if decodeExchangeOrderFilled(ev, topics, dataHex) {
								slot.Sequence = append(slot.Sequence, uint8(generated.EventTypeExchangeOrderFilled))
							} else {
								slot.ExchangeOrderFilleds = slot.ExchangeOrderFilleds[:len(slot.ExchangeOrderFilleds)-1]
							}
						}

					case addrNegRiskAdapter:
						switch topic0 {
						case topicNRAMarketPrepared:
							slot.NegRiskAdapterMarketPrepareds = append(slot.NegRiskAdapterMarketPrepareds, generated.NegRiskAdapterMarketPrepared{EventMeta: meta})
							ev := &slot.NegRiskAdapterMarketPrepareds[len(slot.NegRiskAdapterMarketPrepareds)-1]
							if decodeNegRiskAdapterMarketPrepared(ev, topics, dataHex, alloc) {
								slot.Sequence = append(slot.Sequence, uint8(generated.EventTypeNegRiskAdapterMarketPrepared))
							} else {
								slot.NegRiskAdapterMarketPrepareds = slot.NegRiskAdapterMarketPrepareds[:len(slot.NegRiskAdapterMarketPrepareds)-1]
							}
						case topicNRAQuestionPrepared:
							slot.NegRiskAdapterQuestionPrepareds = append(slot.NegRiskAdapterQuestionPrepareds, generated.NegRiskAdapterQuestionPrepared{EventMeta: meta})
							ev := &slot.NegRiskAdapterQuestionPrepareds[len(slot.NegRiskAdapterQuestionPrepareds)-1]
							if decodeNegRiskAdapterQuestionPrepared(ev, topics, dataHex, alloc) {
								slot.Sequence = append(slot.Sequence, uint8(generated.EventTypeNegRiskAdapterQuestionPrepared))
							} else {
								slot.NegRiskAdapterQuestionPrepareds = slot.NegRiskAdapterQuestionPrepareds[:len(slot.NegRiskAdapterQuestionPrepareds)-1]
							}
						case topicNRAPositionSplit:
							slot.NegRiskAdapterPositionSplits = append(slot.NegRiskAdapterPositionSplits, generated.NegRiskAdapterPositionSplit{EventMeta: meta})
							ev := &slot.NegRiskAdapterPositionSplits[len(slot.NegRiskAdapterPositionSplits)-1]
							if decodeNegRiskAdapterPositionSplit(ev, topics, dataHex) {
								slot.Sequence = append(slot.Sequence, uint8(generated.EventTypeNegRiskAdapterPositionSplit))
							} else {
								slot.NegRiskAdapterPositionSplits = slot.NegRiskAdapterPositionSplits[:len(slot.NegRiskAdapterPositionSplits)-1]
							}
						case topicNRAPositionsMerge:
							slot.NegRiskAdapterPositionsMerges = append(slot.NegRiskAdapterPositionsMerges, generated.NegRiskAdapterPositionsMerge{EventMeta: meta})
							ev := &slot.NegRiskAdapterPositionsMerges[len(slot.NegRiskAdapterPositionsMerges)-1]
							if decodeNegRiskAdapterPositionsMerge(ev, topics, dataHex) {
								slot.Sequence = append(slot.Sequence, uint8(generated.EventTypeNegRiskAdapterPositionsMerge))
							} else {
								slot.NegRiskAdapterPositionsMerges = slot.NegRiskAdapterPositionsMerges[:len(slot.NegRiskAdapterPositionsMerges)-1]
							}
						case topicNRAPositionsConverted:
							slot.NegRiskAdapterPositionsConverteds = append(slot.NegRiskAdapterPositionsConverteds, generated.NegRiskAdapterPositionsConverted{EventMeta: meta})
							ev := &slot.NegRiskAdapterPositionsConverteds[len(slot.NegRiskAdapterPositionsConverteds)-1]
							if decodeNegRiskAdapterPositionsConverted(ev, topics, dataHex) {
								slot.Sequence = append(slot.Sequence, uint8(generated.EventTypeNegRiskAdapterPositionsConverted))
							} else {
								slot.NegRiskAdapterPositionsConverteds = slot.NegRiskAdapterPositionsConverteds[:len(slot.NegRiskAdapterPositionsConverteds)-1]
							}
						case topicNRAPayoutRedemption:
							slot.NegRiskAdapterPayoutRedemptions = append(slot.NegRiskAdapterPayoutRedemptions, generated.NegRiskAdapterPayoutRedemption{EventMeta: meta})
							ev := &slot.NegRiskAdapterPayoutRedemptions[len(slot.NegRiskAdapterPayoutRedemptions)-1]
							if decodeNegRiskAdapterPayoutRedemption(ev, topics, dataHex, alloc) {
								slot.Sequence = append(slot.Sequence, uint8(generated.EventTypeNegRiskAdapterPayoutRedemption))
							} else {
								slot.NegRiskAdapterPayoutRedemptions = slot.NegRiskAdapterPayoutRedemptions[:len(slot.NegRiskAdapterPayoutRedemptions)-1]
							}
						}
					}
				}

				lexer.WantComma()
			}
			lexer.Delim(']')
		default:
			lexer.Skip()
		}
		lexer.WantComma()
	}
	lexer.Delim('}')
	return lexer.Error()
}

type Allocator interface {
	MakeUint256Slice(len int) []uint256.Int
	MakeByteSlice(len int) []byte
	Reset()
}
