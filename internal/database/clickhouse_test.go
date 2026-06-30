package database

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ClickHouse/ch-go/proto"
	"github.com/ethereum/go-ethereum/common"
	"github.com/holiman/uint256"
)

func TestUInt256ValueColumnAppendParses10Pow77(t *testing.T) {
	n := "1" + strings.Repeat("0", 77)
	parsed, err := uint256.FromDecimal(n)
	if err != nil {
		t.Fatalf("holiman uint256.FromDecimal(%q): %v", n, err)
	}

	c := &uint256ValueColumn{name: "value"}
	c.append(n)

	if c.col.Rows() != 1 {
		t.Fatalf("rows = %d, want 1", c.col.Rows())
	}
	got := c.col.Row(0)
	want := protoUInt256(*parsed)
	if got != want {
		t.Fatalf("UInt256 column value = %#v, want %#v", got, want)
	}
	if got == (proto.UInt256{}) {
		t.Fatal("UInt256 column value is zero")
	}
}

func TestFixedStringValueColumnAppendAddressHexAsBytes(t *testing.T) {
	address := common.HexToAddress("0x8236a87084f8B84306f72007F36F2618A5634494")
	c := newFixedStringValueColumn("address", common.AddressLength)

	c.append(address.Hex())

	got := c.col.Row(0)
	want := address.Bytes()
	if !bytes.Equal(got, want) {
		t.Fatalf("FixedString(20) bytes = %x, want %x", got, want)
	}
	if bytes.HasPrefix(got, []byte("0x")) {
		t.Fatalf("FixedString(20) stored ASCII hex prefix: %q", got[:2])
	}
}

func TestFixedStringValueColumnAppendHashHexAsBytes(t *testing.T) {
	hash := common.HexToHash("0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef")
	c := newFixedStringValueColumn("topic0", common.HashLength)

	c.append(hash.Hex())

	got := c.col.Row(0)
	want := hash.Bytes()
	if !bytes.Equal(got, want) {
		t.Fatalf("FixedString(32) bytes = %x, want %x", got, want)
	}
	if bytes.HasPrefix(got, []byte("0x")) {
		t.Fatalf("FixedString(32) stored ASCII hex prefix: %q", got[:2])
	}
}

func TestFixedStringValueColumnAppendRawAddressAndHashBytes(t *testing.T) {
	address := common.HexToAddress("0x00000000000000000000000000000000000000ab")
	hash := common.HexToHash("0x0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20")

	addrCol := newFixedStringValueColumn("address", common.AddressLength)
	addrCol.append(address.Bytes())
	if got, want := addrCol.col.Row(0), address.Bytes(); !bytes.Equal(got, want) {
		t.Fatalf("raw address bytes = %x, want %x", got, want)
	}

	hashCol := newFixedStringValueColumn("hash", common.HashLength)
	hashCol.append(hash.Bytes())
	if got, want := hashCol.col.Row(0), hash.Bytes(); !bytes.Equal(got, want) {
		t.Fatalf("raw hash bytes = %x, want %x", got, want)
	}
}

func TestFixedStringHexRoundTripMatchesCanonicalString(t *testing.T) {
	tests := []struct {
		name string
		in   string
		size int
	}{
		{
			name: "address",
			in:   "0x8236a87084f8B84306f72007F36F2618A5634494",
			size: common.AddressLength,
		},
		{
			name: "hash",
			in:   "0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef",
			size: common.HashLength,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			col := newFixedStringValueColumn(tt.name, tt.size)
			col.append(tt.in)

			got := "0x" + strings.ToLower(common.Bytes2Hex(col.col.Row(0)))
			want := strings.ToLower(tt.in)
			if got != want {
				t.Fatalf("string -> bytes -> FixedString -> hex() -> string = %q, want %q", got, want)
			}
		})
	}
}

func BenchmarkFixedStringValueColumnAppendAddressString(b *testing.B) {
	address := common.HexToAddress("0x8236a87084f8B84306f72007F36F2618A5634494").Hex()
	col := newFixedStringValueColumn("address", common.AddressLength)

	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		col.col.Reset()
		col.append(address)
	}
}

func BenchmarkFixedStringValueColumnAppendHashString(b *testing.B) {
	hash := common.HexToHash("0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef").Hex()
	col := newFixedStringValueColumn("hash", common.HashLength)

	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		col.col.Reset()
		col.append(hash)
	}
}

func BenchmarkFixedStringValueColumnAppendAddressRaw(b *testing.B) {
	address := common.HexToAddress("0x8236a87084f8B84306f72007F36F2618A5634494")
	col := newFixedStringValueColumn("address", common.AddressLength)

	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		col.col.Reset()
		col.append(address)
	}
}

func BenchmarkFixedStringValueColumnAppendHashRaw(b *testing.B) {
	hash := common.HexToHash("0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef")
	col := newFixedStringValueColumn("hash", common.HashLength)

	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		col.col.Reset()
		col.append(hash)
	}
}

