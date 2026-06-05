import os
import requests
from requests.auth import HTTPBasicAuth

REMOTE_CH_URL  = "https://crypto-clickhouse.clickhouse.com/"
REMOTE_CH_AUTH = ("crypto", "")
HEADERS_JSON = {"Content-Type": "text/plain; charset=UTF-8", "Accept": "*/*"}

def query_ch(sql):
    r = requests.post(REMOTE_CH_URL, params={"result_overflow_mode": "break"},
                      data=sql.encode(), headers=HEADERS_JSON,
                      auth=HTTPBasicAuth(*REMOTE_CH_AUTH), timeout=120)
    print(f"Status: {r.status_code}")
    print(f"Response:\n{r.text}")

# Query map for one of our token IDs
token_id_dec = str(int("0x6ca24c425b866a5ffbba67a2120f48b62501af8453ee0b13ee5362281b582087", 16))
print(f"Token ID Dec: {token_id_dec}")
query_ch(f"SELECT * FROM polymarket.token_id_condition WHERE id = '{token_id_dec}' OR id = '0x6ca24c425b866a5ffbba67a2120f48b62501af8453ee0b13ee5362281b582087'")
