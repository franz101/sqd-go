https://data-api.polymarket.com/value?user=0xfb81f27f1c8758d477332f8e751322c424da1cf3

# THE POSITIONS VALUE
[{"user":"0xfb81f27f1c8758d477332f8e751322c424da1cf3","value":16934.843942644504}]
https://user-pnl-api.polymarket.com/user-pnl?user_address=0xfb81f27f1c8758d477332f8e751322c424da1cf3
[{"t":1765998000,"p":90974.914},{"t":1766001600,"p":90944.1},{"t":1766005200,"p":90652.43},{"t":1766008800,"p":90939.69},{"t":1766012400,"p":90902.336},{"t":1766016000,"p":90817.586},{"t":1766019600,"p":90758.03},{"t":1766023200,"p":90743.36},{"t":1766026800,"p":90902.125},{"t":1766030400,"p":90876.375},{"t":1766034000,"p":90873.914},{"t":1766037600,"p":90809.586},{"t":1766041200,"p":90889.53},{"t":1766044800,"p":90910.16},{"t":1766048400,"p":90896.55},{"t":1766052000,"p":90892.016},{"t":1766055600,"p":90879.43},{"t":1766059200,"p":90810.4},{"t":1766062800,"p":90850.79},{"t":1766066400,"p":90934.65},{"t":1766070000,"p":90927.34},{"t":1766073600,"p":90920.1},{"t":1766077200,"p":90907.75}] where p  of the last row i tem is the PNL

USERS:
0x59d59ae47618f3b340482d175dfc8cf13b2ba273
0xce66940dfe6dc18bc151d66d52a66eb2121bcc64
0x79add3f87e377b0899b935472c07d2c3816ba9f1
0xa1cc5447893617240a80c098783f56304eff3ddb

import requests
from decimal import Decimal
import sys

if len(sys.argv) < 2:
    print("Usage: python script.py <wallet_address>")
    sys.exit(1)
# Run the script
wallet_address = "0x78f329afce2bf9100882e0ee1f83b1e1050c9529".lower()
wallet_address = "0x00000000000050BA7C429821E6D66429452BA168".lower()
wallet_address = "0x2c7904b3c78573f8190e95a354129bd197a01f8b".lower()
wallet_address = "0x268e68979d8e455dae9b40332dc24271aa79bb7f".lower()
wallet_address = "0x78f329afce2bf9100882e0ee1f83b1e1050c9529".lower()
#wallet_address = sys.argv[1]

url = "https://api.goldsky.com/api/public/project_cl6mb8i9h0003e201j6li0diw/subgraphs/pnl-subgraph/0.0.14/gn"
finished = False
PAGE  = 0
MAX_PAGE_SIZE = 250
while not finished:
  # GraphQL Query
  query = """
  query GetUserPnL($user: String!) {
    userPositions(where: {user: $user, totalBought_gt: "0"}, first:""" + str(MAX_PAGE_SIZE)+""", offset: """ + str(PAGE) +""") {
      tokenId
      realizedPnl
      amount
      avgPrice
      totalBought
    }
  }
  """

  variables = {"user": wallet_address.lower()}


  headers = {
  'Content-Type': 'application/json',
  'Accept': 'application/json, multipart/mixed',
  'User-Agent': 'Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/26.2 Safari/605.1.15',
  'Origin': 'https://api.goldsky.com',
  'Referer': 'https://api.goldsky.com/api/public/project_cl6mb8i9h0003e201j6li0diw/subgraphs/pnl-subgraph/0.0.14/gn',
  #'Cookie': '_ga_PGSF5CGSKR=GS2.1.s1769966949$o5$g1$t1769967104$j50$l0$h0; ph_phc_xnF1pd8YYevvkU72QMBo7tj0L8HZ85GPqUxfH3LjkvY_posthog=%7B%22distinct_id%22%3A%22019aefcb-8900-7848-a5d7-7e40402d235a%22%2C%22%24sesid%22%3A%5B1769967104994%2C%22019c1a40-59f3-776f-8e96-50147a2ff0f8%22%2C1769966950898%5D%2C%22%24initial_person_info%22%3A%7B%22r%22%3A%22%24direct%22%2C%22u%22%3A%22https%3A%2F%2Fgoldsky.com%2Fcase-studies%2Fpolymarket-goldsky%22%7D%7D; _ga=GA1.1.1466268324.1761077683; _clck=1ggs9mo%5E2%5Eg1s%5E0%5E2120; _hp2_id.589307451=%7B%22userId%22%3A%227169359058322360%22%2C%22pageviewId%22%3A%223739628426918078%22%2C%22sessionId%22%3A%221175605535264135%22%2C%22identity%22%3Anull%2C%22trackerVersion%22%3A%224.0%22%7D'
  }
  response = requests.post(url, headers=headers, json={'query': query, 'variables': variables})
  response.raise_for_status()
  data = response.json()
  total_realized = Decimal('0')
  total_unrealized_value = Decimal('0')
  positions = data["data"]["userPositions"]
  for pos in positions:
      # Normalize values from the 6-decimal format (USDC/On-chain standard)
      amount = Decimal(pos["amount"]) / Decimal(1e6)
      avg_price = Decimal(pos["avgPrice"]) / Decimal(1e6)
      realized_pnl = Decimal(pos["realizedPnl"]) / Decimal(1e6)

      # Calculate Realized
      total_realized += realized_pnl

      # Calculate Unrealized Value (Current Amount * Price)
      # Note: If avgPrice is 'current price', this is your current bag value.
      total_unrealized_value += (amount * avg_price)
  if len(positions)< MAX_PAGE_SIZE:
    finished = True
    break
  else:
    PAGE += MAX_PAGE_SIZE
    print(PAGE)

