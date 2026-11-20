"""Helpers for rehearse-laptop-supervisor.sh.

  rehearsal.py peer PORT LOG
      A stand-in peer pool. The rehearsal daemon holds no account, so it
      forwards every /v1/messages turn here. The answer is a 200 SSE stream
      that lasts X-Stream-Seconds (default 0) seconds, with a ping every half
      second and message_stop at the end.

  rehearsal.py probe OUT TCP_ADDR SOCKET
      Sends a short /v1/messages turn every 250 ms on each of four clients
      (TCP and the Claude socket, each with a fresh connection per turn and
      with one kept-alive connection) until SIGTERM, and records what every
      turn ended as: the HTTP status with its Retry-After, or the connection
      error. Writes one JSON line per turn to OUT and a summary to OUT.summary.
"""
import http.server
import json
import signal
import socket
import sys
import threading
import time


def peer(port, log_path):
    lock = threading.Lock()

    def log(line):
        with lock, open(log_path, "a") as f:
            f.write("%.3f %s\n" % (time.time(), line))

    class Handler(http.server.BaseHTTPRequestHandler):
        protocol_version = "HTTP/1.1"

        def log_message(self, *args):
            pass

        def reply(self, code, obj):
            data = json.dumps(obj).encode()
            self.send_response(code)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)

        def do_GET(self):
            log("GET %s" % self.path)
            if self.path.startswith("/_subrouter/health"):
                return self.reply(200, {"ok": True})
            return self.reply(404, {"error": "not found"})

        def do_POST(self):
            length = int(self.headers.get("Content-Length") or 0)
            self.rfile.read(length)
            secs = float(self.headers.get("X-Stream-Seconds") or 0)
            tag = self.headers.get("X-Rehearsal-Tag") or ""
            if not self.path.startswith("/v1/messages"):
                log("POST %s 404" % self.path)
                return self.reply(404, {"error": "not found"})
            if tag:
                log("stream %s start secs=%s" % (tag, secs))
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream")
            self.send_header("Transfer-Encoding", "chunked")
            self.end_headers()

            def chunk(text):
                data = text.encode()
                self.wfile.write(b"%x\r\n%s\r\n" % (len(data), data))
                self.wfile.flush()

            try:
                chunk('event: message_start\ndata: {"type":"message_start"}\n\n')
                end, n = time.time() + secs, 0
                while time.time() < end:
                    time.sleep(0.5)
                    n += 1
                    chunk('event: ping\ndata: {"type":"ping","n":%d}\n\n' % n)
                chunk('event: message_stop\ndata: {"type":"message_stop"}\n\n')
                self.wfile.write(b"0\r\n\r\n")
                self.wfile.flush()
                if tag:
                    log("stream %s done pings=%d" % (tag, n))
            except OSError as e:
                if tag:
                    log("stream %s broken after %d pings: %s" % (tag, n, e))

    http.server.ThreadingHTTPServer(("127.0.0.1", int(port)), Handler).serve_forever()


class Closed(Exception):
    pass


def read_response(sock):
    buf = b""
    while b"\r\n\r\n" not in buf:
        data = sock.recv(65536)
        if not data:
            raise Closed("closed before a status line" if not buf else "closed inside the headers")
        buf += data
    head, rest = buf.split(b"\r\n\r\n", 1)
    lines = head.decode("latin-1").split("\r\n")
    status = int(lines[0].split()[1])
    headers = {}
    for line in lines[1:]:
        k, _, v = line.partition(":")
        headers[k.strip().lower()] = v.strip()
    if headers.get("transfer-encoding", "").lower() == "chunked":
        while True:
            while b"\r\n" not in rest:
                data = sock.recv(65536)
                if not data:
                    raise Closed("closed inside the body")
                rest += data
            size_line, rest = rest.split(b"\r\n", 1)
            size = int(size_line.split(b";")[0], 16)
            while len(rest) < size + 2:
                data = sock.recv(65536)
                if not data:
                    raise Closed("closed inside the body")
                rest += data
            rest = rest[size + 2:]
            if size == 0:
                break
    elif "content-length" in headers:
        need = int(headers["content-length"])
        while len(rest) < need:
            data = sock.recv(65536)
            if not data:
                raise Closed("closed inside the body")
            rest += data
    else:
        while sock.recv(65536):
            pass
        return status, headers, False
    return status, headers, headers.get("connection", "").lower() != "close"


