"""Private, deterministic subscription/origin/SOCKS5/DNS/UDP fixtures."""
import base64
import collections
import ipaddress
import json
import os
import select
import socket
import socketserver
import struct
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

STATE = {"version": 1, "mode": "yaml", "bad_provider": False, "rule_version": 1}
COUNTS = collections.Counter()
ADDRESS = os.environ.get("FIXTURE_IP", "10.231.78.2")


def subscription():
    name = "fixture-v" + str(STATE["version"])
    if STATE["mode"] == "invalid":
        return b"proxies: [broken"
    if STATE["mode"] in ("base64", "uri"):
        body = f"socks5://{ADDRESS}:1080#{name}\n".encode()
        return base64.b64encode(body) if STATE["mode"] == "base64" else body
    return f"""proxies:
  - {{name: {name}, type: socks5, server: {ADDRESS}, port: 1080, udp: true}}
proxy-groups:
  - {{name: PROXY, type: select, proxies: [{name}]}}
rule-providers:
  local-rules:
    type: http
    behavior: domain
    url: http://{ADDRESS}:8000/rules
rules:
  - RULE-SET,local-rules,DIRECT
  - IP-ASN,13335,DIRECT,no-resolve
  - MATCH,PROXY
dns:
  enable: true
  enhanced-mode: redir-host
  nameserver: [udp://{ADDRESS}:5353]
  default-nameserver: [{ADDRESS}]
""".encode()


class HTTP(BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def do_GET(self):
        path = self.path.split("?", 1)[0]
        if path == "/subscription":
            tag = f'"{STATE["mode"]}-{STATE["version"]}"'
            if self.headers.get("If-None-Match") == tag:
                COUNTS["subscription_304"] += 1
                self.send_response(304)
                self.end_headers()
                return
            body = subscription()
            self.send_response(200)
            self.send_header("ETag", tag)
            # Some real providers return YAML with this incorrect media type.
            self.send_header("Content-Type", "text/html")
        elif path == "/rules":
            body = b"invalid: true" if STATE["bad_provider"] else f"payload: ['+.direct{STATE['rule_version']}.test']\n".encode()
            self.send_response(200)
        elif path == "/state":
            body = json.dumps({"state": STATE, "counts": dict(COUNTS)}).encode()
            self.send_response(200)
        elif path == "/origin":
            COUNTS["origin"] += 1
            body = json.dumps({"origin": "clashcli-test", "source": self.client_address[0]}).encode()
            self.send_response(200)
        else:
            self.send_response(404)
            body = b"not found"
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_POST(self):
        if self.path != "/state":
            self.send_error(404)
            return
        STATE.update(json.loads(self.rfile.read(int(self.headers["Content-Length"]))))
        self.send_response(204)
        self.end_headers()


def exact(sock, n):
    out = b""
    while len(out) < n:
        b = sock.recv(n - len(out))
        if not b:
            raise EOFError()
        out += b
    return out


def address(sock):
    kind = exact(sock, 1)[0]
    if kind == 1:
        host = socket.inet_ntoa(exact(sock, 4))
    elif kind == 3:
        host = exact(sock, exact(sock, 1)[0]).decode()
    elif kind == 4:
        host = socket.inet_ntop(socket.AF_INET6, exact(sock, 16))
    else:
        raise ValueError("address type")
    return host, struct.unpack("!H", exact(sock, 2))[0]


class SOCKS(socketserver.BaseRequestHandler):
    def handle(self):
        conn = self.request
        try:
            version, methods = exact(conn, 2)
            exact(conn, methods)
            conn.sendall(b"\x05\x00")
            version, command, reserved = exact(conn, 3)
            host, port = address(conn)
            if command == 1:
                # Documentation-only destinations are reachable exclusively through this proxy.
                if host in ("192.0.2.2", "2001:db8::2"):
                    host = "127.0.0.1"
                with socket.create_connection((host, port), timeout=10) as upstream:
                    COUNTS["socks_tcp"] += 1
                    conn.sendall(b"\x05\x00\x00\x01\x00\x00\x00\x00\x00\x00")
                    while True:
                        ready, _, _ = select.select([conn, upstream], [], [], 15)
                        if not ready:
                            return
                        for source in ready:
                            data = source.recv(65536)
                            if not data:
                                return
                            (upstream if source is conn else conn).sendall(data)
            elif command == 3:
                with socket.socket(socket.AF_INET, socket.SOCK_DGRAM) as relay:
                    relay.bind(("0.0.0.0", 0))
                    conn.sendall(b"\x05\x00\x00\x01" + socket.inet_aton(ADDRESS) + struct.pack("!H", relay.getsockname()[1]))
                    client = None
                    while True:
                        ready, _, _ = select.select([conn, relay], [], [], 15)
                        if not ready or conn in ready:
                            return
                        data, sender = relay.recvfrom(65536)
                        if client is None or sender == client:
                            client = sender
                            if data[:3] != b"\x00\x00\x00":
                                continue
                            kind = data[3]
                            if kind == 1:
                                dest, offset = socket.inet_ntoa(data[4:8]), 8
                            elif kind == 3:
                                dest, offset = data[5:5+data[4]].decode(), 5+data[4]
                            else:
                                continue
                            dest_port = struct.unpack("!H", data[offset:offset+2])[0]
                            if dest == "192.0.2.2":
                                dest = "127.0.0.1"
                            reply_header = data[:offset+2]
                            relay.sendto(data[offset+2:], (dest, dest_port))
                            COUNTS["socks_udp"] += 1
                        else:
                            relay.sendto(reply_header + data, client)
        except (OSError, EOFError, ValueError):
            pass


class UDP(socketserver.BaseRequestHandler):
    def handle(self):
        data, sock = self.request
        COUNTS["udp_echo"] += 1
        sock.sendto(data, self.client_address)


class DNS(socketserver.BaseRequestHandler):
    def handle(self):
        data, sock = self.request
        if len(data) < 12:
            return
        COUNTS["dns"] += 1
        end = 12
        while end < len(data) and data[end]:
            end += data[end] + 1
        question = data[12:end+5]
        qtype = data[end+1:end+3]
        answer = b""
        if qtype == b"\x00\x01":
            answer = b"\xc0\x0c\x00\x01\x00\x01\x00\x00\x00\x1e\x00\x04" + ipaddress.ip_address(ADDRESS).packed
        response = data[:2] + b"\x81\x80\x00\x01" + struct.pack("!H", bool(answer)) + b"\x00\x00\x00\x00" + question + answer
        sock.sendto(response, self.client_address)


class ThreadTCP(socketserver.ThreadingTCPServer):
    allow_reuse_address = True
    daemon_threads = True


for server in [ThreadTCP(("0.0.0.0", 1080), SOCKS), socketserver.ThreadingUDPServer(("0.0.0.0", 9999), UDP), socketserver.ThreadingUDPServer(("0.0.0.0", 5353), DNS)]:
    threading.Thread(target=server.serve_forever, daemon=True).start()
ThreadingHTTPServer(("0.0.0.0", 8000), HTTP).serve_forever()
