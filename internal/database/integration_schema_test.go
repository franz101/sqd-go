package database

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/ch-go"
	"github.com/ClickHouse/ch-go/proto"
)

// --- NewClickHouse ---------------------------------------------------------

// TestNewClickHouseSuccess verifies the success path: connecting creates the
// target database and returns a usable Store backed by three live
// connections (conn, insertConn, commitConn).
func TestNewClickHouseSuccess(t *testing.T) {
	store := newTestStore(t)

	if store.DB() == "" {
		t.Fatal("DB() returned empty string after successful connect")
	}
	if store.Conn() == nil {
		t.Fatal("Conn() returned nil after successful connect")
	}

	// The database should now show up in system.databases.
	var cnt proto.ColUInt64
	q := fmt.Sprintf("SELECT count() AS c FROM system.databases WHERE name = %s", quoteString(store.DB()))
	if err := store.Conn().Do(context.Background(), ch.Query{Body: q, Result: proto.Results{{Name: "c", Data: &cnt}}}); err != nil {
		t.Fatalf("query system.databases: %v", err)
	}
	if cnt.Rows() == 0 || cnt.Row(0) != 1 {
		t.Fatalf("database %q not present in system.databases after NewClickHouse", store.DB())
	}
}

// TestNewClickHouseUnreachablePort exercises a failure path not already
// covered by TestNewClickHouseErrors (which uses an unresolvable hostname):
// a TCP connection refused on an otherwise-valid host. Verified live: this
// surfaces as "connect clickhouse: dial: dial tcp 127.0.0.1:<port>: connect:
// connection refused".
func TestNewClickHouseUnreachablePort(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	_, err := NewClickHouse(ctx, "127.0.0.1", 59999, "default", "irrelevant", "irrelevant_db")
	if err == nil {
		t.Fatal("expected error connecting to closed/unreachable port, got nil")
	}
	if !strings.Contains(err.Error(), "connect clickhouse") {
		t.Errorf("error = %q, want it to wrap with \"connect clickhouse\"", err.Error())
	}
}

// TestNewClickHouseWrongPassword verifies the authentication-failure path
// against a real, reachable ClickHouse server (distinct from the
// unreachable-host/port cases): ClickHouse rejects the handshake and
// NewClickHouse surfaces that as an error.
func TestNewClickHouseWrongPassword(t *testing.T) {
	if !clickhouseAvailable() {
		t.Skip("ClickHouse not available; set CLICKHOUSE_HOST/CLICKHOUSE_NATIVE_PORT or run with docker")
	}
	host, port, _ := chEnv()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := NewClickHouse(ctx, host, port, "default", "definitely-not-the-password", "wrong_pw_test_db")
	if err == nil {
		t.Fatal("expected error connecting with wrong password, got nil")
	}
	if !strings.Contains(err.Error(), "connect clickhouse") {
		t.Errorf("error = %q, want it to wrap with \"connect clickhouse\"", err.Error())
	}
}

// --- DropClickHouseDatabase -------------------------------------------------

// TestDropClickHouseDatabase verifies that after dropping a database it no
// longer appears in system.databases, and that querying a table inside it
// fails with ClickHouse's UNKNOWN_DATABASE error (verified live).
func TestDropClickHouseDatabase(t *testing.T) {
	if !clickhouseAvailable() {
		t.Skip("ClickHouse not available; set CLICKHOUSE_HOST/CLICKHOUSE_NATIVE_PORT or run with docker")
	}
	host, port, password := chEnv()
	dbName := fmt.Sprintf("db_test_drop_%d", time.Now().UnixNano())
	ctx := context.Background()

	store, err := NewClickHouse(ctx, host, port, "default", password, dbName)
	if err != nil {
		t.Fatalf("NewClickHouse: %v", err)
	}
	if err := store.EnsureTables(ctx); err != nil {
		t.Fatalf("EnsureTables: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close before drop: %v", err)
	}

	if err := DropClickHouseDatabase(ctx, host, port, "default", password, dbName); err != nil {
		t.Fatalf("DropClickHouseDatabase: %v", err)
	}

	probe, err := NewClickHouse(ctx, host, port, "default", password, "default")
	if err != nil {
		t.Fatalf("NewClickHouse(default) probe: %v", err)
	}
	t.Cleanup(func() { _ = probe.Close() })

	var cnt proto.ColUInt64
	q := fmt.Sprintf("SELECT count() AS c FROM system.databases WHERE name = %s", quoteString(dbName))
	if err := probe.Conn().Do(ctx, ch.Query{Body: q, Result: proto.Results{{Name: "c", Data: &cnt}}}); err != nil {
		t.Fatalf("query system.databases post-drop: %v", err)
	}
	if cnt.Rows() == 0 || cnt.Row(0) != 0 {
		t.Fatalf("database %q still present in system.databases after drop", dbName)
	}

	q2 := fmt.Sprintf("SELECT count() AS c FROM %s.sync_state", quoteIdent(dbName))
	err = probe.Conn().Do(ctx, ch.Query{Body: q2, Result: proto.Results{{Name: "c", Data: &cnt}}})
	if err == nil {
		t.Fatal("expected error querying a table in the dropped database, got nil")
	}
	if !strings.Contains(err.Error(), "UNKNOWN_DATABASE") {
		t.Errorf("error = %q, want it to mention UNKNOWN_DATABASE", err.Error())
	}
}

