package template

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSQLTemplateParity verifies that template outputs match the old fmt.Sprintf implementations.
// Each test case includes the "old" implementation for direct comparison.

func TestCreateBlocksTable_Parity(t *testing.T) {
	tests := []struct {
		name       string
		db         string
		engine     string
		collapsing bool
	}{
		{
			name:       "MergeTree engine",
			db:         "test_db",
			engine:     "MergeTree()",
			collapsing: false,
		},
		{
			name:       "CollapsingMergeTree engine",
			db:         "test_db",
			engine:     "CollapsingMergeTree(sign)",
			collapsing: true,
		},
		{
			name:       "Database with backtick requiring escape",
			db:         "test`db",
			engine:     "MergeTree()",
			collapsing: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := quoteIdent(tt.db)

			// OLD IMPLEMENTATION (fmt.Sprintf)
			signColumn := ""
			if tt.collapsing {
				signColumn = "sign Int8 DEFAULT 1,"
			}
			oldOutput := fmt.Sprintf(`
CREATE TABLE IF NOT EXISTS %s.blocks (
    chain_id UInt64, block_number UInt64,
    block_timestamp DateTime64(3, 'UTC'), block_hash String,
    %s
    inserted_at DateTime64(3, 'UTC') DEFAULT now64(3)
) ENGINE = %s ORDER BY (chain_id, block_number)`, db, signColumn, tt.engine)

			// NEW IMPLEMENTATION (template)
			newOutput := MustExecute("sql/createBlocksTable", struct {
				DatabaseIdent string
				Engine        string
				Collapsing    bool
			}{
				DatabaseIdent: db,
				Engine:        tt.engine,
				Collapsing:    tt.collapsing,
			})

			// Compare outputs
			assert.Equal(t, normalizeWhitespace(oldOutput), normalizeWhitespace(newOutput),
				"template output should match fmt.Sprintf output")
		})
	}
}

func TestCreateLogsTable_Parity(t *testing.T) {
	tests := []struct {
		name       string
		db         string
		engine     string
		collapsing bool
	}{
		{
			name:       "MergeTree engine",
			db:         "test_db",
			engine:     "MergeTree()",
			collapsing: false,
		},
		{
			name:       "CollapsingMergeTree engine",
			db:         "test_db",
			engine:     "CollapsingMergeTree(sign)",
			collapsing: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := quoteIdent(tt.db)

			// OLD IMPLEMENTATION
			signColumn := ""
			if tt.collapsing {
				signColumn = "sign Int8 DEFAULT 1,"
			}
			oldOutput := fmt.Sprintf(`
				CREATE TABLE IF NOT EXISTS %s.logs (
					chain_id UInt64, block_number UInt64,
					block_timestamp DateTime64(3, 'UTC'), block_hash String,
					transaction_hash FixedString(32), transaction_index UInt64, log_index UInt64,
					address FixedString(20), event_name LowCardinality(String),
					topic0 FixedString(32), params String,
					%s
					inserted_at DateTime64(3, 'UTC') DEFAULT now64(3)
				) ENGINE = %s ORDER BY (chain_id, block_number, transaction_index, log_index)`, db, signColumn, tt.engine)

			// NEW IMPLEMENTATION
			newOutput := MustExecute("sql/createLogsTable", struct {
				DatabaseIdent string
				Engine        string
				Collapsing    bool
			}{
				DatabaseIdent: db,
				Engine:        tt.engine,
				Collapsing:    tt.collapsing,
			})

			assert.Equal(t, normalizeWhitespace(oldOutput), normalizeWhitespace(newOutput))
		})
	}
}

func TestInsertBlocks_Parity(t *testing.T) {
	db := quoteIdent("test_db")

	// OLD
	oldOutput := fmt.Sprintf("INSERT INTO %s.blocks (chain_id, block_number, block_timestamp, block_hash) VALUES", db)

	// NEW
	newOutput := MustExecute("sql/insertBlocks", struct {
		DatabaseIdent string
	}{
		DatabaseIdent: db,
	})

	assert.Equal(t, strings.TrimSpace(oldOutput), strings.TrimSpace(newOutput))
}

