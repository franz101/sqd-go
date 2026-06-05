import requests
from decimal import Decimal
import json

wallet_address = "0x6de391f369a4d7f2e93553cbd8939b270269668a"
url = "https://api.goldsky.com/api/public/project_cl6mb8i9h0003e201j6li0diw/subgraphs/pnl-subgraph/0.0.14/gn"

all_positions = []
PAGE_SIZE = 100
skip = 0
finished = False

print(f"Fetching positions for {wallet_address}...")

while not finished:
    query = """
    query GetUserPnL($user: String!, $first: Int!, $skip: Int!) {
      userPositions(where: {user: $user}, first: $first, skip: $skip) {
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

total_realized = Decimal('0')
total_unrealized_value = Decimal('0')

print("\n--- Positions ---")
for pos in all_positions:
    amount = Decimal(pos["amount"]) / Decimal(1e6)
    avg_price = Decimal(pos["avgPrice"]) / Decimal(1e6)
    realized_pnl = Decimal(pos["realizedPnl"]) / Decimal(1e6)
    total_bought = Decimal(pos["totalBought"]) / Decimal(1e6)

    total_realized += realized_pnl
    total_unrealized_value += (amount * avg_price)
    
    # Format and print the position
    # Convert token ID to hex to easily match
    try:
        val = int(pos["tokenId"])
        hex_val = hex(val)
    except Exception:
        hex_val = pos["tokenId"]
    
    print(f"TokenID: {hex_val} ({pos['tokenId']})")
    print(f"  Amount: {amount}")
    print(f"  AvgPrice: {avg_price}")
    print(f"  RealizedPnL: {realized_pnl}")
    print(f"  TotalBought: {total_bought}")

print(f"\nSummary:")
print(f"Positions Count:  {len(all_positions)}")
print(f"Realized PnL:     ${total_realized:,.6f}")
print(f"Holdings Cost:    ${total_unrealized_value:,.6f}")
print(f"Net Realized:     ${(total_realized):,.6f}") 