// TestDropClickHouseDatabaseIdempotent verifies DROP DATABASE IF EXISTS
// semantics: dropping a database that doesn't exist is not an error.
func TestDropClickHouseDatabaseIdempotent(t *testing.T) {
	if !clickhouseAvailable() {
		t.Skip("ClickHouse not available; set CLICKHOUSE_HOST/CLICKHOUSE_NATIVE_PORT or run with docker")
	}
	host, port, password := chEnv()
	dbName := fmt.Sprintf("db_test_never_existed_%d", time.Now().UnixNano())
	ctx := context.Background()

	if err := DropClickHouseDatabase(ctx, host, port, "default", password, dbName); err != nil {
		t.Fatalf("DropClickHouseDatabase on nonexistent database: %v", err)
	}
}

// --- Close / Conn / DB accessors -------------------------------------------

// TestStoreAccessors verifies DB() returns the configured database name, the
// three connection accessors return distinct non-nil *ch.Client before
// Close, and Close itself does not error.
func TestStoreAccessors(t *testing.T) {
	store := newTestStore(t)

	if store.Conn() == nil {
		t.Error("Conn() is nil")
	}
	if store.InsertConn() == nil {
		t.Error("InsertConn() is nil")
	}
	if store.CommitConn() == nil {
		t.Error("CommitConn() is nil")
	}
	if store.Conn() == store.InsertConn() {
		t.Error("Conn() and InsertConn() unexpectedly share the same *ch.Client")
	}
	if store.Conn() == store.CommitConn() {
		t.Error("Conn() and CommitConn() unexpectedly share the same *ch.Client")
	}
}

// TestStoreDBReturnsConfiguredName verifies DB() returns exactly the database
// name passed to NewClickHouse.
func TestStoreDBReturnsConfiguredName(t *testing.T) {
	if !clickhouseAvailable() {
		t.Skip("ClickHouse not available; set CLICKHOUSE_HOST/CLICKHOUSE_NATIVE_PORT or run with docker")
	}
	host, port, password := chEnv()
	dbName := fmt.Sprintf("db_test_dbname_%d", time.Now().UnixNano())
	ctx := context.Background()

	store, err := NewClickHouse(ctx, host, port, "default", password, dbName)
	if err != nil {
		t.Fatalf("NewClickHouse: %v", err)
	}
	t.Cleanup(func() {
		_ = store.Close()
		_ = DropClickHouseDatabase(context.Background(), host, port, "default", password, dbName)
	})

	if got := store.DB(); got != dbName {
		t.Fatalf("DB() = %q, want %q", got, dbName)
	}
}

// TestStoreCloseDoesNotError verifies Close() returns nil on a healthy Store.
func TestStoreCloseDoesNotError(t *testing.T) {
	if !clickhouseAvailable() {
		t.Skip("ClickHouse not available; set CLICKHOUSE_HOST/CLICKHOUSE_NATIVE_PORT or run with docker")
	}
	host, port, password := chEnv()
	dbName := fmt.Sprintf("db_test_close_%d", time.Now().UnixNano())
	ctx := context.Background()

	store, err := NewClickHouse(ctx, host, port, "default", password, dbName)
	if err != nil {
		t.Fatalf("NewClickHouse: %v", err)
	}
	t.Cleanup(func() {
		_ = DropClickHouseDatabase(context.Background(), host, port, "default", password, dbName)
	})

	if err := store.Close(); err != nil {
		t.Fatalf("Close() = %v, want nil", err)
	}
}

