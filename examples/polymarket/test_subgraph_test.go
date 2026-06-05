package polymarket

import (
	"encoding/binary"
	"math/big"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	generated "github.com/franz101/sqd-go/examples/polymarket/generated"
	"github.com/holiman/uint256"
	"github.com/shopspring/decimal"
)

var testSubgraphUSDC = common.HexToAddress("0x2791Bca1f2de4661ED88A30C99A7a9449Aa84174")

func TestSubgraphSample(t *testing.T) {
	if !true {
		t.Fatal("expected true")
	}
}

func TestSubgraphAddressComparison(t *testing.T) {
	addressLowercaseString := "0x84834141f76bdb7ee72a9e67ca7bd1e849288c3a"
	address := common.HexToAddress(addressLowercaseString)

	if got := strings.ToLower(address.Hex()); got != addressLowercaseString {
		t.Fatalf("address hex mismatch: got=%s want=%s", got, addressLowercaseString)
	}
}

func TestSubgraphBigIntToString(t *testing.T) {
	if got := uint256.NewInt(1234).String(); got != "1234" {
		t.Fatalf("uint256 string mismatch: got=%s want=1234", got)
	}
}

func TestSubgraphGraphTSBigIntByteArraySemantics(t *testing.T) {
	var littleEndianOne [4]byte
	binary.LittleEndian.PutUint32(littleEndianOne[:], 1)
	if len(littleEndianOne) != 4 {
		t.Fatalf("littleEndianOne length mismatch: got=%d want=4", len(littleEndianOne))
	}
	if littleEndianOne[0] != 1 {
		t.Fatalf("littleEndianOne[0] mismatch: got=%d want=1", littleEndianOne[0])
	}

	byteArray := []byte{0x00, 0x00, 0x00, 0x01}
	if got, want := graphTSSignedLittleEndianBytesToBigInt(byteArray), big.NewInt(16777216); got.Cmp(want) != 0 {
		t.Fatalf("signed little-endian bigint mismatch: got=%s want=%s", got, want)
	}
	reverseBytes(byteArray)
	if byteArray[0] != 1 {
		t.Fatalf("reversed byteArray[0] mismatch: got=%d want=1", byteArray[0])
	}
	if got, want := graphTSSignedLittleEndianBytesToBigInt(byteArray), big.NewInt(1); got.Cmp(want) != 0 {
		t.Fatalf("reversed signed little-endian bigint mismatch: got=%s want=%s", got, want)
	}

	allOnes := []byte{0xff, 0xff, 0xff, 0xff}
	if got, want := graphTSSignedLittleEndianBytesToBigInt(allOnes), big.NewInt(-1); got.Cmp(want) != 0 {
		t.Fatalf("signed all-ones mismatch: got=%s want=%s", got, want)
	}
	if got, want := graphTSUnsignedLittleEndianBytesToBigInt(allOnes), big.NewInt(4294967295); got.Cmp(want) != 0 {
		t.Fatalf("unsigned all-ones mismatch: got=%s want=%s", got, want)
	}
}

func TestSubgraphComputeCreate2Address(t *testing.T) {
	deployer := common.HexToAddress("0x8ba1f109551bD432803012645Ac136ddd64DBA72")
	salt := common.FromHex("0x7c5ea36004851c764c44143b1dcb59679b11c9a68e5f41497f6cf3d480715331")
	initCode := common.FromHex("0x6394198df16000526103ff60206004601c335afa6040516060f3")
	initCodeHash := crypto.Keccak256(initCode)

	got := testSubgraphComputeCreate2Address(deployer, salt, initCodeHash)
	want := common.HexToAddress("0x533ae9d683B10C02EbDb05471642F85230071FC3")
	if got != want {
		t.Fatalf("create2 address mismatch: got=%s want=%s", got.Hex(), want.Hex())
	}
}

func TestSubgraphProxyWalletBytecode(t *testing.T) {
	factoryAddress := common.HexToAddress("0x2279b7a0a67db372996a5fab50d91eaa73d2ebe6")
	implementationAddress := common.HexToAddress("0xdb49cad7f11f8b7ff228044befa0ef3f3b5b4225")
	expectedBytecodeHex := "0x3d3d606380380380913d393d732279b7a0a67db372996a5fab50d91eaa73d2ebe65af4602a57600080fd5b602d8060366000396000f3363d3d373d3d3d363d73db49cad7f11f8b7ff228044befa0ef3f3b5b42255af43d82803e903d91602b57fd5bf352e831dd00000000000000000000000000000000000000000000000000000000000000200000000000000000000000000000000000000000000000000000000000000000"

	got := "0x" + common.Bytes2Hex(testSubgraphGenerateProxyWalletBytecode(factoryAddress, implementationAddress))
	if got != expectedBytecodeHex {
		t.Fatalf("proxy wallet bytecode mismatch: got=%s want=%s", got, expectedBytecodeHex)
	}
}

