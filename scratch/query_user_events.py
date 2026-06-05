import urllib.request
import urllib.parse
import json

user_hex = "3CF3E8D5427AED066A7A5926980600F6C3CF87B3"

# We want to check all events involving this user.
# In OrderFilled: Maker is user.
# In FPMMBuy/Sell: buyer/seller is user.
# In Split/Merge: stakeholder is user.
# In Converted: stakeholder is user.

queries = {
    "splits": f"SELECT block_number, hex(conditionId), partition, amount FROM polymarket.conditional_tokens_position_split_events WHERE hex(stakeholder)='{user_hex}'",
    "merges": f"SELECT block_number, hex(conditionId), partition, amount FROM polymarket.conditional_tokens_positions_merge_events WHERE hex(stakeholder)='{user_hex}'",
    "nr_splits": f"SELECT block_number, hex(conditionID), amount FROM polymarket.neg_risk_adapter_position_split_events WHERE hex(stakeholder)='{user_hex}'",
    "nr_merges": f"SELECT block_number, hex(conditionID), amount FROM polymarket.neg_risk_adapter_positions_merge_events WHERE hex(stakeholder)='{user_hex}'",
    "nr_converted": f"SELECT block_number, hex(marketID), indexSet, amount FROM polymarket.neg_risk_adapter_positions_converted_events WHERE hex(stakeholder)='{user_hex}'",
    "order_filled": f"SELECT block_number, hex(makerAssetId), hex(takerAssetId), makerAmountFilled, takerAmountFilled FROM polymarket.exchange_order_filled_events WHERE hex(maker)='{user_hex}'",
    "neg_risk_order_filled": f"SELECT block_number, hex(makerAssetId), hex(takerAssetId), makerAmountFilled, takerAmountFilled FROM polymarket.neg_risk_exchange_order_filled_events WHERE hex(maker)='{user_hex}'",
    "fpmm_buy": f"SELECT block_number, outcomeIndex, outcomeTokensBought, investmentAmount FROM polymarket.fixed_product_market_maker_fpmmbuy_events WHERE hex(buyer)='{user_hex}'",
    "fpmm_sell": f"SELECT block_number, outcomeIndex, outcomeTokensSold, returnAmount FROM polymarket.fixed_product_market_maker_fpmmsell_events WHERE hex(seller)='{user_hex}'"
}

for name, q in queries.items():
    url = "http://localhost:8135/?database=polymarket&query=" + urllib.parse.quote(q)
    req = urllib.request.Request(url, headers={"X-ClickHouse-User": "default", "X-ClickHouse-Key": "sqd-clickhouse"})
    try:
        with urllib.request.urlopen(req) as response:
            res = response.read().decode('utf-8').strip()
            lines = res.split('\n') if res else []
            if lines:
                print(f"--- {name} ({len(lines)} rows) ---")
                for l in lines[:10]:
                    print("  ", l)
                if len(lines) > 10:
                    print("   ...")
    except Exception as e:
        print(f"Error querying {name}: {e}")
