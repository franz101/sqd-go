import requests
import json

goldsky_url = "https://api.goldsky.com/api/public/project_cl6mb8i9h0003e201j6li0diw/subgraphs/pnl-subgraph/0.0.14/gn"

# Target discrepant token: 261231390761996...
# Its hex ID is derived from `261231390761996...` which is `0x39c12c395df55c753143b3684c35831de325999cc72df5604361a46e6653cf77` (from BUGREPORT.md line 101)
# Let's convert `39c12c395df55c753143b3684c35831de325999cc72df5604361a46e6653cf77` to decimal:
tid_dec = str(int("39c12c395df55c753143b3684c35831de325999cc72df5604361a46e6653cf77", 16))

query = """
query {
  u_before: userPosition(id: "0xa0932d9aa1ca003376d1237c799efacb302a1198-""" + tid_dec + """", block: { number: 33605400 }) {
    id amount avgPrice realizedPnl totalBought
  }
}
"""

r = requests.post(goldsky_url, json={'query': query})
print(json.dumps(r.json(), indent=2))
