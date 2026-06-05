
-- Custom tables generated from custom schema definitions.

CREATE TABLE IF NOT EXISTS `case_1_lbtc_event_only`.`user_positions` (
  `address` FixedString(20),
  `balance` UInt256,
  `total_in` UInt256,
  `total_out` UInt256,
  `updated_at_block` UInt64,
  `updated_at` DateTime64(3, 'UTC') DEFAULT now64(3),
  `block_number` UInt64,
  `transaction_index` UInt64,
  `log_index` UInt64
) ENGINE = ReplacingMergeTree(block_number)
PRIMARY KEY (`address`)
ORDER BY (`address`, `block_number`, `transaction_index`, `log_index`);
