#!/usr/bin/env bash
# migrate-to-mergetree.sh
#
# Convert every CollapsingMergeTree(sign) event table in the polymarket DB to a
# plain MergeTree with NO sign column, preserving all data and the sort key —
# in place, no re-ingestion. The no-duplicates invariant (prune block_number >
# lastBlock before re-insert) makes Collapsing/Replacing dedup unnecessary for
# event tables, so plain MergeTree is sufficient and rollback collapses to
# lightweight DELETEs.
#
# Per table (smallest first, so we validate on cheap tables before the 42 GiB one):
#   1. CREATE <t>__mt AS <t> ENGINE = MergeTree()   (copies cols + PRIMARY KEY + ORDER BY)
#   2. INSERT INTO <t>__mt SELECT * FROM <t>         (copies rows incl. sign)
#   3. ALTER TABLE <t>__mt DROP COLUMN sign          (metadata-only, instant)
#   4. verify count(<t>__mt) == count(<t>)
#   5. RENAME TABLE <t> TO <t>__bak, <t>__mt TO <t>  (atomic swap)
#   6. DROP TABLE <t>__bak SYNC                       (frees old space)
#
# Resumable: already-migrated tables are MergeTree, so the Collapsing query skips them.

set -euo pipefail

DB="${DB:-polymarket}"
CH_READ() { docker exec sqd-go-clickhouse-1 clickhouse-client --user=default --password=sqd-clickhouse --query "$1"; }
CH_RUN()  { docker exec sqd-go-clickhouse-1 clickhouse-client --user=default --password=sqd-clickhouse --query "$1"; }

# Collapsing event tables, smallest first.
mapfile -t TABLES < <(CH_READ "SELECT name FROM system.tables WHERE database='$DB' AND engine='CollapsingMergeTree' ORDER BY total_bytes ASC FORMAT TSV")

echo "Migrating ${#TABLES[@]} CollapsingMergeTree table(s) to plain MergeTree (no sign column)."
start=$(date +%s)

for t in "${TABLES[@]}"; do
  bak="${t}__bak"
  mt="${t}__mt"
  echo "----- $t -----"
  rows_before=$(CH_READ "SELECT count() FROM \`$DB\`.\`$t\`")
  echo "  rows: $rows_before"

  # clean any leftover from a prior interrupted run
  CH_RUN "DROP TABLE IF EXISTS \`$DB\`.\`$mt\` SYNC" 2>/dev/null || true
  CH_RUN "DROP TABLE IF EXISTS \`$DB\`.\`$bak\` SYNC" 2>/dev/null || true

  # 1. fresh MergeTree copy (preserves columns + sort key from source)
  CH_RUN "CREATE TABLE \`$DB\`.\`$mt\` AS \`$DB\`.\`$t\` ENGINE = MergeTree()"

  # 2. copy data
  CH_RUN "INSERT INTO \`$DB\`.\`$mt\` SELECT * FROM \`$DB\`.\`$t\`"

  # 3. drop the now-vestigial sign column (metadata-only op)
  CH_RUN "ALTER TABLE \`$DB\`.\`$mt\` DROP COLUMN sign"

  # 4. verify row count before the irreversible swap
  rows_after=$(CH_READ "SELECT count() FROM \`$DB\`.\`$mt\`")
  if [ "$rows_before" != "$rows_after" ]; then
    echo "  COUNT MISMATCH: before=$rows_before after=$rows_after — ABORTING (originals left intact)"
    CH_RUN "DROP TABLE IF EXISTS \`$DB\`.\`$mt\` SYNC"
    exit 1
  fi

  # 5. atomic swap, 6. drop backup
  CH_RUN "RENAME TABLE \`$DB\`.\`$t\` TO \`$DB\`.\`$bak\`, \`$DB\`.\`$mt\` TO \`$DB\`.\`$t\`"
  CH_RUN "DROP TABLE \`$DB\`.\`$bak\` SYNC"

  engine=$(CH_READ "SELECT engine FROM system.tables WHERE database='$DB' AND name='$t'")
  has_sign=$(CH_READ "SELECT countIf(name='sign') FROM system.columns WHERE database='$DB' AND table='$t'")
  echo "  -> engine=$engine sign_columns=$has_sign  OK"
done

elapsed=$(( $(date +%s) - start ))
echo "===== ALL DONE in ${elapsed}s ====="
echo "Remaining CollapsingMergeTree tables (should be 0):"
CH_READ "SELECT count() FROM system.tables WHERE database='$DB' AND engine='CollapsingMergeTree'"
