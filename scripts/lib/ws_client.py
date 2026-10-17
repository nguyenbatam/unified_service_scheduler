"""Minimal WebSocket client (stdlib only) for the smoke and chaos scripts.

Text frames only; answers the server's pings so its keepalive does not drop the socket. Used from
heredocs run with ``python3 -I`` the same way as smoke_http (argv[1] is this directory):

    python3 -I - "$ROOT/scripts/lib" ... <<'PY'
    import sys; sys.path.insert(0, sys.argv.pop(1))
    from ws_client import WSClient
"""

import socket
import struct
import time


class WSClient:
    def __init__(self, host, port, path="/api/v1/ws/dispatch"):
        self.sock = socket.create_connection((host, port), timeout=10)
        key = "dGhlIHNhbXBsZSBub25jZQ=="
        self.sock.sendall(
            (
                f"GET {path} HTTP/1.1\r\n"
                f"Host: {host}:{port}\r\n"
                "Upgrade: websocket\r\n"
                "Connection: Upgrade\r\n"
                f"Sec-WebSocket-Key: {key}\r\n"
                "Sec-WebSocket-Version: 13\r\n\r\n"
            ).encode()
        )
        buf = b""
        while b"\r\n\r\n" not in buf:
            chunk = self.sock.recv(4096)
            if not chunk:
                raise RuntimeError("WS handshake closed")
            buf += chunk
        head, self.buf = buf.split(b"\r\n\r\n", 1)
        if b"101" not in head.split(b"\r\n", 1)[0]:
            raise RuntimeError(f"WS upgrade failed: {head[:200]!r}")

    def _need(self, n, deadline):
        while len(self.buf) < n:
            self.sock.settimeout(max(0.05, deadline - time.time()))
            try:
                chunk = self.sock.recv(4096)
            except socket.timeout:
                return False
            if not chunk:
                raise RuntimeError("WS closed")
            self.buf += chunk
        return True

    def recv_text(self, deadline):
        while time.time() < deadline:
            if not self._need(2, deadline):
                continue
            op = self.buf[0] & 0x0F
            masked = bool(self.buf[1] & 0x80)
            length = self.buf[1] & 0x7F
            idx = 2
            if length == 126:
                if not self._need(4, deadline):
                    continue
                length = struct.unpack("!H", self.buf[2:4])[0]
                idx = 4
            elif length == 127:
                if not self._need(10, deadline):
                    continue
                length = struct.unpack("!Q", self.buf[2:10])[0]
                idx = 10
            mask_len = 4 if masked else 0
            if not self._need(idx + mask_len + length, deadline):
                continue
            mask = self.buf[idx : idx + mask_len] if masked else None
            idx += mask_len
            payload = bytearray(self.buf[idx : idx + length])
            self.buf = self.buf[idx + length :]
            if mask:
                for i in range(len(payload)):
                    payload[i] ^= mask[i % 4]
            if op == 0x1:
                return payload.decode()
            if op == 0x8:
                raise RuntimeError("WS close frame")
            if op == 0x9:
                # Reply to server ping so keepalive does not drop the client.
                plen = len(payload)
                if plen < 126:
                    header = bytes([0x8A, 0x80 | plen])
                elif plen < 65536:
                    header = bytes([0x8A, 0x80 | 126]) + struct.pack("!H", plen)
                else:
                    header = bytes([0x8A, 0x80 | 127]) + struct.pack("!Q", plen)
                mask = b"\x00\x00\x00\x00"
                self.sock.sendall(header + mask + bytes(payload))
                continue
            # skip pong/binary
        raise TimeoutError("WS text timeout")

    def close(self):
        try:
            self.sock.close()
        except OSError:
            pass
