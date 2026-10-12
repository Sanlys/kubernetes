"""Minimal stand-in for Syncthing's REST API: one folder, and an event queue
the test can append to via POST /test/emit."""
import json
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import parse_qs, urlparse

API_KEY = "test-api-key"
events = []
cond = threading.Condition()


class H(BaseHTTPRequestHandler):
    def log_message(self, *a):
        pass

    def reply(self, obj, code=200):
        body = json.dumps(obj).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_POST(self):
        if self.path == "/test/emit":
            data = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
            with cond:
                events.append({"id": len(events) + 1, "type": data.get("type", "ItemFinished"), "data": data["data"]})
                cond.notify_all()
            return self.reply({"ok": True})
        self.reply({}, 404)

    def do_GET(self):
        if self.headers.get("X-API-Key") != API_KEY:
            return self.reply({"error": "forbidden"}, 403)
        u = urlparse(self.path)
        q = parse_qs(u.query)
        if u.path == "/rest/config/folders":
            return self.reply([{"id": "docs", "path": "/data/docs"}])
        if u.path == "/rest/events":
            since = int(q.get("since", ["0"])[0])
            timeout = float(q.get("timeout", ["60"])[0])
            limit = int(q.get("limit", ["0"])[0])
            with cond:
                if limit:
                    return self.reply(events[-limit:])
                cond.wait_for(lambda: any(e["id"] > since for e in events), timeout=timeout)
                return self.reply([e for e in events if e["id"] > since])
        self.reply({}, 404)


ThreadingHTTPServer(("0.0.0.0", 8384), H).serve_forever()