def turn_bytes(host):
    body = b'{"model":"claude-haiku-4-5","max_tokens":1,"stream":true,"messages":[{"role":"user","content":"x"}]}'
    return (b"POST /v1/messages HTTP/1.1\r\nHost: %s\r\nContent-Type: application/json\r\n"
            b"anthropic-version: 2023-06-01\r\nX-Stream-Seconds: 0\r\nContent-Length: %d\r\n\r\n%s"
            % (host.encode(), len(body), body))


def connect(door, target):
    if door == "tcp":
        host, port = target.rsplit(":", 1)
        s = socket.create_connection((host, int(port)), timeout=10)
    else:
        s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        s.settimeout(10)
        s.connect(target)
    return s


def classify(exc):
    if isinstance(exc, ConnectionRefusedError):
        return "refused"
    if isinstance(exc, FileNotFoundError):
        return "refused (no socket file)"
    if isinstance(exc, socket.timeout):
        return "timeout"
    if isinstance(exc, (ConnectionResetError, BrokenPipeError, Closed)):
        return "closed"
    return "error %s" % type(exc).__name__


def probe(out_path, tcp_addr, sock_path):
    stop = threading.Event()
    lock = threading.Lock()
    out = open(out_path, "a")
    records = []

    def record(door, mode, outcome):
        r = {"t": round(time.time(), 3), "door": door, "mode": mode, "outcome": outcome}
        with lock:
            records.append(r)
            out.write(json.dumps(r) + "\n")
            out.flush()

    def client(door, mode):
        target = tcp_addr if door == "tcp" else sock_path
        host = tcp_addr if door == "tcp" else "api.anthropic.com"
        held = None
        while not stop.is_set():
            try:
                s = held or connect(door, target)
                s.sendall(turn_bytes(host))
                status, headers, reusable = read_response(s)
                ra = headers.get("retry-after")
                record(door, mode, str(status) if status == 200 else "%d retry-after=%s" % (status, ra or "none"))
                if mode == "keepalive" and reusable:
                    held = s
                else:
                    s.close()
                    held = None
            except Exception as e:
                record(door, mode, classify(e))
                if held:
                    held.close()
                held = None
            stop.wait(0.25)

    threads = [threading.Thread(target=client, args=(d, m), daemon=True)
               for d in ("tcp", "socket") for m in ("fresh", "keepalive")]
    for t in threads:
        t.start()
    signal.signal(signal.SIGTERM, lambda *a: stop.set())
    while not stop.is_set():
        stop.wait(0.2)
    for t in threads:
        t.join(timeout=15)

    lines = []
    for door in ("tcp", "socket"):
        for mode in ("fresh", "keepalive"):
            mine = [r for r in records if r["door"] == door and r["mode"] == mode]
            counts = {}
            for r in mine:
                counts[r["outcome"]] = counts.get(r["outcome"], 0) + 1
            other = [r for r in mine if r["outcome"] != "200"]
            span = ""
            if other:
                span = "  non-200 from +%.1fs to +%.1fs" % (other[0]["t"] - records[0]["t"], other[-1]["t"] - records[0]["t"])
            lines.append("%-6s %-9s %s%s" % (door, mode, ", ".join("%s x%d" % kv for kv in sorted(counts.items())), span))
    long_ra = [r for r in records if r["outcome"].startswith("503") and not r["outcome"].endswith("=none")
               and int(r["outcome"].split("=")[1]) > 60]
    bare_503 = [r for r in records if r["outcome"] == "503 retry-after=none"]
    lines.append("503 with Retry-After > 60: %d" % len(long_ra))
    lines.append("503 without Retry-After: %d" % len(bare_503))
    with open(out_path + ".summary", "w") as f:
        f.write("\n".join(lines) + "\n")


if __name__ == "__main__":
    if sys.argv[1] == "peer":
        peer(sys.argv[2], sys.argv[3])
    elif sys.argv[1] == "probe":
        probe(sys.argv[2], sys.argv[3], sys.argv[4])
    else:
        sys.exit("usage: rehearsal.py peer PORT LOG | probe OUT TCP_ADDR SOCKET")