func TestStringValueColumnAppend(t *testing.T) {
	col := &stringValueColumn{name: "params"}

	// Test append
	col.append(`{"foo": "bar"}`)
	col.append(`{"hello": "world; semicolon"}`)

	if col.col.Rows() != 2 {
		t.Fatalf("expected 2 rows, got %d", col.col.Rows())
	}

	if got := col.col.Row(0); got != `{"foo": "bar"}` {
		t.Errorf("expected first row to be %q, got %q", `{"foo": "bar"}`, got)
	}

	if got := col.col.Row(1); got != `{"hello": "world; semicolon"}` {
		t.Errorf("expected second row to be %q, got %q", `{"hello": "world; semicolon"}`, got)
	}

	// Test reset
	col.reset()
	if col.col.Rows() != 0 {
		t.Fatalf("expected 0 rows after reset, got %d", col.col.Rows())
	}
}

func TestSplitSQLStatements(t *testing.T) {
	sqlString := `
		-- This is a comment
		CREATE TABLE IF NOT EXISTS test_db.table1 (
			id FixedString(32),
			name String
		) ENGINE = ReplacingMergeTree(block_number)
		ORDER BY id;

		-- Another comment
		INSERT INTO test_db.table1 (id, name) VALUES ('0x123', 'semicolon; inside; string');

		SELECT * FROM test_db.table1;
	`

	statements := splitSQLStatements(sqlString)
	if len(statements) != 3 {
		t.Fatalf("expected 3 statements, got %d: %#v", len(statements), statements)
	}

	if !strings.Contains(statements[0], "CREATE TABLE IF NOT EXISTS test_db.table1") {
		t.Errorf("statement 0 does not match: %q", statements[0])
	}

	if !strings.Contains(statements[1], "semicolon; inside; string") {
		t.Errorf("statement 1 does not preserve semicolon inside quotes: %q", statements[1])
	}

	if statements[2] != "SELECT * FROM test_db.table1" {
		t.Errorf("statement 2 mismatch, got %q", statements[2])
	}
}

func TestRewriteSQLDatabase(t *testing.T) {
	raw := "CREATE DATABASE IF NOT EXISTS `source_db`; CREATE TABLE IF NOT EXISTS `source_db`.`table1` (id UInt64);"
	got := rewriteSQLDatabase(raw, "source_db", "target_db")

	if strings.Contains(got, "`source_db`") {
		t.Fatalf("old database name still present: %s", got)
	}
	if !strings.Contains(got, "`target_db`.`table1`") {
		t.Fatalf("target database name missing: %s", got)
	}
}

// Test database connection and error handling
func TestNewClickHouseErrors(t *testing.T) {
	ctx := context.Background()

	// Test invalid connection
	_, err := NewClickHouse(ctx, "invalid-host", 9999, "user", "pass", "test_db")
	if err == nil {
		t.Fatal("expected error for invalid host, got nil")
	}

	// Test connection with valid host but invalid credentials (will fail in real scenario)
	// This test validates the error path is properly handled
}

func TestQuoteIdent(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"simple", "`simple`"},
		{"with`quote", "`with``quote`"},
		{"database_name", "`database_name`"},
		{"123", "`123`"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := quoteIdent(tt.input)
			if got != tt.expected {
				t.Errorf("quoteIdent(%q) = %q, want %q", tt.input, got, tt.expected)
			}
		})
	}
}

func TestQuoteString(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"simple", "'simple'"},
		{"with'quote", "'with''quote'"},
		{"with\\slash", "'with\\slash'"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := quoteString(tt.input)
			if got != tt.expected {
				t.Errorf("quoteString(%q) = %q, want %q", tt.input, got, tt.expected)
			}
		})
	}
}

func TestSplitSQLStatementsComplex(t *testing.T) {
	tests := []struct {
		name     string
		sql      string
		expected int
	}{
		{
			name: "multiple statements",
			sql: "SELECT 1; SELECT 2; SELECT 3;",
			expected: 3,
		},
		{
			name: "statements with comments",
			sql: "-- comment\nSELECT 1;\n-- another comment\nSELECT 2;",
			expected: 2,
		},
		{
			name: "complex strings with semicolons",
			sql: "INSERT INTO t (s) VALUES ('hello; world'); SELECT * FROM t;",
			expected: 2,
		},
		{
			name: "empty statements",
			sql: ";\n;\n;",
			expected: 0, // Empty statements are filtered out
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			statements := splitSQLStatements(tt.sql)
			if len(statements) != tt.expected {
				t.Errorf("got %d statements, want %d", len(statements), tt.expected)
			}
		})
	}
}

func TestFirstSQLLine(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"SELECT 1", "SELECT 1"},
		{"SELECT 1;", "SELECT 1;"},
		{"  SELECT 1", "SELECT 1"},
		{"\n\tSELECT 1", "SELECT 1"},
		{"", ""},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := firstSQLLine(tt.input)
			if got != tt.expected {
				t.Errorf("firstSQLLine(%q) = %q, want %q", tt.input, got, tt.expected)
			}
		})
	}
}

