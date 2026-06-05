import requests
import json
clickhouse_url = "http://127.0.0.1:8135/"

# Find the hex token IDs for user `0x0b9a5211a680aa80b342caaa6325bbf5725f332d`
query = """
SELECT lower(hex(token_id)) as tokenIdHex FROM polymarket.memory_user_positions 
WHERE user = unhex('0b9a5211a680aa80b342caaa6325bbf5725f332d')
FORMAT JSON
"""
r = requests.post(clickhouse_url, auth=("default", "sqd-clickhouse"), params={"database": "polymarket"}, data=query)
data = r.json().get("data", [])

print("Dec -> Hex mapping for 0x0b9a...:")
for row in data:
    tid_hex = row["tokenIdHex"]
    tid_dec = str(int(tid_hex, 16))
    if tid_dec.startswith("141715") or tid_dec.startswith("131576"):
        print(f"Dec: {tid_dec} -> Hex: {tid_hex}")
