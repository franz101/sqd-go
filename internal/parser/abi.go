package parser

import (
	"encoding/hex"
	"fmt"
	"math/big"
	"reflect"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/franz101/sqd-go/internal/client"
	"github.com/franz101/sqd-go/internal/config"
	"github.com/franz101/sqd-go/abiunpack"
	"github.com/holiman/uint256"
)

type DecodedEvent struct {
	ChainID        uint64
	BlockNumber    uint64
	BlockTimestamp time.Time
	BlockHash      string
	TxHash         string
	TxIndex        uint64
	LogIndex       uint64
	Address        string
	EventName      string
	Topic0         string
	Params         map[string]any
}

type EventDef struct {
	eventName string
	topic0    common.Hash
	abi       abi.ABI
	abiEvent  abi.Event
	indexed   abi.Arguments
	addresses map[string]struct{}
}

func BuildEventDecoder(contracts []config.ChainContractConfig) (map[common.Hash]*EventDef, []client.LogFilter, error) {
	decoders := make(map[common.Hash]*EventDef)
	var filters []client.LogFilter

	for _, cc := range contracts {
		addresses := flattenAddresses(cc.Address)
		var topic0s []string

		for _, ec := range cc.Events {
			sig := ec.Event
			name := eventNameFromSig(sig)
			abiJSON := eventToABIJSON(name, sig)
			parsed, err := abi.JSON(strings.NewReader(abiJSON))
			if err != nil {
				return nil, nil, fmt.Errorf("parse event %q: %w", sig, err)
			}
			ev, ok := parsed.Events[name]
			if !ok {
				return nil, nil, fmt.Errorf("event %q not found in parsed ABI", name)
			}
			topic0 := crypto.Keccak256Hash([]byte(ev.Sig))
			var indexed abi.Arguments
			for _, input := range ev.Inputs {
				if input.Indexed {
					indexed = append(indexed, input)
				}
			}
			if existing, ok := decoders[topic0]; ok {
				existing.mergeAddresses(addresses)
			} else {
				decoders[topic0] = &EventDef{
					eventName: name, topic0: topic0,
					abi: parsed, abiEvent: ev, indexed: indexed,
					addresses: addressSet(addresses),
				}
			}
			topic0s = append(topic0s, topic0.Hex())
		}
		if len(topic0s) > 0 {
			filters = append(filters, client.LogFilter{Address: dedupeStrings(addresses), Topic0: dedupeStrings(topic0s)})
		}
	}
	if len(filters) == 0 {
		return nil, nil, fmt.Errorf("no log filters built from config")
	}
	return decoders, dedupeFilters(filters), nil
}

func (e *EventDef) Decode(address string, topics []string, data []byte) (*DecodedEvent, error) {
	result := make(map[string]any)
	if len(data) > 0 {
		if err := e.fastUnpack(result, data); err != nil {
			if err := e.abi.UnpackIntoMap(result, e.eventName, data); err != nil {
				return nil, fmt.Errorf("unpack non-indexed: %w", err)
			}
		}
	}
	for i, arg := range e.indexed {
		if i+1 >= len(topics) {
			break
		}
		val, err := decodeIndexedTopic(topics[i+1], arg.Type)
		if err != nil {
			return nil, fmt.Errorf("decode indexed %q: %w", arg.Name, err)
		}
		result[arg.Name] = val
	}
	for k, v := range result {
		result[k] = normalizeParamValue(v)
	}
	return &DecodedEvent{Address: address, EventName: e.eventName, Topic0: e.topic0.Hex(), Params: result}, nil
}