func TestSubgraphComputeProxyWalletAddress(t *testing.T) {
	signer := common.HexToAddress("0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266")
	factory := common.HexToAddress("0xEcA1c266193F03d28517a500007738adfb7754d8")
	implementation := common.HexToAddress("0x7d5330Fe12E75B5B775036cC1ba39EE546bD3850")

	got := testSubgraphComputeProxyWalletAddress(signer, factory, implementation)
	want := common.HexToAddress("0x03CaCD9b90eDf7E440227faeA044e566247a8635")
	if got != want {
		t.Fatalf("proxy wallet address mismatch: got=%s want=%s", got.Hex(), want.Hex())
	}
}

func TestSubgraphComputeNegRiskYesPrice(t *testing.T) {
	tests := []struct {
		name          string
		noPrice       string
		noCount       uint32
		questionCount uint32
		want          string
	}{
		{name: "case 1", noPrice: "0.75", noCount: 3, questionCount: 5, want: "0.125"},
		{name: "case 2", noPrice: "0.73", noCount: 1, questionCount: 6, want: "0.146"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := computeNegRiskYesPriceDecimal(decimal.RequireFromString(tt.noPrice), tt.noCount, tt.questionCount)
			want := decimal.RequireFromString(tt.want)
			if !got.Equal(want) {
				t.Fatalf("yes price mismatch: got=%s want=%s", got, want)
			}
		})
	}
}

func TestSubgraphComputeFpmmPrice(t *testing.T) {
	amounts := []uint256.Int{*uint256.NewInt(100), *uint256.NewInt(200)}
	scale := decimal.NewFromInt(1_000_000)

	got0 := computeFpmmPriceDecimal(amounts, 0).Mul(scale).Truncate(0)
	got1 := computeFpmmPriceDecimal(amounts, 1).Mul(scale).Truncate(0)

	if want := decimal.NewFromInt(666666); !got0.Equal(want) {
		t.Fatalf("price0 mismatch: got=%s want=%s", got0, want)
	}
	if want := decimal.NewFromInt(333333); !got1.Equal(want) {
		t.Fatalf("price1 mismatch: got=%s want=%s", got1, want)
	}
}

func TestSubgraphComputePositionID(t *testing.T) {
	state := generated.NewState()
	conditionID := common.HexToHash("0xda558eddf6eb57760bd5371fb313167f871d823a16e9d66fccb292baf2a117c0")
	collateralAddress := common.HexToAddress("0x7D1DC38E60930664F8cBF495dA6556ca091d2F92")

	got0 := state.GetPositionID(collateralAddress, state.GetCollectionID(common.Hash{}, conditionID, *uint256.NewInt(1)))
	got1 := state.GetPositionID(collateralAddress, state.GetCollectionID(common.Hash{}, conditionID, *uint256.NewInt(2)))

	if want := "108051088633899060239124498527429950692254744883563327407154880807410490438693"; got0.String() != want {
		t.Fatalf("position id 0 mismatch: got=%s want=%s", got0.String(), want)
	}
	if want := "45163082656174410071592939534766820181648934703824597457997612898109272294349"; got1.String() != want {
		t.Fatalf("position id 1 mismatch: got=%s want=%s", got1.String(), want)
	}
}

func TestSubgraphCollectionID(t *testing.T) {
	tests := []struct {
		name        string
		conditionID common.Hash
		want0       string
		want1       string
	}{
		{
			name:        "even",
			conditionID: common.HexToHash("0xdb4ab1dbbedd6aeec17aa6e3f66262cff0e3b04742dd3acdf99652575e5422f8"),
			want0:       "0x12adf3dfeaddeef8f31fa86654bf367c5c7b1e854dff407d7c87ff76af4ad16d",
			want1:       "0x2f5ebcc5972889a57d587b7088d543bbf464fbdd2b2c4cb7c276ca3d4d70415b",
		},
		{
			name:        "odd",
			conditionID: common.HexToHash("0xda558eddf6eb57760bd5371fb313167f871d823a16e9d66fccb292baf2a117c0"),
			want0:       "0x45ca66c3edbdbf0fbd03366cf395d3663ee3b34c4db07964bb60e3c7ca7e20e2",
			want1:       "0x416929e8901456139abeec231eba635f911a240961f78919665280c69288ee0d",
		},
	}

	state := generated.NewState()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got0 := state.GetCollectionID(common.Hash{}, tt.conditionID, *uint256.NewInt(1))
			got1 := state.GetCollectionID(common.Hash{}, tt.conditionID, *uint256.NewInt(2))
			if strings.ToLower(got0.Hex()) != tt.want0 {
				t.Fatalf("collection id 0 mismatch: got=%s want=%s", got0.Hex(), tt.want0)
			}
			if strings.ToLower(got1.Hex()) != tt.want1 {
				t.Fatalf("collection id 1 mismatch: got=%s want=%s", got1.Hex(), tt.want1)
			}
		})
	}
}