// --- EnsureTables variants ---------------------------------------------------

// TestEnsureTablesCreatesBlocksLogsAndSyncState verifies the default
// EnsureTables() (non-collapsing, blocks+sync_state only, no logs table:
// EnsureTables -> EnsureTablesWithCollapsing(false) ->
// EnsureTablesWithCollapsingAndOmit(false, false) -> StoreLogs=false).
func TestEnsureTablesCreatesBlocksAndSyncStateOnly(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	if err := store.EnsureTables(ctx); err != nil {
		t.Fatalf("EnsureTables: %v", err)
	}

	mustTableExists(t, store, "blocks", true)
	mustTableExists(t, store, "sync_state", true)
	mustTableExists(t, store, "logs", false)
}

// TestEnsureTablesWithCollapsingAndOmitStoreLogs verifies that passing
// storeLogs=true creates the logs table too.
func TestEnsureTablesWithCollapsingAndOmitStoreLogs(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	if err := store.EnsureTablesWithCollapsingAndOmit(ctx, false, true); err != nil {
		t.Fatalf("EnsureTablesWithCollapsingAndOmit: %v", err)
	}

	mustTableExists(t, store, "blocks", true)
	mustTableExists(t, store, "logs", true)
	mustTableExists(t, store, "sync_state", true)
}

// TestEnsureTablesWithCollapsingAddsSignColumn verifies the collapsing=true
// path uses CollapsingMergeTree(sign) and the blocks/logs tables gain a
// "sign" column (verified live via tableColumns).
func TestEnsureTablesWithCollapsingAddsSignColumn(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	if err := store.EnsureTablesWithCollapsingAndOmit(ctx, true, true); err != nil {
		t.Fatalf("EnsureTablesWithCollapsingAndOmit(collapsing=true): %v", err)
	}

	blocksCols := mustTableColumns(t, store, "blocks")
	if !containsString(blocksCols, "sign") {
		t.Errorf("blocks columns = %v, want \"sign\" present under collapsing=true", blocksCols)
	}
	logsCols := mustTableColumns(t, store, "logs")
	if !containsString(logsCols, "sign") {
		t.Errorf("logs columns = %v, want \"sign\" present under collapsing=true", logsCols)
	}
}

// TestEnsureTablesWithOptionsAllCombinations verifies every StoreBlocks x
// StoreLogs combination produces exactly the expected set of tables, and
// sync_state is always created regardless of the options (verified live:
// sync_state creation is unconditional in EnsureTablesWithOptions).
func TestEnsureTablesWithOptionsAllCombinations(t *testing.T) {
	cases := []struct {
		name   string
		opts   EnsureTablesOptions
		blocks bool
		logs   bool
	}{
		{"both", EnsureTablesOptions{StoreBlocks: true, StoreLogs: true}, true, true},
		{"blocks-only", EnsureTablesOptions{StoreBlocks: true, StoreLogs: false}, true, false},
		{"logs-only", EnsureTablesOptions{StoreBlocks: false, StoreLogs: true}, false, true},
		{"neither", EnsureTablesOptions{StoreBlocks: false, StoreLogs: false}, false, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newTestStore(t)
			ctx := context.Background()

			if err := store.EnsureTablesWithOptions(ctx, false, tc.opts); err != nil {
				t.Fatalf("EnsureTablesWithOptions(%+v): %v", tc.opts, err)
			}

			mustTableExists(t, store, "blocks", tc.blocks)
			mustTableExists(t, store, "logs", tc.logs)
			mustTableExists(t, store, "sync_state", true)
		})
	}
}

// --- ApplySQLFile / ApplySQLFileWithDatabase --------------------------------

// TestApplySQLFile verifies a SQL file with no source-database placeholder is
// applied verbatim against the Store's own database.
func TestApplySQLFile(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	sql := fmt.Sprintf(
		"CREATE TABLE IF NOT EXISTS %s.plain_table (id UInt64) ENGINE = MergeTree() ORDER BY id;",
		quoteIdent(store.DB()),
	)
	path := writeTempSQLFile(t, sql)

	if err := store.ApplySQLFile(ctx, path); err != nil {
		t.Fatalf("ApplySQLFile: %v", err)
	}

	mustTableExists(t, store, "plain_table", true)
}

