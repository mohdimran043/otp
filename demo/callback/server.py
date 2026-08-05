#!/usr/bin/env python3
"""A callback endpoint, standing in for whatever system asked for the transfer.

It saves what it is given and checks it: the body's own SHA-256 has to match the header the receiver sent.
That comparison is the point of the whole exercise — the bytes that left the sender's disk are the bytes
that arrived here — so an endpoint that merely counted requests would be proving nothing.
"""
import hashlib
import http.server
import json
import pathlib
import socketserver

OUT = pathlib.Path("/deliveries")
OUT.mkdir(parents=True, exist_ok=True)


class Handler(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        length = int(self.headers.get("Content-Length", "0"))
        body = self.rfile.read(length)

        declared = self.headers.get("X-OTP-SHA256", "")
        actual = hashlib.sha256(body).hexdigest()
        filename = self.headers.get("X-OTP-Filename", "delivered.bin")
        transmission = self.headers.get("X-OTP-Transmission-Id", "unknown")

        # Saved under the transmission id as well as the name, so several deliveries of files with the same
        # name do not overwrite each other.
        (OUT / f"{transmission}-{pathlib.Path(filename).name}").write_bytes(body)
        record = {
            "transmission_id": transmission,
            "filename": filename,
            "bytes": len(body),
            "declared_sha256": declared,
            "actual_sha256": actual,
            "match": declared == actual,
        }
        (OUT / f"{transmission}.json").write_text(json.dumps(record, indent=2))
        print(json.dumps(record), flush=True)

        # Refused if the hashes disagree, so a mismatch is reported back rather than absorbed: the receiver
        # records the status it got, and the sender's callback record shows it.
        self.send_response(200 if record["match"] else 422)
        self.end_headers()
        self.wfile.write(b'{"received":true}\n')

    def do_GET(self):
        records = sorted(p.name for p in OUT.glob("*.json"))
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(json.dumps({"deliveries": records}, indent=2).encode())

    def log_message(self, *args):
        pass


class Server(socketserver.ThreadingMixIn, http.server.HTTPServer):
    daemon_threads = True


if __name__ == "__main__":
    print("callback endpoint listening on :9000", flush=True)
    Server(("", 9000), Handler).serve_forever()