func TestSubgraphGetPositionID(t *testing.T) {
	state := generated.NewState()

	standardConditionID := common.HexToHash("0xfc690d5069b296bb9278af0cba42c02666e0999e4a4009ed97ea4a885f045457")
	gotStandard0 := getSubgraphPositionID(state, testSubgraphUSDC, standardConditionID, 0)
	gotStandard1 := getSubgraphPositionID(state, testSubgraphUSDC, standardConditionID, 1)
	if want := "73716170047628147940237270507900673332129573201293655532643868111690843426372"; gotStandard0.String() != want {
		t.Fatalf("standard position id 0 mismatch: got=%s want=%s", gotStandard0.String(), want)
	}
	if want := "2890213445014127424511466931609154310536269586265875141655148162320150952197"; gotStandard1.String() != want {
		t.Fatalf("standard position id 1 mismatch: got=%s want=%s", gotStandard1.String(), want)
	}

	negRiskConditionID := common.HexToHash("0xbf5ba08b3a0c4dd741f00759282e38e9bfa9ad59aa623ed13d26a8786c1e5afc")
	gotNegRisk0 := getSubgraphPositionID(state, generated.NegRiskWrappedCollateralAddr, negRiskConditionID, 0)
	gotNegRisk1 := getSubgraphPositionID(state, generated.NegRiskWrappedCollateralAddr, negRiskConditionID, 1)
	if want := "84121562275746951169805992721206824933074826856805500029198362509460400440947"; gotNegRisk0.String() != want {
		t.Fatalf("neg risk position id 0 mismatch: got=%s want=%s", gotNegRisk0.String(), want)
	}
	if want := "6492254133524731704185750998715576231739345323060925006131340263114065023619"; gotNegRisk1.String() != want {
		t.Fatalf("neg risk position id 1 mismatch: got=%s want=%s", gotNegRisk1.String(), want)
	}
}

func TestSubgraphGetNegRiskQuestionID(t *testing.T) {
	marketID := common.HexToHash("0xcc4727a6394620b9c8ae82db3db50a34d5ca9828675547bcc4cddf5e86b63000")
	got := testSubgraphNegRiskQuestionID(marketID, 7)
	want := "0xcc4727a6394620b9c8ae82db3db50a34d5ca9828675547bcc4cddf5e86b63007"
	if strings.ToLower(got.Hex()) != want {
		t.Fatalf("neg risk question id mismatch: got=%s want=%s", got.Hex(), want)
	}
}

func TestSubgraphGetNegRiskPositionID(t *testing.T) {
	tests := []struct {
		name          string
		marketID      common.Hash
		questionIndex uint32
		want0         string
		want1         string
	}{
		{
			name:          "case 1",
			marketID:      common.HexToHash("0xcc4727a6394620b9c8ae82db3db50a34d5ca9828675547bcc4cddf5e86b63000"),
			questionIndex: 7,
			want0:         "96833685517457790753237027711749956491556223430098771101130535462280443103710",
			want1:         "112683192116716745370273337699109698649408967993699289993927321945615517688893",
		},
		{
			name:          "case 2",
			marketID:      common.HexToHash("0x904aa321a48f737e2223e7b3007bf51d68b6a0d66bdda0c1e4bc581f55d62800"),
			questionIndex: 4,
			want0:         "11031149734538275426690039809123992018327740438980973428241361937177748285493",
			want1:         "92849115097658926029726616555072992123532598747617388960074918380114146610948",
		},
		{
			name:          "case 3",
			marketID:      common.HexToHash("0x904aa321a48f737e2223e7b3007bf51d68b6a0d66bdda0c1e4bc581f55d62800"),
			questionIndex: 3,
			want0:         "92934986068759649975171712359405804888500621431140776758674716227798619042594",
			want1:         "83272680118121060051327450493118657102857345150945269348505485036103238138715",
		},
		{
			name:          "case 4",
			marketID:      common.HexToHash("0x5e596465dca57c10c8b175f901974e2de2877498410b0210d0a21b57e14da000"),
			questionIndex: 4,
			want0:         "30637845681714148498359907433169105263223689440526909041094893305583115580796",
			want1:         "111796127100720291855951404495290728144208289103084969375425640210971192620108",
		},
	}

	state := generated.NewState()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got0 := state.GetNegRiskPositionID(tt.marketID, tt.questionIndex, 0)
			got1 := state.GetNegRiskPositionID(tt.marketID, tt.questionIndex, 1)
			if got0.String() != tt.want0 {
				t.Fatalf("neg risk position id 0 mismatch: got=%s want=%s", got0.String(), tt.want0)
			}
			if got1.String() != tt.want1 {
				t.Fatalf("neg risk position id 1 mismatch: got=%s want=%s", got1.String(), tt.want1)
			}
		})
	}
}

