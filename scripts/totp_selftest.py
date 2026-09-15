import hmac, hashlib, struct, base64

def totp(secret, t):
    key = base64.b32decode(secret + '=' * (-len(secret) % 8))
    t = struct.pack('>Q', t // 30)
    h = hmac.new(key, t, hashlib.sha1).digest()
    o = h[-1] & 15
    return '%06d' % ((struct.unpack('>I', h[o:o+4])[0] & 0x7fffffff) % 100000)

# RFC 6238 SHA1 向量（8 位截 6 位）：T=59 -> 287082，T=1111111109 -> 081804
print('T59:', totp('GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ', 59))
print('T1111111109:', totp('GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ', 1111111109))