print(f"--- PnL Report {wallet_address} ---")
print(f"Realized PnL:     ${total_realized:,.2f}")
print(f"Holdings Value:   ${total_unrealized_value:,.2f}")
print(f"Total Portfolio:  ${(total_realized - total_unrealized_value):,.2f}")
print(positions)

import requests
from decimal import Decimal
import sys

# Setup wallet
wallet_address = "0x8c2fa256c8690a6651d4db04ee7c1d08031564fa"
url = "https://api.goldsky.com/api/public/project_cl6mb8i9h0003e201j6li0diw/subgraphs/pnl-subgraph/0.0.14/gn"

def fetch_pnl():
    total_realized = Decimal('0')
    total_holdings_value = Decimal('0')
    skip = 0
    PAGE_SIZE = 250
    all_positions = []

    while True:
        # Added 'skip' to actually move through the dataset
        query = """
        query GetUserPnL($user: String!, $skip: Int!, $first: Int!) {
          userPositions(
            where: {user: $user, totalBought_gt: "0"},
            first: $first,
            skip: $skip
          ) {
            tokenId
            realizedPnl
            amount
            avgPrice
          }
        }
        """

        variables = {
            "user": wallet_address.lower(),
            "skip": skip,
            "first": PAGE_SIZE
        }

        response = requests.post(url, json={'query': query, 'variables': variables})
        response.raise_for_status()
        data = response.json()

        positions = data.get("data", {}).get("userPositions", [])
        if not positions:
            break

        for pos in positions:
            amount = Decimal(pos["amount"]) / Decimal(1e6)
            avg_price = Decimal(pos["avgPrice"]) / Decimal(1e6)
            realized_pnl = Decimal(pos["realizedPnl"]) / Decimal(1e6)

            total_realized += realized_pnl
            # This represents the 'cost basis' of current open positions
            total_holdings_value += (amount * avg_price)
            all_positions.append(pos)

        if len(positions) < PAGE_SIZE:
            break

        skip += PAGE_SIZE
        print(f"Fetched {skip} positions...")

    return total_realized, total_holdings_value

realized, holdings = fetch_pnl()

print(f"\n--- PnL Report: {wallet_address} ---")
print(f"Realized PnL:     ${realized:,.2f}")
print(f"Open Cost Basis:  ${holdings:,.2f}")
print(f"Net Equity:       ${(realized + holdings):,.2f}")


FOR DEBUGGING TAKE PEOPLE WITH BELOW 100 TRADES AND NO ACTIVITY AFTER LAST BLOCK

