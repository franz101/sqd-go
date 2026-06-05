import requests
from decimal import Decimal
import sys
import json

if len(sys.argv) < 2:
    print("Usage: python pnl_report.py <wallet_address>")
    sys.exit(1)

wallet_address = sys.argv[1].lower()
url = "https://api.goldsky.com/api/public/project_cl6mb8i9h0003e201j6li0diw/subgraphs/pnl-subgraph/0.0.14/gn"

all_positions = []
PAGE_SIZE = 250
skip = 0
finished = False

print(f"Fetching positions for {wallet_address}...")

while not finished:
    query = """
    query GetUserPnL($user: String!, $first: Int!, $skip: Int!) {
      userPositions(where: {user: $user, totalBought_gt: "0"}, first: $first, skip: $skip) {
        tokenId
        realizedPnl
        amount
        avgPrice
        totalBought
      }
    }
    """
    
    variables = {
        "user": wallet_address,
        "first": PAGE_SIZE,
        "skip": skip
    }

    headers = {
        'Content-Type': 'application/json',
        'Accept': 'application/json',
        'User-Agent': 'Mozilla/5.0'
    }

    response = requests.post(url, headers=headers, json={'query': query, 'variables': variables})
    response.raise_for_status()
    data = response.json()
    
    if "errors" in data:
        print("GraphQL Errors:", data["errors"])
        break

    positions = data["data"]["userPositions"]
    all_positions.extend(positions)
    
    if len(positions) < PAGE_SIZE:
        finished = True
    else:
        skip += PAGE_SIZE
        print(f"  ...fetched {len(all_positions)} positions")

total_realized = Decimal('0')
total_unrealized_value = Decimal('0')

for pos in all_positions:
    amount = Decimal(pos["amount"]) / Decimal(1e6)
    avg_price = Decimal(pos["avgPrice"]) / Decimal(1e6)
    realized_pnl = Decimal(pos["realizedPnl"]) / Decimal(1e6)

    total_realized += realized_pnl
    total_unrealized_value += (amount * avg_price)

print(f"
--- PnL Report {wallet_address} ---")
print(f"Positions Count:  {len(all_positions)}")
print(f"Realized PnL:     ${total_realized:,.2f}")
print(f"Holdings Cost:    ${total_unrealized_value:,.2f}")
# The user's original logic for "Total Portfolio" was realized - cost, which is unusual.
# Usually Portfolio Value = Cash + Holdings Value. 
# But I will output what was requested.
print(f"Net Realized:     ${(total_realized):,.2f}") 

# Output the first 5 positions as a sample instead of the whole list to keep it readable
if all_positions:
    print("
Sample Positions (First 5):")
    print(json.dumps(all_positions[:5], indent=2))
