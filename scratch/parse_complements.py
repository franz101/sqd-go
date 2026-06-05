import decimal

v1 = 0xffffffffffffffffffffffffe54c4bc97acf6acc493e75c2346302cf379ad73a
addr1 = 0x3AD79a37cf026334C2753e49cC6ACf7ac94B4ce5

print(f"v1:   {hex(v1)}")
print(f"addr: {hex(addr1)}")

# 1. 2s complement of address in 160-bit?
print(f"160-bit 2s comp: {hex((1<<160) - addr1)}")
# 2. 2s complement of address in 256-bit?
print(f"256-bit 2s comp: {hex((1<<256) - addr1)}")
# 3. What is (v1 ^ ((1<<256)-1))?
print(f"v1 XOR 256: {hex(v1 ^ ((1<<256)-1))}")
# 4. What is (v1 ^ ((1<<160)-1))?
print(f"v1 XOR 160: {hex(v1 ^ ((1<<160)-1))}")
# 5. Let's see what is 0 - v1
print(f"Neg v1: {hex(((1<<256) - v1) & ((1<<256)-1))}")