# pip install requests
import requests
from requests.auth import HTTPBasicAuth
import pandas as pd
url = "https://crypto-clickhouse.clickhouse.com/"
params = {
   # "query_id": "2580cab3-71f9-4cd7-88e9-ee35b224469b",
   # "allow_experimental_analyzer": "1",
    "result_overflow_mode": "break",
    "read_overflow_mode": "break",
}

sql = """WITH market_tokens AS (
    -- Step 1: Find all outcome tokens ('Yes' and 'No') for the target market.
    SELECT
        outcome_token_id,
        outcome_name
    FROM polymarket.assets
    WHERE market_id = '524152'
)

, all_trades AS (
    -- Step 2: Combine current and historical trade data.
    SELECT timestamp, maker_asset_id, taker_asset_id, maker_amount_filled, taker_amount_filled
    FROM polymarket.orders_matched
    WHERE maker_amount_filled > 0 AND taker_amount_filled > 0
    UNION ALL
    SELECT timestamp, maker_asset_id, taker_asset_id, maker_amount_filled, taker_amount_filled
    FROM polymarket.orders_matched_old
    WHERE maker_amount_filled > 0 AND taker_amount_filled > 0
)

, trades_with_price AS (
    -- Step 3: Calculate price, volume, and direction for every relevant trade.
    SELECT
        toStartOfHour(t.timestamp) AS hour,
        mt.outcome_name,
        -- Trade volume is the amount of collateral (USDC) exchanged.
        CASE
            WHEN t.taker_asset_id = mt.outcome_token_id THEN t.maker_amount_filled
            ELSE t.taker_amount_filled
        END AS trade_volume,
        -- Determine if the trade hit the bid or the ask side.
        CASE
            WHEN t.taker_asset_id = mt.outcome_token_id THEN 'ask' -- Aggressive BUY of the outcome token.
            ELSE 'bid' -- Aggressive SELL of the outcome token.
        END AS trade_type,
        -- Calculate the price based on the trade direction.
        CASE
            WHEN t.taker_asset_id = mt.outcome_token_id THEN t.maker_amount_filled / t.taker_amount_filled
            ELSE t.taker_amount_filled / t.maker_amount_filled
        END AS trade_price
    FROM all_trades AS t
    JOIN market_tokens AS mt ON t.taker_asset_id = mt.outcome_token_id OR t.maker_asset_id = mt.outcome_token_id
)

-- Step 4: Aggregate and pivot the processed trades into the final hourly report.
SELECT
    hour,
    -- 'Yes' Outcome Columns
    round(maxIf(trade_price, outcome_name = 'Yes' AND trade_type = 'bid'), 4) AS hourly_yes_bid,
    round(minIf(trade_price, outcome_name = 'Yes' AND trade_type = 'ask'), 4) AS hourly_yes_ask,
    round(sumIf(trade_volume, outcome_name = 'Yes'), 2) AS hourly_yes_liquidity,

    -- 'No' Outcome Columns
    round(maxIf(trade_price, outcome_name = 'No' AND trade_type = 'bid'), 4) AS hourly_no_bid,
    round(minIf(trade_price, outcome_name = 'No' AND trade_type = 'ask'), 4) AS hourly_no_ask,
    round(sumIf(trade_volume, outcome_name = 'No'), 2) AS hourly_no_liquidity
FROM trades_with_price
GROUP BY hour
ORDER BY hour DESC
LIMIT 100
FORMAT JSON
"""

headers = {
    "Content-Type": "text/plain; charset=UTF-8",
    "Accept": "*/*",
}

# The curl header had "Authorization: Basic Y3J5cHRvOg==" which is username "crypto" and empty password.
auth = HTTPBasicAuth("crypto", "")

resp = requests.post(
    url,
    params=params,
    data=sql.encode("utf-8"),
    headers=headers,
    auth=auth,
    timeout=60,
)

resp.raise_for_status()
result = resp.json()  # ClickHouse JSON format: {meta, data, rows, statistics}
 # rows


feel free to create a scratchpad.py

CREATE TABLE polymarket.user_positions
(
    `vid` UInt64,
    `id` String,
    `user` String,
    `token_id` String,
    `amount` Decimal(76, 18),
    `avg_price` Decimal(76, 18),
    `realized_pnl` Decimal(76, 18),
    `total_bought` Decimal(76, 18),
    `block_number` Int64,
    `is_deleted` UInt8
)
ENGINE = SharedReplacingMergeTree('/clickhouse/tables/{uuid}/{shard}', '{replica}', vid, is_deleted)
ORDER BY id
SETTINGS index_granularity = 8192

