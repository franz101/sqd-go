
-- Custom tables generated from custom schema definitions.

CREATE TABLE IF NOT EXISTS `polymarket`.`memory_conditions` (
  `id` FixedString(32),
  `oracle` FixedString(20),
  `question_id` FixedString(32),
  `outcome_slot_count` UInt8,
  `resolved` UInt8,
  `payouts` Array(UInt256),
  `updated_at_block` UInt64,
  `updated_at` DateTime64(3, 'UTC') DEFAULT now64(3),
  `block_number` UInt64,
  `transaction_index` UInt64,
  `log_index` UInt64
) ENGINE = ReplacingMergeTree(block_number)
PRIMARY KEY (`id`)
ORDER BY (`id`, `block_number`, `transaction_index`, `log_index`);

CREATE TABLE IF NOT EXISTS `polymarket`.`memory_user_positions` (
  `user` FixedString(20),
  `token_id` FixedString(32),
  `amount` Decimal256(18),
  `avg_price` Decimal256(18),
  `realized_pn_l` Decimal256(18),
  `total_bought` Decimal256(18),
  `updated_at_block` UInt64,
  `updated_at` DateTime64(3, 'UTC') DEFAULT now64(3),
  `block_number` UInt64,
  `transaction_index` UInt64,
  `log_index` UInt64
) ENGINE = ReplacingMergeTree(block_number)
PRIMARY KEY (`user`, `token_id`)
ORDER BY (`user`, `token_id`, `block_number`, `transaction_index`, `log_index`);

CREATE TABLE IF NOT EXISTS `polymarket`.`memory_markets` (
  `id` FixedString(32),
  `question_count` UInt32,
  `question_ids` Array(FixedString(32)),
  `updated_at_block` UInt64,
  `updated_at` DateTime64(3, 'UTC') DEFAULT now64(3),
  `block_number` UInt64,
  `transaction_index` UInt64,
  `log_index` UInt64
) ENGINE = ReplacingMergeTree(block_number)
PRIMARY KEY (`id`)
ORDER BY (`id`, `block_number`, `transaction_index`, `log_index`);

CREATE TABLE IF NOT EXISTS `polymarket`.`memory_neg_risk_events` (
  `id` FixedString(32),
  `question_count` UInt32,
  `question_ids` Array(FixedString(32)),
  `updated_at_block` UInt64,
  `updated_at` DateTime64(3, 'UTC') DEFAULT now64(3),
  `block_number` UInt64,
  `transaction_index` UInt64,
  `log_index` UInt64
) ENGINE = ReplacingMergeTree(block_number)
PRIMARY KEY (`id`)
ORDER BY (`id`, `block_number`, `transaction_index`, `log_index`);

CREATE TABLE IF NOT EXISTS `polymarket`.`memory_fixed_product_market_makers` (
  `id` FixedString(20),
  `condition_id` FixedString(32),
  `collateral_token` FixedString(20),
  `updated_at_block` UInt64,
  `updated_at` DateTime64(3, 'UTC') DEFAULT now64(3),
  `block_number` UInt64,
  `transaction_index` UInt64,
  `log_index` UInt64
) ENGINE = ReplacingMergeTree(block_number)
PRIMARY KEY (`id`)
ORDER BY (`id`, `block_number`, `transaction_index`, `log_index`);