func (e *EventDef) fastUnpack(result map[string]any, data []byte) error {
	var nonIndexed abi.Arguments
	for _, arg := range e.abiEvent.Inputs {
		if !arg.Indexed {
			nonIndexed = append(nonIndexed, arg)
		}
	}
	headWord := 0
	for _, arg := range nonIndexed {
		switch arg.Type.T {
		case abi.UintTy, abi.IntTy:
			word, ok := abiunpack.Word(data, headWord)
			if !ok {
				return fmt.Errorf("out of bounds")
			}
			var n uint256.Int
			n.SetBytes32(word)
			result[arg.Name] = &n
			headWord++
		case abi.AddressTy:
			word, ok := abiunpack.Word(data, headWord)
			if !ok {
				return fmt.Errorf("out of bounds")
			}
			result[arg.Name] = common.BytesToAddress(word[12:]).Hex()
			headWord++
		case abi.BoolTy:
			word, ok := abiunpack.Word(data, headWord)
			if !ok {
				return fmt.Errorf("out of bounds")
			}
			val, ok := abiunpack.Bool(word)
			if !ok {
				return fmt.Errorf("invalid bool")
			}
			result[arg.Name] = val
			headWord++
		case abi.StringTy:
			b, ok := abiunpack.Bytes(data, headWord)
			if !ok {
				return fmt.Errorf("out of bounds")
			}
			result[arg.Name] = string(b)
			headWord++
		case abi.BytesTy:
			b, ok := abiunpack.Bytes(data, headWord)
			if !ok {
				return fmt.Errorf("out of bounds")
			}
			result[arg.Name] = b
			headWord++
		case abi.FixedBytesTy:
			word, ok := abiunpack.Word(data, headWord)
			if !ok {
				return fmt.Errorf("out of bounds")
			}
			dst := make([]byte, arg.Type.Size)
			copy(dst, word[:arg.Type.Size])
			result[arg.Name] = dst
			headWord++
		default:
			return fmt.Errorf("unsupported fastUnpack type %v", arg.Type.T)
		}
	}
	return nil
}

func (e *EventDef) MatchesAddress(address string) bool {
	if len(e.addresses) == 0 {
		return true
	}
	_, ok := e.addresses[strings.ToLower(address)]
	return ok
}

func (e *EventDef) EventName() string {
	return e.eventName
}

func decodeIndexedTopic(topic string, t abi.Type) (any, error) {
	switch t.T {
	case abi.AddressTy:
		return abiunpack.DecodeTopicAddress(topic), nil
	case abi.BoolTy:
		return abiunpack.TopicBool(topic), nil
	case abi.BytesTy, abi.FixedBytesTy, abi.HashTy, abi.StringTy, abi.SliceTy, abi.ArrayTy:
		return abiunpack.DecodeTopicHash(topic), nil
	case abi.UintTy, abi.IntTy:
		n := new(uint256.Int)
		abiunpack.DecodeTopicUint256(topic, n)
		return n, nil
	default:
		return abiunpack.DecodeTopicHash(topic), nil
	}
}

func eventNameFromSig(sig string) string {
	if p := strings.IndexByte(sig, '('); p >= 0 {
		return sig[:p]
	}
	return sig
}

func eventToABIJSON(name, sig string) string {
	p := strings.IndexByte(sig, '(')
	if p < 0 {
		return ""
	}
	argsStr := sig[p+1:]
	if strings.HasSuffix(argsStr, ")") {
		argsStr = argsStr[:len(argsStr)-1]
	}
	var inputs []string
	if strings.TrimSpace(argsStr) != "" {
		for i, arg := range strings.Split(argsStr, ",") {
			arg = strings.TrimSpace(arg)
			parts := strings.Fields(arg)
			if len(parts) == 0 {
				continue
			}
			typ := normalizeSolidityType(parts[0])
			indexed := false
			paramName := fmt.Sprintf("p%d", i)
			for _, p := range parts[1:] {
				if p == "indexed" {
					indexed = true
				} else {
					paramName = p
				}
			}
			inputs = append(inputs, fmt.Sprintf(
				`{"indexed":%t,"name":"%s","type":"%s"}`, indexed, paramName, typ))
		}
	}
	return fmt.Sprintf(`[{"anonymous":false,"inputs":[%s],"name":"%s","type":"event"}]`,
		strings.Join(inputs, ","), name)
}

func normalizeSolidityType(typ string) string {
	switch typ {
	case "uint":
		return "uint256"
	case "int":
		return "int256"
	default:
		return typ
	}
}

func flattenAddresses(addr config.Address) []string {
	if len(addr) == 0 {
		return nil
	}
	out := make([]string, len(addr))
	for i, v := range addr {
		out[i] = strings.ToLower(v)
	}
	return out
}