# Sanity Check & Comparison Report (Active Wallets)

Comparing ClickHouse (local) vs Goldsky Subgraph API for active wallets.

## Wallet: `0x29a3b878c7b344fd0a05910439af19d3faa43a51`

- **Sample Size**: Compared 29 positions (out of 354 total)

- **Discrepant Positions in Sample**: 1 / 30

<details><summary>Click to view positions details</summary>


| Token ID | Source | Amount | Avg Price | Realized PnL | Total Bought | Block |
|---|---|---|---|---|---|---|
| `536731836605688...` | Goldsky | 50.5297 | 0.4993 | 0.4135 | 56.8958 | 39611078 |
| | Local | 50.5297 | 0.4993 | 0.4135 | 56.8958 | |
| `248196225172313...` | Goldsky | 50.0000 | 0.5000 | 0.1831 | 56.3661 | 39611078 |
| | Local | 50.0000 | 0.5000 | 0.1831 | 56.3661 | |
| `747993299065555...` | Goldsky | 12.8000 | 0.5000 | 0.0000 | 13.0000 | 39605485 |
| | Local | 12.8000 | 0.5000 | 0.0000 | 13.0000 | |
| `609411281871888...` | Goldsky | 0.0000 | 0.5000 | -3.2050 | 13.0000 | 39605485 |
| | Local | 0.0000 | 0.5000 | -3.2050 | 13.0000 | |
| `965378094208404...` | Goldsky | 5.7530 | 0.4346 | 0.0000 | 5.7530 | 39572212 |
| | Local | 5.7530 | 0.4346 | 0.0000 | 5.7530 | |
| `719416738900848...` | Goldsky | 0.0000 | 0.4730 | 532.6397 | 1010.6383 | 39567738 |
| | Local | 0.0000 | 0.4730 | 532.6383 | 1010.6383 | |
| `594336614733166...` | Goldsky | 0.0000 | 0.5000 | -49.9152 | 100.0000 | 39567738 |
| | Local | 0.0000 | 0.5000 | -49.9151 | 100.0000 | |
| `106229249698740...` | Goldsky | 0.0000 | 0.5000 | -410.6682 | 856.3571 | 39529220 |
| | Local | 0.0000 | 0.5000 | -410.6686 | 856.3571 | |
| `674062109487665...` | Goldsky | 0.0000 | 0.5008 | 424.6263 | 850.6254 | 39529220 |
| | Local | 0.0000 | 0.5008 | 424.6254 | 850.6254 | |
| `403698201454955...` | Goldsky | 0.0000 | 0.0300 | 5.6344 | 310.0546 | 39489218 |
| | Local | 0.0000 | 0.0300 | 5.6345 | 310.0546 | |
| `385402896196604...` | Goldsky | 40.0000 | 0.2500 | 0.0000 | 40.0000 | 39467175 |
| | Local | 40.0000 | 0.2500 | 0.0000 | 40.0000 | |
| `414363224028872...` | Goldsky | 0.0000 | 0.3833 | 3.5393 | 5.7393 | 39428032 |
| | Local | 0.0000 | 0.3833 | 3.5393 | 5.7393 | |
| `943791109331514...` | Goldsky | 0.0000 | 0.5000 | -416.9912 | 835.1700 | 39355973 |
| | Local | 0.0000 | 0.5000 | -416.9911 | 835.1700 | |
| `792174056900912...` | Goldsky | 0.0000 | 0.5000 | 417.5850 | 835.1700 | 39355973 |
| | Local | 0.0000 | 0.5000 | 417.5850 | 835.1700 | |
| `367213812311939...` | Goldsky | 0.0000 | 0.0500 | 0.2053 | 60.0000 | 39342185 |
| | Local | 0.0000 | 0.0500 | 0.2053 | 60.0000 | |
| `971483860345810...` | Goldsky | 0.0000 | 0.5000 | 418.8590 | 1972.7073 | 39342185 |
| | Local | 0.0000 | 0.5000 | 418.8570 | 1972.7073 | |
| `821377965460694...` | Goldsky | 0.0000 | 0.5000 | -417.2632 | 834.9506 | 39342185 |
| | Local | 0.0000 | 0.5000 | -417.2626 | 834.9506 | |
| `894906442447925...` | Goldsky | 0.0000 | 0.5000 | -443.3748 | 888.0000 | 39299568 |
| | Local | 0.0000 | 0.5000 | -443.3743 | 888.0000 | |
| `262777644708103...` | Goldsky | 0.0000 | 0.5000 | 444.7602 | 903.5039 | 39299568 |
| | Local | 0.0000 | 0.5000 | 444.7593 | 903.5039 | |
| `803686699915270...` | Goldsky | 0.0000 | 0.9967 | 2.6796 | 802.0205 | 39299568 |
| | Local | 0.0000 | 0.9967 | 2.6793 | 802.0205 | |
| `772435975784303...` | Goldsky | 0.0000 | 0.0033 | -1.7530 | 801.9344 | 39299568 |
| | Local | 0.0000 | 0.0033 | -1.7535 | 801.9344 | |
| `526853977570272...` | Goldsky | 0.0000 | 0.5000 | -277.5000 | 555.0000 | 39299568 |
| | Local | 0.0000 | 0.5000 | -277.5000 | 555.0000 | |
| `310713728800597...` | Goldsky | 0.0000 | 0.5000 | 277.5000 | 555.0000 | 39299568 |
| | Local | 0.0000 | 0.5000 | 277.5000 | 555.0000 | |
| `561195216190969...` | **Goldsky** | *Not Found* | - | - | - | 39285153 |
| | **Local** | 0.0050 | 0.1154 | -10.7159 | 46.4331 | |
| `949025897750144...` | Goldsky | 0.0000 | 0.5000 | 400.7650 | 801.5301 | 39266706 |
| | Local | 0.0000 | 0.5000 | 400.7650 | 801.5301 | |
| `268732248698891...` | Goldsky | 0.0000 | 0.5000 | -400.4420 | 801.5301 | 39266706 |
| | Local | 0.0000 | 0.5000 | -400.4413 | 801.5301 | |
| `617875002551817...` | Goldsky | 0.0000 | 0.5000 | 388.5000 | 777.0000 | 39262809 |
| | Local | 0.0000 | 0.5000 | 388.5000 | 777.0000 | |
| `356722680863100...` | Goldsky | 0.0000 | 0.5000 | -388.0425 | 777.0000 | 39262809 |
| | Local | 0.0000 | 0.5000 | -388.0420 | 777.0000 | |
| `439759934472112...` | Goldsky | 0.0000 | 0.5000 | 400.3742 | 800.7485 | 39245801 |
| | Local | 0.0000 | 0.5000 | 400.3742 | 800.7485 | |
| `216587144184202...` | Goldsky | 0.0000 | 0.5000 | -400.0510 | 800.7485 | 39245801 |
| | Local | 0.0000 | 0.5000 | -400.0507 | 800.7485 | |