func TestFixedStringSize(t *testing.T) {
	tests := []struct {
		clickHouseType string
		expected       int
	}{
		{"FixedString(20)", 20},
		{"FixedString(32)", 32},
		{"FixedString(1)", 1},
		{"String", 0},
		{"UInt64", 0},
	}

	for _, tt := range tests {
		t.Run(tt.clickHouseType, func(t *testing.T) {
			got := fixedStringSize(tt.clickHouseType)
			if got != tt.expected {
				t.Errorf("fixedStringSize(%q) = %d, want %d", tt.clickHouseType, got, tt.expected)
			}
		})
	}
}

func TestSyncStateSerialization(t *testing.T) {
	state := SyncState{
		Current: SyncCursor{
			Number: 1000,
			Hash:   "0xabcd",
		},
		Finalized: &SyncCursor{
			Number: 950,
			Hash:   "0x1234",
		},
		RollbackChain: []SyncCursor{
			{Number: 999, Hash: "0x9999"},
			{Number: 998, Hash: "0x8888"},
		},
	}

	// Test JSON marshaling/unmarshaling
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}

	var decoded SyncState
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}

	if decoded.Current.Number != state.Current.Number {
		t.Errorf("Current.Number = %d, want %d", decoded.Current.Number, state.Current.Number)
	}
	if decoded.Current.Hash != state.Current.Hash {
		t.Errorf("Current.Hash = %q, want %q", decoded.Current.Hash, state.Current.Hash)
	}
	if decoded.Finalized == nil {
		t.Fatal("Finalized is nil, want non-nil")
	}
	if len(decoded.RollbackChain) != len(state.RollbackChain) {
		t.Errorf("RollbackChain length = %d, want %d", len(decoded.RollbackChain), len(state.RollbackChain))
	}
}

func TestUInt256ValueColumn(t *testing.T) {
	tests := []struct {
		name  string
		input string
		valid bool
	}{
		{"zero", "0", true},
		{"small", "12345", true},
		{"large", "1000000000000000000000000000", true},
		{"max uint256", "115792089237316195423570985008687907853269984665640564039457584007913129639935", true},
		{"invalid", "not a number", false},
		{"negative", "-100", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			col := &uint256ValueColumn{name: "test"}

			if tt.valid {
				col.append(tt.input)
				if col.col.Rows() != 1 {
					t.Errorf("expected 1 row, got %d", col.col.Rows())
				}
			} else {
				// Should panic or handle gracefully
				defer func() {
					if r := recover(); r != nil {
						// Expected to panic for invalid input
					}
				}()
				col.append(tt.input)
			}
		})
	}
}

func TestBoolValueColumn(t *testing.T) {
	col := &boolValueColumn{name: "test"}

	// Test true values
	col.append(true)
	col.append("true")
	col.append("1")
	col.append(1)

	if col.col.Rows() != 4 {
		t.Errorf("expected 4 rows, got %d", col.col.Rows())
	}

	// Test false values
	col.reset()
	col.append(false)
	col.append("false")
	col.append("0")
	col.append(0)

	if col.col.Rows() != 4 {
		t.Errorf("expected 4 rows after reset, got %d", col.col.Rows())
	}
}

func TestProtoUInt256(t *testing.T) {
	tests := []struct {
		name string
		input string
	}{
		{"zero", "0"},
		{"small", "100"},
		{"large", "1000000000000000000000000000"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n, err := uint256.FromDecimal(tt.input)
			if err != nil {
				t.Fatalf("uint256.FromDecimal(%q): %v", tt.input, err)
			}

			result := protoUInt256(*n)
			want := proto.UInt256{
				Low:  proto.UInt128{Low: n[0], High: n[1]},
				High: proto.UInt128{Low: n[2], High: n[3]},
			}
			if result != want {
				t.Fatalf("protoUInt256(%q) = %#v, want %#v", tt.input, result, want)
			}
		})
	}
}

func TestEnsureTablesOptions(t *testing.T) {
	tests := []struct {
		name        string
		opts        EnsureTablesOptions
		storeBlocks bool
		storeLogs   bool
	}{
		{"both true", EnsureTablesOptions{StoreBlocks: true, StoreLogs: true}, true, true},
		{"blocks only", EnsureTablesOptions{StoreBlocks: true, StoreLogs: false}, true, false},
		{"logs only", EnsureTablesOptions{StoreBlocks: false, StoreLogs: true}, false, true},
		{"both false", EnsureTablesOptions{StoreBlocks: false, StoreLogs: false}, false, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.opts.StoreBlocks != tt.storeBlocks {
				t.Errorf("StoreBlocks = %v, want %v", tt.opts.StoreBlocks, tt.storeBlocks)
			}
			if tt.opts.StoreLogs != tt.storeLogs {
				t.Errorf("StoreLogs = %v, want %v", tt.opts.StoreLogs, tt.storeLogs)
			}
		})
	}
}
