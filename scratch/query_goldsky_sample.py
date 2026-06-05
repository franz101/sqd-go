import requests
import json

goldsky_url = "https://api.goldsky.com/api/public/project_cl6mb8i9h0003e201j6li0diw/subgraphs/pnl-subgraph/0.0.14/gn"

query = """
query {
  userPositions(where: { user: "0xd94dc8afca38868d041fe76799ecab506a71fb42" }, first: 5) {
    id
    tokenId
    amount
  }
}
"""

r = requests.post(goldsky_url, json={'query': query})
print(json.dumps(r.json(), indent=2))
