"""A deliberately slow KServe v2 REST scorer for the isolation load test.

    python3 loadtest/slow_kserve.py [--port 9555] [--delay-ms 0]

Serves POST /v2/models/{name}[/versions/{v}]/infer with FEATURES FP32 [N, F] in and SCORES FP32
[N] out (docs/MODELS.md §1): each score is the row's first feature, after sleeping the current
delay. Model validation needs a fast answer, so the delay starts at --delay-ms (default 0) and
is changed at runtime with POST /admin/delay?ms=2000. Standard library only.
"""

from __future__ import annotations

import argparse
import json
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import parse_qs, urlparse

state = {"delay_ms": 0, "calls": 0}
lock = threading.Lock()


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def _json(self, status: int, body: dict) -> None:
        raw = json.dumps(body).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)

    def do_GET(self) -> None:  # noqa: N802 (http.server API)
        if self.path.startswith("/v2/health"):
            self._json(200, {})
        elif self.path == "/admin/stats":
            with lock:
                self._json(200, dict(state))
        else:
            self._json(404, {"error": "not found"})

    def do_POST(self) -> None:  # noqa: N802
        url = urlparse(self.path)
        body = self.rfile.read(int(self.headers.get("Content-Length") or 0))
        if url.path == "/admin/delay":
            ms = int(parse_qs(url.query).get("ms", ["0"])[0])
            with lock:
                state["delay_ms"] = ms
            self._json(200, {"delay_ms": ms})
            return
        if not (url.path.startswith("/v2/models/") and url.path.endswith("/infer")):
            self._json(404, {"error": "not found"})
            return
        try:
            req = json.loads(body)
            feats = next(t for t in req["inputs"] if t["name"] == "FEATURES")
            n, f = feats["shape"]
            data = feats["data"]
            if any(isinstance(x, list) for x in data):  # nested rows
                data = [v for row in data for v in row]
            scores = [float(data[i * f]) for i in range(n)]
        except (KeyError, ValueError, StopIteration, TypeError) as e:
            self._json(400, {"error": f"bad request: {e}"})
            return
        with lock:
            state["calls"] += 1
            delay = state["delay_ms"]
        if delay:
            time.sleep(delay / 1000)
        self._json(
            200,
            {
                "model_name": url.path.split("/")[3],
                "outputs": [{"name": "SCORES", "datatype": "FP32", "shape": [n], "data": scores}],
            },
        )

    def log_message(self, *_args) -> None:
        pass


def main() -> None:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--port", type=int, default=9555)
    ap.add_argument("--delay-ms", type=int, default=0)
    args = ap.parse_args()
    state["delay_ms"] = args.delay_ms
    srv = ThreadingHTTPServer(("127.0.0.1", args.port), Handler)
    srv.daemon_threads = True
    print(f"slow kserve on 127.0.0.1:{args.port}, delay {args.delay_ms} ms", flush=True)
    srv.serve_forever()


if __name__ == "__main__":
    main()
