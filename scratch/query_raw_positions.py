import requests
from requests.auth import HTTPBasicAuth

url = "https://crypto-clickhouse.clickhouse.com/"
auth = HTTPBasicAuth("crypto", "")
headers = {"Content-Type": "text/plain; charset=UTF-8", "Accept": "*/*"}

query = """
SELECT token_id, amount, avg_price, realized_pnl, total_bought
FROM polymarket.user_positions
WHERE user = '0x6de391f369a4d7f2e93553cbd8939b270269668a' AND total_bought > 0
LIMIT 10
"""

r = requests.post(url, data=query.encode(), headers=headers, auth=auth)
if r.status_code == 200:
    print(r.text)
else:
    print(f"Error {r.status_code}: {r.text}")