// TestApplySQLFileWithDatabase verifies the rewriteSQLDatabase placeholder
// substitution: a SQL file written against a fixed `source_db` identifier
// (matching TestRewriteSQLDatabase's exact backtick-quoted-identifier
// syntax) gets its database identifier rewritten to the Store's own
// database, and the resulting table is created there (not in a literal
// "source_db" database, which is verified to never get created).
func TestApplySQLFileWithDatabase(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	sql := "CREATE DATABASE IF NOT EXISTS `source_db`; " +
		"CREATE TABLE IF NOT EXISTS `source_db`.`rewritten_table` (id UInt64) ENGINE = MergeTree() ORDER BY id;"
	path := writeTempSQLFile(t, sql)

	if err := store.ApplySQLFileWithDatabase(ctx, path, "source_db"); err != nil {
		t.Fatalf("ApplySQLFileWithDatabase: %v", err)
	}

	mustTableExists(t, store, "rewritten_table", true)

	// The literal "source_db" database should never have been created: the
	// CREATE DATABASE statement's identifier gets rewritten too.
	var cnt proto.ColUInt64
	q := fmt.Sprintf("SELECT count() AS c FROM system.databases WHERE name = %s", quoteString("source_db"))
	if err := store.Conn().Do(ctx, ch.Query{Body: q, Result: proto.Results{{Name: "c", Data: &cnt}}}); err != nil {
		t.Fatalf("query system.databases: %v", err)
	}
	if cnt.Rows() == 0 || cnt.Row(0) != 0 {
		t.Fatalf("literal \"source_db\" database exists; want rewriteSQLDatabase to have replaced the identifier")
	}
}

// --- tableExists / tableColumns ---------------------------------------------

// TestTableExistsAndColumns verifies tableExists/tableColumns against a real
// table created via EnsureTables, and the nonexistent-table case for both.
func TestTableExistsAndColumns(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	if err := store.EnsureTables(ctx); err != nil {
		t.Fatalf("EnsureTables: %v", err)
	}

	exists, err := store.tableExists(ctx, "sync_state")
	if err != nil {
		t.Fatalf("tableExists(sync_state): %v", err)
	}
	if !exists {
		t.Error("tableExists(sync_state) = false, want true")
	}

	missing, err := store.tableExists(ctx, "this_table_does_not_exist")
	if err != nil {
		t.Fatalf("tableExists(missing): %v", err)
	}
	if missing {
		t.Error("tableExists(this_table_does_not_exist) = true, want false")
	}

	cols, err := store.tableColumns(ctx, "sync_state")
	if err != nil {
		t.Fatalf("tableColumns(sync_state): %v", err)
	}
	wantCols := []string{"chain_id", "last_block", "last_hash", "finalized_block", "finalized_hash", "rollback_chain", "updated_at"}
	if len(cols) != len(wantCols) {
		t.Fatalf("tableColumns(sync_state) = %v, want %v", cols, wantCols)
	}
	for i, c := range wantCols {
		if cols[i] != c {
			t.Errorf("tableColumns(sync_state)[%d] = %q, want %q (full: %v)", i, cols[i], c, cols)
		}
	}

	// tableColumns on a nonexistent table: verified live to return an empty
	// (non-nil-error) slice rather than an error, since the underlying query
	// against system.columns simply returns zero rows.
	missingCols, err := store.tableColumns(ctx, "this_table_does_not_exist")
	if err != nil {
		t.Fatalf("tableColumns(missing) returned error: %v", err)
	}
	if len(missingCols) != 0 {
		t.Errorf("tableColumns(missing) = %v, want empty", missingCols)
	}
}

// --- tablesWithBlockNumber ---------------------------------------------------

