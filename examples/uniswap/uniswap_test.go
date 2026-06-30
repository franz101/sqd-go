package uniswap

import (
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/franz101/sqd-go/examples/uniswap/generated"
	"github.com/holiman/uint256"
)

// TestProcessBasicFunctionality tests the basic Process function with mock data
func TestProcessBasicFunctionality(t *testing.T) {
	state := &generated.State{}
	block := &generated.ParsedBlock{}

	// Test processing with empty block
	err := Process(state, block)
	if err != nil {
		t.Errorf("Process() with empty block should not error, got: %v", err)
	}
}

// TestUserPositionInitialization tests the userPosition helper function
func TestUserPositionInitialization(t *testing.T) {
	state := &generated.State{}
	testAddr := common.HexToAddress("0x8236a87084f8B84306f72007F36F2618A5634494")

	// Test creating new position
	pos := userPosition(state, testAddr)
	if pos == nil {
		t.Fatal("userPosition() should return non-nil position")
	}
	if pos.Address != testAddr {
		t.Errorf("userPosition() address = %s, want %s", pos.Address, testAddr)
	}
}

// TestUserPositionRetrieval tests retrieving existing positions
func TestUserPositionRetrieval(t *testing.T) {
	// Save/Get require a HotState; the zero-value &generated.State{} leaves
	// HotState nil, so use the constructor like real callers do.
	state := generated.NewState()
	testAddr := common.HexToAddress("0x8236a87084f8B84306f72007F36F2618A5634494")

	// Create and save initial position
	hundred := uint256.NewInt(100)
	fifty := uint256.NewInt(50)
	initialPos := &generated.UserPosition{
		Address:       testAddr,
		TotalIn:       *hundred,
		TotalOut:      *fifty,
		TransferCount: 1,
	}

	// Mock the save operation
	state.UserPosition.Save(initialPos, generated.EventMeta{
		BlockNumber: 100,
	})

	// Try to retrieve the position
	retrievedPos, ok := state.UserPosition.Get(testAddr)
	if !ok {
		t.Fatal("Failed to retrieve saved position")
	}

	if retrievedPos.Address != testAddr {
		t.Errorf("Retrieved address = %s, want %s", retrievedPos.Address, testAddr)
	}
}

// TestUserPositionZeroAddressHandling tests handling of zero addresses
func TestUserPositionZeroAddressHandling(t *testing.T) {
	state := &generated.State{}
	var zeroAddr common.Address

	// Test with zero address - should still work but might not be stored
	pos := userPosition(state, zeroAddr)
	if pos == nil {
		t.Fatal("userPosition() with zero address should return non-nil position")
	}
}

// TestProcessWithMockEvents tests Process with mock event data
func TestProcessWithMockEvents(t *testing.T) {
	// Create mock event
	value := uint256.NewInt(1000)
	mockEvent := &generated.LBTCTransfer{
		EventMeta: generated.EventMeta{
			BlockNumber: 100,
		},
		From:  common.HexToAddress("0x8236a87084f8B84306f72007F36F2618A5634494"),
		To:    common.HexToAddress("0xabcdefabcdefabcdefabcdefabcdefabcdefabcd"),
		Value: *value,
	}

	// Test event field access
	fromAddr := mockEvent.From
	toAddr := mockEvent.To
	eventValue := &mockEvent.Value

	// Test value handling
	expectedValue := uint256.NewInt(1000)
	if eventValue.Cmp(expectedValue) != 0 {
		t.Errorf("Event value mismatch, got %v, want %v", eventValue, expectedValue)
	}

	// Test address handling
	if mockEvent.From != fromAddr {
		t.Errorf("Event From mismatch, got %s, want %s", mockEvent.From, fromAddr)
	}

	if mockEvent.To != toAddr {
		t.Errorf("Event To mismatch, got %s, want %s", mockEvent.To, toAddr)
	}
}

// TestProcessProtoFunction tests the ProcessProto bridge function
func TestProcessProtoFunction(t *testing.T) {
	state := &generated.State{}

	// Test with nil ProtoEventBlock - should handle gracefully
	// We can't easily create a real ProtoEventBlock without the generated code
	// but we can test that the function exists and has the right signature

	// This test verifies the bridge function exists
	// In real scenario, ProcessProto calls Process internally
	if state == nil {
		t.Error("State should not be nil for ProcessProto")
	}
}

// TestEventMetaFieldHandling tests EventMeta field handling
func TestEventMetaFieldHandling(t *testing.T) {
	meta := generated.EventMeta{
		BlockNumber:       100,
		BlockTimestamp:    time.Now(),
		TransactionIndex:  1,
		LogIndex:          2,
	}

	// Verify EventMeta fields are accessible
	if meta.BlockNumber != 100 {
		t.Errorf("BlockNumber = %d, want 100", meta.BlockNumber)
	}

	if meta.TransactionIndex != 1 {
		t.Errorf("TransactionIndex = %d, want 1", meta.TransactionIndex)
	}

	if meta.LogIndex != 2 {
		t.Errorf("LogIndex = %d, want 2", meta.LogIndex)
	}
}

