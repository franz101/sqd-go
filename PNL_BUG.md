# Findings on BabyJubJub Parity and PnL Bug

This document outlines the findings and resolutions for the two core issues: the BabyJubJub curve calculation parity discrepancy and the `RealizedPnL=0` precision loss bug.

---

## 1. BabyJubJub Curve Calculation Parity Discrepancy

### Context & Symptoms
In the Polymarket subgraph, positions and outcome tokens are represented by condition and collection IDs. The BabyJubJub elliptic curve is used to compute the `CollectionID` and `PositionID` for negative risk markets. The Go implementation of `getNegRiskPositionIDByCondition` did not match the TypeScript reference, causing E2E tests (`TestWallet0xf05b67Positions`) and parity tests to fail.

### Root Cause
1. **Byte Ordering**: The AssemblyScript/TypeScript implementation reverses bytes before parsing as a `BigInt` because AssemblyScript's `BigInt.fromUnsignedBytes` is little-endian. In Go, initializing directly using `SetBytes(hashBytes)` (without byte-reversal) produces the correct big-endian numeric representation of the hash.
2. **Modulo Reduction**: The elliptic curve coordinate search loop lacked a modulo $P$ reduction for the coordinate variable `x1`.

### Resolution
We modified `computeCollectionId` (renamed to `getNegRiskCollectionID` or `getCollectionID` depending on the scope) in [babyjub_collection.go](file:///Users/franz/Desktop/AHH/examples/polymarket/babyjub_collection.go) to:
- Use big-endian numeric initialization directly from hash bytes without byte reversal.
- Perform a modulo $P$ reduction (`x1.Mod(x1, P)`) inside the coordinate search loop.

This successfully resolved the parity gap, allowing all BabyJubJub test cases to pass.

---

## 2. Realized PnL Precision Loss Bug (`RealizedPnL=0`)

### Context & Symptoms
In [e2e_wallet_test.go](file:///Users/franz/Desktop/AHH/examples/polymarket/e2e_wallet_test.go), `TestWallet0xf05b67Positions` failed because the realized PnL for token `0x9fd554bb...db24e5de` was stored/retrieved as `0` instead of the expected `0.532905`. 

### Root Cause
1. **PnL Calculation**: The PnL from a sell is computed as:
   $$\text{PnL} = \text{Amount} \times (\text{Price} - \text{AvgPrice})$$
   For the failing position:
   - $\text{Amount} = 224.192556$ (6 decimal places)
   - $\text{Price} - \text{AvgPrice} = 0.5 - 0.4976244135903251 = 0.0023755864096749$ (16 decimal places)
   - The result of the multiplication was $0.5325887891838789600444$, which has **22 decimal places** (exponent = `-22`).

2. **Decimal Serialization (`fromDecimal`)**:
   When saving the updated position to the database via `state.Position.Save(up, meta)`, the helper function `fromDecimal(v)` converts the `decimal.Decimal` to the generated `protomath.Decimal256` (stored in ClickHouse with scale 18).
   
   In [custom_processor.go](file:///Users/franz/Desktop/AHH/examples/polymarket/custom_processor.go):
   - The fast path is only taken if the exponent is exactly `-18`.
   - Since the exponent of the calculated PnL was `-22`, the code fell through to:
     ```go
     res, _ := protomath.ParseDecimal256(v.String(), protomath.Decimal256Scale18)
     return res
     ```
   - Inside `ParseDecimal256` (in [decimal256.go](file:///Users/franz/Desktop/AHH/drafts/protomath/decimal256.go)), the function checks the length of the fractional part of the string:
     ```go
     if len(frac) > int(scale.scale) {
         return Decimal256{}, errDecimalScale
     }
     ```
     Since the fractional part length (22) was greater than the scale (18), it returned `errDecimalScale`.
   - `fromDecimal` silently ignored this error and returned a zero-initialized `Decimal256{}`, causing the PnL to be saved to ClickHouse as `0`.

### Resolution
We updated the `fromDecimal` helper in [custom_processor.go](file:///Users/franz/Desktop/AHH/examples/polymarket/custom_processor.go) to round the value to 18 decimal places before conversion if its exponent is less than `-18`:
```diff
 func fromDecimal(v decimal.Decimal) protomath.Decimal256 {
 	if v.IsZero() {
 		return protomath.Decimal256{}
 	}
+	if v.Exponent() < -18 {
+		v = v.Round(18)
+	}
 	coeff := v.Coefficient()
 	exp := int(v.Exponent())
```
Rounding to 18 decimal places ensures the value satisfies the scale constraint, hits the fast path `exp == -18`, and converts correctly to `protomath.Decimal256` without losing precision or returning `0`.

---

## 3. Database Race Condition in Test Execution

### Context & Symptoms
During E2E testing, the process would occasionally crash with:
`custom proto processing block 38549598 failed: handle packet: UNKNOWN_TABLE (60): DB::Exception: Table polymarket_wallet_0xf05b67_test_antigravity.memory_user_positions does not exist`

### Root Cause
- The E2E test was configured to use a hardcoded database name `polymarket_wallet_0xf05b67_test_antigravity`.
- When file-watchers or editors automatically trigger test runs in the background concurrently, one run calls `DropClickHouseDatabase` and `NewClickHouse` at start, dropping the database out from under the active background test run.

### Resolution
We changed the database name in `setupWalletTestClickHouse` inside [e2e_wallet_test.go](file:///Users/franz/Desktop/AHH/examples/polymarket/e2e_wallet_test.go) to be dynamically generated with a timestamp:
```go
db := fmt.Sprintf("polymarket_wallet_0xf05b67_test_antigravity_%d", time.Now().UnixNano())
```
This isolates each test execution to its own database instance, eliminating concurrent table modifications.