// TestTablesWithBlockNumber verifies that after EnsureTablesWithCollapsingAndOmit
// creates blocks+logs+sync_state, tablesWithBlockNumber reports exactly the
// tables that have a block_number column (blocks, logs -- sync_state has no
// block_number column and is correctly excluded), each flagged with
// HasChainID/HasSign matching their real columns.
func TestTablesWithBlockNumber(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	if err := store.EnsureTablesWithCollapsingAndOmit(ctx, true, true); err != nil {
		t.Fatalf("EnsureTablesWithCollapsingAndOmit: %v", err)
	}

	tables, err := store.tablesWithBlockNumber(ctx)
	if err != nil {
		t.Fatalf("tablesWithBlockNumber: %v", err)
	}

	byName := map[string]blockNumberTable{}
	for _, tb := range tables {
		byName[tb.Name] = tb
	}

	if len(tables) != 2 {
		t.Fatalf("tablesWithBlockNumber = %+v, want exactly 2 entries (blocks, logs)", tables)
	}
	blocksTb, ok := byName["blocks"]
	if !ok {
		t.Fatalf("tablesWithBlockNumber missing \"blocks\": %+v", tables)
	}
	if !blocksTb.HasChainID || !blocksTb.HasSign {
		t.Errorf("blocks table = %+v, want HasChainID=true HasSign=true (collapsing=true)", blocksTb)
	}
	logsTb, ok := byName["logs"]
	if !ok {
		t.Fatalf("tablesWithBlockNumber missing \"logs\": %+v", tables)
	}
	if !logsTb.HasChainID || !logsTb.HasSign {
		t.Errorf("logs table = %+v, want HasChainID=true HasSign=true (collapsing=true)", logsTb)
	}
	if _, ok := byName["sync_state"]; ok {
		t.Error("tablesWithBlockNumber unexpectedly includes sync_state, which has no block_number column")
	}
}

// TestTablesWithBlockNumberNonCollapsing verifies HasSign is false when the
// schema was created without collapsing.
func TestTablesWithBlockNumberNonCollapsing(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	if err := store.EnsureTablesWithCollapsingAndOmit(ctx, false, true); err != nil {
		t.Fatalf("EnsureTablesWithCollapsingAndOmit: %v", err)
	}

	tables, err := store.tablesWithBlockNumber(ctx)
	if err != nil {
		t.Fatalf("tablesWithBlockNumber: %v", err)
	}
	for _, tb := range tables {
		if tb.HasSign {
			t.Errorf("table %q HasSign = true, want false under non-collapsing schema", tb.Name)
		}
		if !tb.HasChainID {
			t.Errorf("table %q HasChainID = false, want true (blocks/logs always carry chain_id)", tb.Name)
		}
	}
}

// --- rollbackSQLLoggingEnabled (pure function, no live connection needed) --

// TestRollbackSQLLoggingEnabled verifies the env-var gate values, confirmed
// against the real implementation (strings.ToLower + TrimSpace, matches
// "1"/"true"/"yes"/"on" case-insensitively with surrounding whitespace
// trimmed; anything else, including unset, is false).
func TestRollbackSQLLoggingEnabled(t *testing.T) {
	const envVar = "SQD_LOG_ROLLBACK_SQL"
	old, hadOld := os.LookupEnv(envVar)
	t.Cleanup(func() {
		if hadOld {
			os.Setenv(envVar, old)
		} else {
			os.Unsetenv(envVar)
		}
	})

	tests := []struct {
		value string
		unset bool
		want  bool
	}{
		{unset: true, want: false},
		{value: "", want: false},
		{value: "0", want: false},
		{value: "false", want: false},
		{value: "garbage", want: false},
		{value: "1", want: true},
		{value: "true", want: true},
		{value: "TRUE", want: true},
		{value: "yes", want: true},
		{value: "on", want: true},
		{value: " 1 ", want: true},
	}

	for _, tt := range tests {
		name := tt.value
		if tt.unset {
			name = "<unset>"
		}
		t.Run(name, func(t *testing.T) {
			if tt.unset {
				os.Unsetenv(envVar)
			} else {
				os.Setenv(envVar, tt.value)
			}
			if got := rollbackSQLLoggingEnabled(); got != tt.want {
				t.Errorf("rollbackSQLLoggingEnabled() with %s=%q = %v, want %v", envVar, tt.value, got, tt.want)
			}
		})
	}
}

// --- shared helpers -----------------------------------------------------

func mustTableExists(t *testing.T, store *Store, table string, want bool) {
	t.Helper()
	exists, err := store.tableExists(context.Background(), table)
	if err != nil {
		t.Fatalf("tableExists(%q): %v", table, err)
	}
	if exists != want {
		t.Errorf("tableExists(%q) = %v, want %v", table, exists, want)
	}
}

func mustTableColumns(t *testing.T, store *Store, table string) []string {
	t.Helper()
	cols, err := store.tableColumns(context.Background(), table)
	if err != nil {
		t.Fatalf("tableColumns(%q): %v", table, err)
	}
	return cols
}

func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

func writeTempSQLFile(t *testing.T, sql string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "schema.sql")
	if err := os.WriteFile(path, []byte(sql), 0o644); err != nil {
		t.Fatalf("write temp sql file: %v", err)
	}
	return path
}