func TestInsertLogs_Parity(t *testing.T) {
	db := quoteIdent("test_db")

	// OLD
	oldOutput := fmt.Sprintf("INSERT INTO %s.logs (chain_id, block_number, block_timestamp, block_hash, transaction_hash, transaction_index, log_index, address, event_name, topic0, params) VALUES", db)

	// NEW
	newOutput := MustExecute("sql/insertLogs", struct {
		DatabaseIdent string
	}{
		DatabaseIdent: db,
	})

	assert.Equal(t, strings.TrimSpace(oldOutput), strings.TrimSpace(newOutput))
}

func TestDeleteWhereChain_Parity(t *testing.T) {
	db := quoteIdent("test_db")
	tableName := quoteIdent("events")
	chainID := uint64(1)
	lastBlock := uint64(1000)

	// OLD
	oldOutput := fmt.Sprintf("DELETE FROM %s.%s WHERE chain_id = %d AND block_number > %d SETTINGS lightweight_deletes_sync = 1",
		db, tableName, chainID, lastBlock)

	// NEW
	newOutput := MustExecute("sql/deleteWhereChain", struct {
		DatabaseIdent string
		TableName     string
		ChainID       uint64
		LastBlock     uint64
	}{
		DatabaseIdent: db,
		TableName:     tableName,
		ChainID:       chainID,
		LastBlock:     lastBlock,
	})

	assert.Equal(t, oldOutput, newOutput)
}

func TestDeleteWhereNoChain_Parity(t *testing.T) {
	db := quoteIdent("test_db")
	tableName := quoteIdent("blocks")
	lastBlock := uint64(1000)

	// OLD
	oldOutput := fmt.Sprintf("DELETE FROM %s.%s WHERE block_number > %d SETTINGS lightweight_deletes_sync = 1",
		db, tableName, lastBlock)

	// NEW
	newOutput := MustExecute("sql/deleteWhereNoChain", struct {
		DatabaseIdent string
		TableName     string
		LastBlock     uint64
	}{
		DatabaseIdent: db,
		TableName:     tableName,
		LastBlock:     lastBlock,
	})

	assert.Equal(t, oldOutput, newOutput)
}

func TestInsertSelectFinal_Parity(t *testing.T) {
	db := quoteIdent("test_db")
	tableName := quoteIdent("events")
	columnList := "chain_id, block_number, sign"
	selectExprs := "chain_id, block_number, toInt8(-sign) AS sign"
	whereClause := "chain_id = 1 AND block_number > 1000"

	// OLD
	oldOutput := fmt.Sprintf(
		"INSERT INTO %s.%s (%s) SELECT %s FROM %s.%s FINAL WHERE %s",
		db, tableName, columnList, selectExprs, db, tableName, whereClause,
	)

	// NEW
	newOutput := MustExecute("sql/insertSelectFinal", struct {
		DatabaseIdent string
		TableName     string
		ColumnList    string
		SelectExprs   string
		WhereClause   string
	}{
		DatabaseIdent: db,
		TableName:     tableName,
		ColumnList:    columnList,
		SelectExprs:   selectExprs,
		WhereClause:   whereClause,
	})

	assert.Equal(t, oldOutput, newOutput)
}

func TestTablesWithBlockNumber_Parity(t *testing.T) {
	db := quoteString("test_db")

	// OLD
	oldOutput := fmt.Sprintf(`
	SELECT
		c.table AS table,
		countIf(c.name = 'chain_id') AS has_chain,
		countIf(c.name = 'sign') AS has_sign
	FROM system.columns c
	INNER JOIN system.tables t
		ON c.database = t.database AND c.table = t.name
	WHERE c.database = %s
		AND c.name IN ('block_number', 'chain_id', 'sign')
		AND t.engine NOT LIKE '%%View'
	GROUP BY c.table
	HAVING countIf(c.name = 'block_number') > 0
	ORDER BY c.table`, db)

	// NEW
	newOutput := MustExecute("sql/tablesWithBlockNumber", struct {
		DatabaseString string
	}{
		DatabaseString: db,
	})

	assert.Equal(t, normalizeWhitespace(oldOutput), normalizeWhitespace(newOutput))
}