</details>


## Wallet: `0xdc118cb573f1161bb88fb84b0d1b6c30faf82d7a`

- **Sample Size**: Compared 30 positions (out of 339 total)

- **Discrepant Positions in Sample**: 1 / 30

<details><summary>Click to view positions details</summary>


| Token ID | Source | Amount | Avg Price | Realized PnL | Total Bought | Block |
|---|---|---|---|---|---|---|
| `235426868706596...` | Goldsky | 0.0000 | 0.5000 | 4.4944 | 9.0000 | 39615933 |
| | Local | 0.0000 | 0.5000 | 4.4944 | 9.0000 | |
| `407322912377508...` | Goldsky | 0.0000 | 0.4700 | 1.3194 | 2.4894 | 39615933 |
| | Local | 0.0000 | 0.4700 | 1.3194 | 2.4894 | |
| `430844708186953...` | Goldsky | 0.0000 | 0.4932 | -5.6728 | 11.7609 | 39603853 |
| | Local | 0.0000 | 0.4932 | -5.6728 | 11.7609 | |
| `274295319736117...` | Goldsky | 10.4651 | 0.8600 | 0.0000 | 10.4651 | 39603586 |
| | Local | 10.4651 | 0.8600 | 0.0000 | 10.4651 | |
| `719416738900848...` | Goldsky | 0.0000 | 0.7723 | 1.1852 | 5.2052 | 39603487 |
| | Local | 0.0000 | 0.7723 | 1.1852 | 5.2052 | |
| `111427177250008...` | Goldsky | 0.0000 | 0.4300 | -0.8839 | 3.0000 | 39603472 |
| | Local | 0.0000 | 0.4300 | -0.8839 | 3.0000 | |
| `419091943838844...` | Goldsky | 24.6174 | 0.2862 | 0.0000 | 24.6174 | 39586295 |
| | Local | 24.6174 | 0.2862 | 0.0000 | 24.6174 | |
| `980735377692263...` | Goldsky | 0.0000 | 0.0650 | 0.9494 | 6.7018 | 39563793 |
| | Local | 0.0000 | 0.0650 | 0.9494 | 6.7018 | |
| `100405324519105...` | Goldsky | 6.4462 | 0.5430 | 0.0000 | 6.4462 | 39563537 |
| | Local | 6.4462 | 0.5430 | 0.0000 | 6.4462 | |
| `125475537811640...` | Goldsky | 13.9056 | 0.2841 | 0.0000 | 13.9056 | 39563510 |
| | Local | 13.9056 | 0.2841 | 0.0000 | 13.9056 | |
| `921731334801832...` | Goldsky | 4.0000 | 0.0600 | 0.0000 | 4.0000 | 39545761 |
| | Local | 4.0000 | 0.0600 | 0.0000 | 4.0000 | |
| `301296599052008...` | Goldsky | 15.0000 | 0.4200 | 0.0000 | 15.0000 | 39542465 |
| | Local | 15.0000 | 0.4200 | 0.0000 | 15.0000 | |
| `101201868980727...` | Goldsky | 10.0070 | 0.1475 | 0.0000 | 10.0070 | 39535910 |
| | Local | 10.0070 | 0.1475 | 0.0000 | 10.0070 | |
| `193477261688779...` | Goldsky | 2.1429 | 0.0700 | 0.0000 | 2.1429 | 39535457 |
| | Local | 2.1429 | 0.0700 | 0.0000 | 2.1429 | |
| `345319032508735...` | Goldsky | 0.0000 | 0.5230 | -0.0576 | 2.5049 | 39535375 |
| | Local | 0.0000 | 0.5230 | -0.0576 | 2.5049 | |
| `379126177078315...` | Goldsky | 0.0000 | 0.5417 | -0.2588 | 6.2024 | 39535365 |
| | Local | 0.0000 | 0.5417 | -0.2588 | 6.2024 | |
| `126320538059086...` | Goldsky | 14.4381 | 0.1042 | 2.4551 | 20.6405 | 39535365 |
| | Local | 14.4381 | 0.1042 | 2.4551 | 20.6405 | |
| `617790592620072...` | Goldsky | 4.1738 | 0.2396 | 0.0000 | 4.1738 | 39488622 |
| | Local | 4.1738 | 0.2396 | 0.0000 | 4.1738 | |
| `536731836605688...` | Goldsky | 1.0081 | 0.5059 | 0.0000 | 1.0081 | 39488215 |
| | Local | 1.0081 | 0.5059 | 0.0000 | 1.0081 | |
| `129288343864921...` | Goldsky | 1.1498 | 0.8697 | 0.0000 | 1.1498 | 39488205 |
| | Local | 1.1498 | 0.8697 | 0.0000 | 1.1498 | |
| `**107150439623412...**` | **Goldsky** | **30.0059** | **0.3513** | **1.4084** | **42.8710** | 39488107 |
| | **Local** | **30.0009** | **0.3513** | **0.0000** | **30.0009** | |
| | _Diff_ | _0.0050_ | _0.0000_ | _1.4084_ | _12.8701_ | |
| `891169345577863...` | Goldsky | 1.2439 | 0.8039 | 0.0000 | 1.2439 | 39488018 |
| | Local | 1.2439 | 0.8039 | 0.0000 | 1.2439 | |
| `111519876395504...` | Goldsky | 6.0973 | 0.4797 | 0.0000 | 6.0973 | 39487961 |
| | Local | 6.0973 | 0.4797 | 0.0000 | 6.0973 | |
| `890644678044862...` | Goldsky | 0.0000 | 0.2700 | -3.6986 | 13.6986 | 39487666 |
| | Local | 0.0000 | 0.2700 | -3.6986 | 13.6986 | |
| `682009732115487...` | Goldsky | 0.0000 | 0.2900 | -4.0845 | 14.0845 | 39487666 |
| | Local | 0.0000 | 0.2900 | -4.0845 | 14.0845 | |
| `475622977307982...` | Goldsky | 0.0000 | 0.2500 | -3.3333 | 13.3333 | 39487666 |
| | Local | 0.0000 | 0.2500 | -3.3333 | 13.3333 | |
| `804173669656718...` | Goldsky | 0.0000 | 0.3600 | -7.1964 | 19.9900 | 39487666 |
| | Local | 0.0000 | 0.3600 | -7.1964 | 19.9900 | |
| `907903699987598...` | Goldsky | 3.6190 | 0.4421 | 0.0000 | 3.6190 | 39487650 |
| | Local | 3.6190 | 0.4421 | 0.0000 | 3.6190 | |
| `952574480722789...` | Goldsky | 0.0000 | 0.2300 | 5.0000 | 6.4935 | 39446784 |
| | Local | 0.0000 | 0.2300 | 5.0000 | 6.4935 | |
| `110113635134798...` | Goldsky | 0.0000 | 0.1900 | -2.8477 | 14.9877 | 39446784 |
| | Local | 0.0000 | 0.1900 | -2.8477 | 14.9877 | |

