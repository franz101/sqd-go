import requests
import json

goldsky_url = "https://api.goldsky.com/api/public/project_cl6mb8i9h0003e201j6li0diw/subgraphs/pnl-subgraph/0.0.14/gn"

query = """
query {
  u1: userPosition(id: "0xd94dc8afca38868d041fe76799ecab506a71fb42-385167050514863041928081391056086691177616020327245197958833157777967264474", block: { number: 36673035 }) {
    id amount avgPrice realizedPnl totalBought
  }
  u_latest: userPosition(id: "0xd94dc8afca38868d041fe76799ecab506a71fb42-385167050514863041928081391056086691177616020327245197958833157777967264474") {
    id amount avgPrice realizedPnl totalBought
  }
}
"""

r = requests.post(goldsky_url, json={'query': query})
print(json.dumps(r.json(), indent=2))