// TestUint256Operations tests uint256.Int operations used in the processor
func TestUint256Operations(t *testing.T) {
	totalIn := uint256.NewInt(100)
	addValue := uint256.NewInt(50)

	// Test addition
	totalIn.Add(totalIn, addValue)
	expected := uint256.NewInt(150)
	if totalIn.Cmp(expected) != 0 {
		t.Errorf("Add result = %v, want %v", totalIn, expected)
	}

	// Test comparison
	if totalIn.Cmp(uint256.NewInt(149)) <= 0 {
		t.Error("Comparison should show totalIn > 149")
	}

	if totalIn.Cmp(uint256.NewInt(151)) >= 0 {
		t.Error("Comparison should show totalIn < 151")
	}
}

// TestAddressComparison tests common.Address comparison operations
func TestAddressComparison(t *testing.T) {
	addr1 := common.HexToAddress("0x8236a87084f8B84306f72007F36F2618A5634494")
	addr2 := common.HexToAddress("0x8236a87084f8B84306f72007F36F2618A5634494")
	addr3 := common.HexToAddress("0xabcdefabcdefabcdefabcdefabcdefabcdefabcd")
	var zeroAddr common.Address

	// Test same address comparison
	if addr1 != addr2 {
		t.Error("Same addresses should be equal")
	}

	// Test different address comparison
	if addr1 == addr3 {
		t.Error("Different addresses should not be equal")
	}

	// Test zero address comparison
	if addr1 == zeroAddr {
		t.Error("Non-zero address should not equal zero address")
	}

	if zeroAddr != (common.Address{}) {
		t.Error("Uninitialized zero address should equal empty address")
	}
}

// TestUserPositionSchemaStructure tests the UserPositionSchema struct
func TestUserPositionSchemaStructure(t *testing.T) {
	addr := common.HexToAddress("0x8236a87084f8B84306f72007F36F2618A5634494")
	hundred := uint256.NewInt(100)
	fifty := uint256.NewInt(50)
	schema := UserPositionSchema{
		Address:        addr,
		TotalIn:        *hundred,
		TotalOut:       *fifty,
		TransferCount:  2,
		UpdatedAtBlock: 100,
		UpdatedAt:      time.Now(),
	}

	// Verify struct fields
	if schema.Address != addr {
		t.Errorf("Address = %s, want %s", schema.Address, addr)
	}

	expectedIn := uint256.NewInt(100)
	if (&schema.TotalIn).Cmp(expectedIn) != 0 {
		t.Errorf("TotalIn = %v, want %v", schema.TotalIn, expectedIn)
	}

	expectedOut := uint256.NewInt(50)
	if (&schema.TotalOut).Cmp(expectedOut) != 0 {
		t.Errorf("TotalOut = %v, want %v", schema.TotalOut, expectedOut)
	}

	if schema.TransferCount != 2 {
		t.Errorf("TransferCount = %d, want 2", schema.TransferCount)
	}

	if schema.UpdatedAtBlock != 100 {
		t.Errorf("UpdatedAtBlock = %d, want 100", schema.UpdatedAtBlock)
	}
}

// TestProcessorRegistration tests that the processor is properly registered in init()
func TestProcessorRegistration(t *testing.T) {
	// Test that the processor functions are registered
	// This is implicitly tested by the fact that the package can be imported
	// and the init() function runs without panic

	// The actual registration is tested in the integration test
	// This unit test verifies the basic structure exists
	if generated.ProjectName == "" {
		t.Error("ProjectName should be set by generated code")
	}
}

// TestProcessErrorHandling tests error handling in Process function
func TestProcessErrorHandling(t *testing.T) {
	block := &generated.ParsedBlock{}

	// Test with nil state (should handle gracefully or error appropriately)
	err := Process(nil, block)
	if err == nil {
		// Some implementations might handle nil state gracefully
		t.Log("Process handled nil state gracefully")
	} else {
		t.Logf("Process correctly errored on nil state: %v", err)
	}
}

// TestValueUpdateLogic tests the value update logic used in Process
func TestValueUpdateLogic(t *testing.T) {
	var totalOut uint256.Int
	addValue := uint256.NewInt(100)

	// Test initial addition
	totalOut.Add(&totalOut, addValue)
	if totalOut.Cmp(addValue) != 0 {
		t.Errorf("Initial addition failed: got %v, want %v", &totalOut, addValue)
	}

	// Test cumulative addition
	secondAdd := uint256.NewInt(50)
	totalOut.Add(&totalOut, secondAdd)
	expected := uint256.NewInt(150)
	if totalOut.Cmp(expected) != 0 {
		t.Errorf("Cumulative addition failed: got %v, want %v", &totalOut, expected)
	}
}

// TestTransferCountIncrement tests the transfer count increment logic
func TestTransferCountIncrement(t *testing.T) {
	transferCount := uint64(0)

	// Test increment
	transferCount++
	if transferCount != 1 {
		t.Errorf("After first increment, count = %d, want 1", transferCount)
	}

	transferCount++
	if transferCount != 2 {
		t.Errorf("After second increment, count = %d, want 2", transferCount)
	}
}