</details>

CREATE TABLE polymarket.user_positions
(
    `vid` UInt64,
    `id` String,
    `user` String,
    `token_id` String,
    `amount` Decimal(76, 18),
    `avg_price` Decimal(76, 18),
    `realized_pnl` Decimal(76, 18),
    `total_bought` Decimal(76, 18),
    `block_number` Int64,
    `is_deleted` UInt8
)
ENGINE = SharedReplacingMergeTree('/clickhouse/tables/{uuid}/{shard}', '{replica}', vid, is_deleted)
ORDER BY id
SETTINGS index_granularity = 8192

  ### 2. Perfect Sanity Check Candidates

  Using this query, I identified three flawless candidate wallets:

  1.  0xd94dc8afca38868d041fe76799ecab506a71fb42 :
      • First Trade Block:  36,005,633 
      • Last Trade Block:  36,986,238  (completely dormant since)
      • Conditions Prepared Between:  35,653,563  and  36,835,747  (100% captured locally)
  2.  0x87b63eb65202b56157956de70e8b5805e273a49e :
      • First Trade Block:  36,012,127 
      • Last Trade Block:  37,302,925  (completely dormant since)
      • Conditions Prepared Between:  35,653,563  and  36,835,747  (100% captured locally)
  3.  0xebde3cf2830c36ec41a83774c857748ca07ba62f :
      • First Trade Block:  36,267,852 
      • Last Trade Block:  36,906,290  (completely dormant since)
      • Conditions Prepared Between:  35,773,587  and  36,835,747  (100% captured locally)

  ──────
  ### 3. Verification Results (0.00% Discrepancy Rate)

  I updated the wallets list in  tmp/compare_active.py  to target these three candidates and ran the comparison against the live Goldsky Subgraph
  API.

  The regenerated SANITY.md reports an absolute 0 / 30 discrepant positions (0.0000% error rate) for all three candidates!

  • Wallet  0xd94dc8afca38868d041fe76799ecab506a71fb42 : 0 / 30 discrepant positions (Compared 30 out of 48 total)
  • Wallet  0x87b63eb65202b56157956de70e8b5805e273a49e : 0 / 30 discrepant positions (Compared 30 out of 34 total)
  • Wallet  0xebde3cf2830c36ec41a83774c857748ca07ba62f : 0 / 15 discrepant positions (Compared 15 out of 15 total)



  ### 1. The Perfect Split/Merge Candidate

  • Wallet Address:  0xa0932d9aa1ca003376d1237c799efacb302a1198 
  • First Active Block:  36,351,006 
  • Last Active Block:  36,991,710  (no subsequent activity)
  • Local Split/Merge Events: This wallet has active split and merge transactions captured by the local indexer.
  ──────

  