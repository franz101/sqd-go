import urllib.request
import json
import struct

user_addr = "0x7d676a88085f3652b2e2e299cbcb529df6b3102c"
user_hex = user_addr[2:].lower()

query = f"""
SELECT 
    hex(token_id) as tid_hex, 
    amount, 
    avg_price, 
    realized_pn_l, 
    total_bought,
    block_number
FROM polymarket.memory_user_positions 
WHERE hex(user) = '{user_hex.upper()}'
"""

url = "http://localhost:8135/?database=polymarket&query=" + urllib.parse.quote(query)
req = urllib.request.Request(url, headers={"X-ClickHouse-User": "default", "X-ClickHouse-Key": "sqd-clickhouse"})

try:
    with urllib.request.urlopen(req) as response:
        res = response.read().decode('utf-8')
        lines = res.strip().split('\n')
        print(f"Total positions: {len(lines)}")
        print("TID_Hex | Decimal_TID | Amount | Avg_Price | Realized_PnL | Total_Bought | Block")
        for line in lines:
            if not line: continue
            parts = line.split('\t')
            tid_hex = parts[0]
            # Convert 32 bytes hex to large integer
            tid_dec = int(tid_hex, 16)
            amount = float(parts[1])
            avg_price = float(parts[2])
            realized_pnl = float(parts[3])
            total_bought = float(parts[4])
            block = int(parts[5])
            
            tid_str = str(tid_dec)
            if tid_str.startswith("2467648") or tid_str.startswith("1457561") or tid_str.startswith("7085633"):
                print(f"{tid_hex} | {tid_dec} | {amount} | {avg_price} | {realized_pnl} | {total_bought} | {block}")
except Exception as e:
    print("Error:", e)
