import decimal

tokens = [
    "-152441292195711919612301016163609326858294077638",
    "-6142032620171811612489115148463091607652120738",
    "-81069948032684419283545531580588120827477024869",
    "100825556865761754322588291650042643502519359196405560550857800530840915622288"
]

for t in tokens:
    v = int(decimal.Decimal(t))
    # If it is negative, interpret as signed 256-bit integer
    if v < 0:
        v_unsigned = v & ((1 << 256) - 1)
    else:
        v_unsigned = v
    print(f"Decimal: {t}")
    print(f"  Hex: {hex(v_unsigned)}")