func TestTableColumns_Parity(t *testing.T) {
	db := quoteString("test_db")
	table := quoteString("events")

	// OLD
	oldOutput := fmt.Sprintf(
		`SELECT name
		 FROM system.columns
		 WHERE database = %s AND table = %s
		 ORDER BY position`,
		db, table,
	)

	// NEW
	newOutput := MustExecute("sql/tableColumns", struct {
		DatabaseString string
		TableString    string
	}{
		DatabaseString: db,
		TableString:    table,
	})

	assert.Equal(t, normalizeWhitespace(oldOutput), normalizeWhitespace(newOutput))
}

func TestGoMod_Parity(t *testing.T) {
	tests := []struct {
		name       string
		modulePath string
		goVersion  string
		sqdPath    string
		sqdVersion string
		sqdReplace string
	}{
		{
			name:       "without replace",
			modulePath: "github.com/example/my-indexer",
			goVersion:  "1.21",
			sqdPath:    "github.com/franz101/sqd-go",
			sqdVersion: "v1.2.3",
			sqdReplace: "",
		},
		{
			name:       "with replace",
			modulePath: "github.com/example/my-indexer",
			goVersion:  "1.21",
			sqdPath:    "github.com/franz101/sqd-go",
			sqdVersion: "v1.2.3",
			sqdReplace: "/Users/franz/dev/sqd-go",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var oldOutput strings.Builder

			// OLD IMPLEMENTATION
			fmt.Fprintf(&oldOutput, "module %s\n\ngo %s\n\nrequire %s %s\n",
				tt.modulePath, tt.goVersion, tt.sqdPath, tt.sqdVersion)
			if tt.sqdReplace != "" {
				fmt.Fprintf(&oldOutput, "\nreplace %s => %s\n", tt.sqdPath, tt.sqdReplace)
			}

			// NEW IMPLEMENTATION
			newOutput := MustExecute("code/goMod", struct {
				ModulePath    string
				GoVersion     string
				SQDModulePath string
				SQDVersion    string
				SQDReplace    string
			}{
				ModulePath:    tt.modulePath,
				GoVersion:     tt.goVersion,
				SQDModulePath: tt.sqdPath,
				SQDVersion:    tt.sqdVersion,
				SQDReplace:    tt.sqdReplace,
			})

			assert.Equal(t, oldOutput.String(), newOutput)
		})
	}
}

func TestEnvFile_Parity(t *testing.T) {
	tests := []struct {
		name     string
		apiToken string
	}{
		{
			name:     "without API token",
			apiToken: "",
		},
		{
			name:     "with API token",
			apiToken: "test-token-12345",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// OLD IMPLEMENTATION
			oldOutput := "CLICKHOUSE_HTTP_PORT=8123\nCLICKHOUSE_NATIVE_PORT=9000\nCLICKHOUSE_USER=default\nCLICKHOUSE_PASSWORD=sqd-clickhouse\n"
			if tt.apiToken != "" {
				oldOutput += "SQD_API_TOKEN=" + tt.apiToken + "\n"
			}

			// NEW IMPLEMENTATION
			newOutput := MustExecute("code/envFile", struct {
				APIToken string
			}{
				APIToken: tt.apiToken,
			})

			assert.Equal(t, oldOutput, newOutput)
		})
	}
}

func TestGraphQLSchema_Parity(t *testing.T) {
	// OLD
	oldOutput := "type Event @entity {\n  id: ID!\n}\n"

	// NEW
	newOutput := MustExecute("code/graphqlSchema", nil)

	assert.Equal(t, oldOutput, newOutput)
}

func TestTemplateNotFound(t *testing.T) {
	_, err := Execute("nonexistent/template", nil)
	require.Error(t, err)
	assert.True(t, IsNotFound(err))
}

func TestMustExecutePanicsOnInvalidTemplate(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("Expected MustExecute to panic on invalid template")
		}
	}()
	MustExecute("nonexistent/template", nil)
}

// Helper functions (copied from database package for parity testing)

func quoteIdent(name string) string {
	return "`" + strings.ReplaceAll(name, "`", "``") + "`"
}

func quoteString(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

// normalizeWhitespace removes leading/trailing whitespace and normalizes
// internal whitespace for comparison purposes.
func normalizeWhitespace(s string) string {
	lines := strings.Split(s, "\n")
	var result []string
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return strings.Join(result, " ")
}