func TestSubgraphIndexSetContains(t *testing.T) {
	indexSet := uint256.NewInt(0b1010)
	assertIndexSetContains(t, indexSet, 0, false)
	assertIndexSetContains(t, indexSet, 1, true)
	assertIndexSetContains(t, indexSet, 2, false)
	assertIndexSetContains(t, indexSet, 3, true)

	indexSet = uint256.NewInt(16)
	assertIndexSetContains(t, indexSet, 0, false)
	assertIndexSetContains(t, indexSet, 1, false)
	assertIndexSetContains(t, indexSet, 2, false)
	assertIndexSetContains(t, indexSet, 3, false)
	assertIndexSetContains(t, indexSet, 4, true)
}

func testSubgraphComputeCreate2Address(deployer common.Address, salt []byte, initCodeHash []byte) common.Address {
	data := make([]byte, 0, 1+20+len(salt)+len(initCodeHash))
	data = append(data, 0xff)
	data = append(data, deployer.Bytes()...)
	data = append(data, salt...)
	data = append(data, initCodeHash...)
	fullHash := crypto.Keccak256(data)
	return common.BytesToAddress(fullHash[12:])
}

func testSubgraphGenerateProxyWalletBytecode(factory common.Address, implementation common.Address) []byte {
	bytecodeHex := "3d3d606380380380913d393d73" +
		strings.ToLower(factory.Hex()[2:]) +
		"5af4602a57600080fd5b602d8060366000396000f3363d3d373d3d3d363d73" +
		strings.ToLower(implementation.Hex()[2:]) +
		"5af43d82803e903d91602b57fd5bf352e831dd" +
		"0000000000000000000000000000000000000000000000000000000000000020" +
		"0000000000000000000000000000000000000000000000000000000000000000"
	return common.FromHex(bytecodeHex)
}

func testSubgraphComputeProxyWalletAddress(signer common.Address, factory common.Address, implementation common.Address) common.Address {
	salt := crypto.Keccak256(signer.Bytes())
	initCode := testSubgraphGenerateProxyWalletBytecode(factory, implementation)
	initCodeHash := crypto.Keccak256(initCode)
	return testSubgraphComputeCreate2Address(factory, salt, initCodeHash)
}

func getSubgraphPositionID(state *generated.State, collateral common.Address, conditionID common.Hash, outcomeIndex uint8) uint256.Int {
	indexSet := new(uint256.Int).Lsh(uint256.NewInt(1), uint(outcomeIndex))
	collectionID := state.GetCollectionID(common.Hash{}, conditionID, *indexSet)
	return state.GetPositionID(collateral, collectionID)
}

func testSubgraphNegRiskQuestionID(marketID common.Hash, questionIndex uint8) common.Hash {
	questionID := marketID
	questionID[31] = questionIndex
	return questionID
}

func assertIndexSetContains(t *testing.T, indexSet *uint256.Int, index uint8, want bool) {
	t.Helper()
	got := getBit(indexSet, int(index)) == 1
	if got != want {
		t.Fatalf("indexSetContains(%s, %d) mismatch: got=%t want=%t", indexSet.String(), index, got, want)
	}
}

func graphTSSignedLittleEndianBytesToBigInt(in []byte) *big.Int {
	if len(in) == 0 {
		return new(big.Int)
	}
	out := graphTSUnsignedLittleEndianBytesToBigInt(in)
	if in[len(in)-1]&0x80 != 0 {
		limit := new(big.Int).Lsh(big.NewInt(1), uint(len(in)*8))
		out.Sub(out, limit)
	}
	return out
}

func graphTSUnsignedLittleEndianBytesToBigInt(in []byte) *big.Int {
	reversed := append([]byte(nil), in...)
	reverseBytes(reversed)
	return new(big.Int).SetBytes(reversed)
}

func reverseBytes(in []byte) {
	for i, j := 0, len(in)-1; i < j; i, j = i+1, j-1 {
		in[i], in[j] = in[j], in[i]
	}
}
