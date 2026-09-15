#!/usr/bin/env python3
"""clamd 协议直测：PING + INSTREAM 扫描 EICAR。"""
import socket

def cmd(payload: bytes) -> bytes:
    s = socket.create_connection(('127.0.0.1', 3310), timeout=60)
    s.sendall(payload)
    out = b''
    s.settimeout(60)
    try:
        while True:
            chunk = s.recv(4096)
            if not chunk:
                break
            out += chunk
            if out.endswith(b'\x00'):
                break
    except socket.timeout:
        pass
    s.close()
    return out

print('PING ->', cmd(b'zPING\x00'))
print('PING(legacy, no z) ->', cmd(b'PING\x00'))
eicar = b'X5O!P%@AP[4\\PZX54(P^)7CC)7}$EICAR-STANDARD-ANTIVIRUS-TEST-FILE!$H+H*'
payload = b'zINSTREAM\x00' + len(eicar).to_bytes(4, 'big') + eicar + (0).to_bytes(4, 'big')
print('INSTREAM(eicar) ->', cmd(payload))