func normalizeParamValue(v any) any {
	switch t := v.(type) {
	case nil:
		return nil
	case string:
		return t
	case bool:
		return t
	case *big.Int:
		if t == nil {
			return nil
		}
		n := new(uint256.Int)
		n.SetFromBig(t)
		return n
	case big.Int:
		n := new(uint256.Int)
		n.SetFromBig(&t)
		return n
	case *uint256.Int:
		if t == nil {
			return nil
		}
		return t.Dec()
	case uint256.Int:
		return t.Dec()
	case common.Address:
		return t
	case common.Hash:
		return t
	case []byte:
		return "0x" + hex.EncodeToString(t)

	// Fixed-size byte arrays (common for bytes32/bytes20/etc.)
	case [32]byte:
		return "0x" + hex.EncodeToString(t[:])
	case [20]byte:
		return "0x" + hex.EncodeToString(t[:])
	case [16]byte:
		return "0x" + hex.EncodeToString(t[:])
	case [8]byte:
		return "0x" + hex.EncodeToString(t[:])
	case [4]byte:
		return "0x" + hex.EncodeToString(t[:])
	case [64]byte:
		return "0x" + hex.EncodeToString(t[:])

	// Slices of integers
	case []*big.Int:
		out := make([]*uint256.Int, len(t))
		for i, x := range t {
			if x != nil {
				n := new(uint256.Int)
				n.SetFromBig(x)
				out[i] = n
			}
		}
		return out
	case []big.Int:
		out := make([]*uint256.Int, len(t))
		for i, x := range t {
			n := new(uint256.Int)
			n.SetFromBig(&x)
			out[i] = n
		}
		return out
	case []*uint256.Int:
		out := make([]string, len(t))
		for i, x := range t {
			if x != nil {
				out[i] = x.Dec()
			}
		}
		return out
	case []uint256.Int:
		out := make([]string, len(t))
		for i, x := range t {
			out[i] = x.Dec()
		}
		return out

	// Slices of other EVM types
	case []common.Address:
		return t
	case []common.Hash:
		return t
	case []string:
		return t
	case []bool:
		return t
	case [][]byte:
		out := make([]string, len(t))
		for i, x := range t {
			out[i] = "0x" + hex.EncodeToString(x)
		}
		return out

	// Fixed-width integer types
	case uint8:
		return uint64(t)
	case uint16:
		return uint64(t)
	case uint32:
		return uint64(t)
	case uint64:
		return t
	case int8:
		return int64(t)
	case int16:
		return int64(t)
	case int32:
		return int64(t)
	case int64:
		return t

	// Pointers to standard primitives/EVM types
	case *string:
		if t == nil {
			return nil
		}
		return *t
	case *bool:
		if t == nil {
			return nil
		}
		return *t
	case *common.Address:
		if t == nil {
			return nil
		}
		return *t
	case *common.Hash:
		if t == nil {
			return nil
		}
		return *t
	}

	// Rare fallback using reflect for arbitrary/custom/nested types (e.g. tuples)
	rv := reflect.ValueOf(v)
	if !rv.IsValid() {
		return nil
	}
	if rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return nil
		}
		return normalizeParamValue(rv.Elem().Interface())
	}
	if rv.Kind() != reflect.Slice && rv.Kind() != reflect.Array {
		return v
	}
	if rv.Type().Elem().Kind() == reflect.Uint8 {
		buf := make([]byte, rv.Len())
		for i := 0; i < rv.Len(); i++ {
			buf[i] = byte(rv.Index(i).Uint())
		}
		return "0x" + hex.EncodeToString(buf)
	}
	out := make([]any, rv.Len())
	for i := 0; i < rv.Len(); i++ {
		out[i] = normalizeParamValue(rv.Index(i).Interface())
	}
	return out
}

func dedupeFilters(filters []client.LogFilter) []client.LogFilter {
	seen := make(map[string]struct{})
	out := make([]client.LogFilter, 0, len(filters))
	for _, f := range filters {
		key := strings.Join(lowered(f.Address), ",") + "|" + strings.Join(lowered(f.Topic0), ",")
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, f)
	}
	return out
}

func dedupeStrings(values []string) []string {
	seen := make(map[string]struct{})
	out := make([]string, 0, len(values))
	for _, value := range values {
		key := strings.ToLower(value)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, value)
	}
	return out
}

func addressSet(addresses []string) map[string]struct{} {
	if len(addresses) == 0 {
		return nil
	}
	out := make(map[string]struct{}, len(addresses))
	for _, address := range addresses {
		out[strings.ToLower(address)] = struct{}{}
	}
	return out
}

func (e *EventDef) mergeAddresses(addresses []string) {
	if len(e.addresses) == 0 || len(addresses) == 0 {
		e.addresses = nil
		return
	}
	for _, address := range addresses {
		e.addresses[strings.ToLower(address)] = struct{}{}
	}
}

func lowered(values []string) []string {
	out := make([]string, len(values))
	for i, value := range values {
		out[i] = strings.ToLower(value)
	}
	return out
}
