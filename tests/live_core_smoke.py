#!/usr/bin/env python3
"""Isolated end-to-end smoke test for mihomo-guard on macOS.

The test starts the explicitly supplied Mihomo binary with a temporary config,
loopback-only controller/listeners, synthetic HTTP proxies, and local probe
targets. It never reads or writes the user's serving configuration.
"""

from __future__ import annotations

import argparse
import contextlib
import http.client
import http.server
import json
import os
import signal
import socket
import socketserver
import subprocess
import sys
import tempfile
import threading
import time
import urllib.parse
from pathlib import Path
from typing import Any


class Stats:
    def __init__(self) -> None:
        self._lock = threading.Lock()
        self.requests: dict[str, int] = {}

    def hit(self, name: str) -> None:
        with self._lock:
            self.requests[name] = self.requests.get(name, 0) + 1

    def count(self, name: str) -> int:
        with self._lock:
            return self.requests.get(name, 0)

    def snapshot(self) -> dict[str, int]:
        with self._lock:
            return dict(self.requests)


STATS = Stats()


class QuietThreadingHTTPServer(http.server.ThreadingHTTPServer):
    daemon_threads = True

    def handle_error(self, _request: Any, _client_address: Any) -> None:
        return


class ProbeHandler(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def do_HEAD(self) -> None:
        self._respond(body=False)

    def do_GET(self) -> None:
        self._respond(body=True)

    def _respond(self, body: bool) -> None:
        path = urllib.parse.urlsplit(self.path).path
        STATS.hit("Origin " + path)
        if path == "/204":
            self.send_response(http.HTTPStatus.NO_CONTENT)
            self.send_header("Content-Length", "0")
            self.end_headers()
            return
        if path == "/geo":
            payload = b".google.com\n"
        elif path == "/trace":
            payload = b"ip=203.0.113.9\ncolo=LAX\nloc=US\n"
        elif path == "/download":
            # Deliberately ignore Range. The guard must stop at its own cap.
            length = 8 * 1024 * 1024
            self.send_response(http.HTTPStatus.OK)
            self.send_header("Content-Type", "application/octet-stream")
            self.send_header("Content-Length", str(length))
            self.send_header("Connection", "close")
            self.end_headers()
            if body:
                chunk = b"x" * 16384
                with contextlib.suppress(BrokenPipeError, ConnectionResetError):
                    for _ in range(length // len(chunk)):
                        self.wfile.write(chunk)
            return
        else:
            payload = b"not found"
            self.send_response(http.HTTPStatus.NOT_FOUND)
            self.send_header("Content-Length", str(len(payload)))
            self.end_headers()
            if body:
                self.wfile.write(payload)
            return
        self.send_response(http.HTTPStatus.OK)
        self.send_header("Content-Type", "application/octet-stream")
        self.send_header("Content-Length", str(len(payload)))
        self.send_header("Connection", "close")
        self.end_headers()
        if body:
            with contextlib.suppress(BrokenPipeError, ConnectionResetError):
                self.wfile.write(payload)

    def log_message(self, *_args: Any) -> None:
        return


class ProxyServer(socketserver.ThreadingMixIn, http.server.HTTPServer):
    daemon_threads = True
    allow_reuse_address = True

    def __init__(self, address: tuple[str, int], name: str, delay: float):
        self.proxy_name = name
        self.chunk_delay = delay
        super().__init__(address, ProxyHandler)

    def handle_error(self, _request: Any, _client_address: Any) -> None:
        return


class ProxyHandler(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def do_CONNECT(self) -> None:
        STATS.hit(self.server.proxy_name)  # type: ignore[attr-defined]
        try:
            host, port = split_host_port(self.path)
            upstream = socket.create_connection((host, port), timeout=3)
        except (OSError, ValueError):
            self.send_error(http.HTTPStatus.BAD_GATEWAY)
            return
        with upstream:
            self.send_response(http.HTTPStatus.OK, "Connection Established")
            self.end_headers()
            relay(
                self.connection,
                upstream,
                self.server.chunk_delay,  # type: ignore[attr-defined]
            )

    def do_HEAD(self) -> None:
        self._forward(body=False)

    def do_GET(self) -> None:
        self._forward(body=True)

    def _forward(self, body: bool) -> None:
        STATS.hit(self.server.proxy_name)  # type: ignore[attr-defined]
        parsed = urllib.parse.urlsplit(self.path)
        if parsed.scheme not in ("http", ""):
            self.send_error(http.HTTPStatus.BAD_REQUEST)
            return
        host = parsed.hostname or self.headers.get("Host", "").split(":", 1)[0]
        port = parsed.port or 80
        path = urllib.parse.urlunsplit(("", "", parsed.path or "/", parsed.query, ""))
        headers = {
            key: value
            for key, value in self.headers.items()
            if key.lower() not in {"connection", "proxy-connection", "proxy-authorization", "host"}
        }
        upstream = http.client.HTTPConnection(host, port, timeout=3)
        try:
            upstream.request(self.command, path, headers=headers)
            response = upstream.getresponse()
            self.send_response(response.status, response.reason)
            for key, value in response.getheaders():
                if key.lower() not in {"connection", "proxy-connection", "transfer-encoding"}:
                    self.send_header(key, value)
            self.send_header("Connection", "close")
            self.end_headers()
            if body:
                while chunk := response.read(4096):
                    self.wfile.write(chunk)
                    self.wfile.flush()
                    if self.server.chunk_delay:  # type: ignore[attr-defined]
                        time.sleep(self.server.chunk_delay)  # type: ignore[attr-defined]
        except (BrokenPipeError, ConnectionResetError, OSError):
            pass
        finally:
            upstream.close()

    def log_message(self, *_args: Any) -> None:
        return


def split_host_port(value: str) -> tuple[str, int]:
    if value.startswith("["):
        host, _, port = value[1:].partition("]:")
    else:
        host, _, port = value.rpartition(":")
    if not host or not port:
        raise ValueError(value)
    return host, int(port)


def relay(client: socket.socket, upstream: socket.socket, delay: float) -> None:
    def pump(source: socket.socket, destination: socket.socket, chunk_delay: float) -> None:
        try:
            while True:
                data = source.recv(4096)
                if not data:
                    return
                destination.sendall(data)
                if chunk_delay:
                    time.sleep(chunk_delay)
        except (BrokenPipeError, ConnectionResetError, OSError):
            return
        finally:
            with contextlib.suppress(OSError):
                destination.shutdown(socket.SHUT_WR)

    upload = threading.Thread(target=pump, args=(client, upstream, 0.0), daemon=True)
    upload.start()
    pump(upstream, client, delay)


def free_port() -> int:
    with socket.socket() as holder:
        holder.bind(("127.0.0.1", 0))
        return holder.getsockname()[1]


def controller(port: int, secret: str, method: str, path: str, body: Any = None) -> Any:
    connection = http.client.HTTPConnection("127.0.0.1", port, timeout=2)
    headers = {"Authorization": f"Bearer {secret}", "Accept": "application/json"}
    payload = None
    if body is not None:
        payload = json.dumps(body).encode()
        headers["Content-Type"] = "application/json"
    try:
        connection.request(method, path, body=payload, headers=headers)
        response = connection.getresponse()
        data = response.read()
        if response.status < 200 or response.status >= 300:
            raise AssertionError(f"controller {method} {path}: {response.status} {data!r}")
        return json.loads(data) if data else None
    finally:
        connection.close()


def selection(port: int, secret: str, name: str) -> str:
    escaped = urllib.parse.quote(name, safe="")
    return controller(port, secret, "GET", f"/proxies/{escaped}")["now"]


def select_proxy(port: int, secret: str, group: str, node: str) -> None:
    escaped = urllib.parse.quote(group, safe="")
    controller(port, secret, "PUT", f"/proxies/{escaped}", {"name": node})


def request_through_proxy(proxy_port: int, origin_port: int, path: str = "/204") -> None:
    connection = http.client.HTTPConnection("127.0.0.1", proxy_port, timeout=3)
    try:
        connection.request("GET", f"http://127.0.0.1:{origin_port}{path}")
        response = connection.getresponse()
        response.read()
        if response.status != http.HTTPStatus.NO_CONTENT:
            raise AssertionError(
                f"route through listener {proxy_port} returned {response.status}"
            )
    finally:
        connection.close()


def wait_for_core(process: subprocess.Popen[bytes], port: int, secret: str, log: Path) -> None:
    deadline = time.monotonic() + 8
    while time.monotonic() < deadline:
        if process.poll() is not None:
            raise AssertionError(f"isolated Mihomo exited with {process.returncode}:\n{log.read_text(errors='replace')}")
        try:
            version = controller(port, secret, "GET", "/version")
            if version.get("version"):
                return
        except (OSError, AssertionError, json.JSONDecodeError):
            pass
        time.sleep(0.05)
    raise AssertionError(f"isolated Mihomo did not start:\n{log.read_text(errors='replace')}")


def guard_environment() -> dict[str, str]:
    environment = os.environ.copy()
    for name in (
        "HTTP_PROXY",
        "HTTPS_PROXY",
        "ALL_PROXY",
        "http_proxy",
        "https_proxy",
        "all_proxy",
    ):
        environment.pop(name, None)
    environment["NO_PROXY"] = "*"
    return environment


def run_guard(guard: Path, *arguments: str) -> str:
    completed = subprocess.run(
        [str(guard), *arguments],
        check=False,
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        env=guard_environment(),
        timeout=30,
        text=True,
    )
    if completed.returncode != 0:
        raise AssertionError(
            f"mihomo-guard {' '.join(arguments)} exited {completed.returncode}:\n{completed.stdout}"
        )
    return completed.stdout


def run_guard_failure(guard: Path, *arguments: str) -> str:
    completed = subprocess.run(
        [str(guard), *arguments],
        check=False,
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        env=guard_environment(),
        timeout=30,
        text=True,
    )
    if completed.returncode == 0:
        raise AssertionError(
            f"mihomo-guard {' '.join(arguments)} unexpectedly succeeded:\n{completed.stdout}"
        )
    return completed.stdout


def run_recovery_daemon(
    guard: Path,
    config: Path,
    controller_port: int,
    secret: str,
    target_group: str,
    failed_node: str,
    follower_group: str = "",
    stable_groups: dict[str, str] | None = None,
) -> str:
    stable_groups = stable_groups or {}
    process = subprocess.Popen(
        [str(guard), "daemon", "-c", str(config)],
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        env=guard_environment(),
        text=True,
    )
    deadline = time.monotonic() + 6
    recovered = ""
    while time.monotonic() < deadline:
        if process.poll() is not None:
            output, _ = process.communicate()
            raise AssertionError(f"recovery daemon exited before failover:\n{output}")
        current = selection(controller_port, secret, target_group)
        follower = (
            selection(controller_port, secret, follower_group) if follower_group else current
        )
        observed = {
            group: selection(controller_port, secret, group) for group in stable_groups
        }
        changed = {
            group: (expected, observed[group])
            for group, expected in stable_groups.items()
            if observed[group] != expected
        }
        if changed:
            process.kill()
            output, _ = process.communicate(timeout=2)
            raise AssertionError(
                f"recovery of {target_group!r} changed unrelated selectors {changed}:\n{output}"
            )
        if current != failed_node and follower == current:
            recovered = current
            break
        time.sleep(0.04)
    if not recovered:
        process.kill()
        output, _ = process.communicate(timeout=2)
        raise AssertionError(
            f"daemon did not recover unreachable selector {target_group!r}:\n{output}"
        )
    process.send_signal(signal.SIGINT)
    try:
        output, _ = process.communicate(timeout=5)
    except subprocess.TimeoutExpired:
        process.kill()
        output, _ = process.communicate(timeout=2)
        raise AssertionError(f"recovery daemon did not stop after SIGINT:\n{output}")
    if process.returncode != 0:
        raise AssertionError(f"recovery daemon SIGINT exit {process.returncode}:\n{output}")
    return recovered


def interrupt_active_bench(guard: Path, config: Path) -> str:
    origin_before = STATS.count("Origin /download")
    process = subprocess.Popen(
        [str(guard), "bench", "-c", str(config), "--node", "Node Slow"],
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        env=guard_environment(),
        text=True,
    )
    deadline = time.monotonic() + 4
    while STATS.count("Origin /download") == origin_before:
        if process.poll() is not None:
            output, _ = process.communicate()
            raise AssertionError(f"bench exited before its download began:\n{output}")
        if time.monotonic() >= deadline:
            process.kill()
            output, _ = process.communicate()
            raise AssertionError(f"bench did not begin a download:\n{output}")
        time.sleep(0.01)
    time.sleep(0.08)
    process.send_signal(signal.SIGINT)
    try:
        output, _ = process.communicate(timeout=5)
    except subprocess.TimeoutExpired:
        process.kill()
        output, _ = process.communicate(timeout=2)
        raise AssertionError(f"bench did not stop after SIGINT:\n{output}")
    if process.returncode != 0:
        raise AssertionError(f"cancelled bench exit {process.returncode}:\n{output}")
    return output


def write_core_config(
    path: Path,
    controller_port: int,
    listener_port: int,
    stale_listener_port: int,
    selector_listener_port: int,
    chain_listener_port: int,
    secret: str,
    proxies: dict[str, int],
) -> None:
    dead, slow, fast, manual, exit_proxy, hk_dead, hk_slow, tw_fast = (
        proxies["Node Dead"],
        proxies["Node Slow"],
        proxies["Node Fast"],
        proxies["Node Manual"],
        proxies["Exit Proxy"],
        proxies["HK Dead"],
        proxies["HK Slow"],
        proxies["TW Fast"],
    )
    provider_path = path.parent / "serving-provider.yaml"
    provider_path.write_text(
        json.dumps(
            {
                "proxies": [
                    {
                        "name": "Node Dead",
                        "type": "http",
                        "server": "127.0.0.1",
                        "port": dead,
                    },
                    {
                        "name": "Node Slow",
                        "type": "http",
                        "server": "127.0.0.1",
                        "port": slow,
                    },
                    {
                        "name": "Node Fast",
                        "type": "http",
                        "server": "127.0.0.1",
                        "port": fast,
                    },
                    {
                        "name": "Node Manual",
                        "type": "http",
                        "server": "127.0.0.1",
                        "port": manual,
                    },
                    {
                        "name": "HK Dead",
                        "type": "http",
                        "server": "127.0.0.1",
                        "port": hk_dead,
                    },
                    {
                        "name": "HK Slow",
                        "type": "http",
                        "server": "127.0.0.1",
                        "port": hk_slow,
                    },
                    {
                        "name": "TW Fast",
                        "type": "http",
                        "server": "127.0.0.1",
                        "port": tw_fast,
                    },
                ]
            }
        )
    )
    path.write_text(
        f"""external-controller: 127.0.0.1:{controller_port}
secret: {json.dumps(secret)}
allow-lan: false
mode: rule
log-level: silent
ipv6: false
geo-auto-update: false
profile:
  store-selected: false
dns:
  enable: false
proxies:
  - {{name: Chain, type: http, server: 127.0.0.1, port: {exit_proxy}, dialer-proxy: Managed}}
proxy-providers:
  synthetic:
    type: file
    path: ./serving-provider.yaml
proxy-groups:
  - {{name: Managed, type: select, use: [synthetic], filter: '^Node (Dead|Slow|Fast)$'}}
  - {{name: Mirror, type: select, use: [synthetic], filter: '^Node (Dead|Slow|Fast)$'}}
  - {{name: 'HK/TW', type: select, use: [synthetic], filter: '^(HK Dead|HK Slow|TW Fast)$'}}
  - {{name: Other Region, type: select, use: [synthetic], filter: '^Node Manual$'}}
  - {{name: mi-guard-bench, type: select, use: [synthetic]}}
  - {{name: stale-live-bench, type: select, use: [synthetic]}}
listeners:
  - name: mi-guard-bench-listener
    type: mixed
    listen: 127.0.0.1
    port: {listener_port}
    proxy: mi-guard-bench
  - name: stale-route-listener
    type: mixed
    listen: 127.0.0.1
    port: {stale_listener_port}
    proxy: stale-live-bench
  - name: selector-route-listener
    type: mixed
    listen: 127.0.0.1
    port: {selector_listener_port}
    proxy: HK/TW
  - name: legacy-chain-route-listener
    type: mixed
    listen: 127.0.0.1
    port: {chain_listener_port}
    proxy: Chain
rules:
  - MATCH,DIRECT
"""
    )


def write_guard_config(
    path: Path,
    core: Path,
    core_config: Path,
    controller_port: int,
    listener_port: int,
    secret: str,
    origin_port: int,
    history: Path,
) -> None:
    origin = f"http://127.0.0.1:{origin_port}"
    path.write_text(
        f"""mihomo:
  controller: http://127.0.0.1:{controller_port}
  secret: {json.dumps(secret)}
  config_path: {json.dumps(str(core_config))}
  stable_binary: {json.dumps(str(core))}
  test_group: mi-guard-bench
  test_proxy: http://127.0.0.1:{listener_port}
schedule:
  heartbeat_interval: 30s
  heartbeat_timeout: 1s
  heartbeat_failures: 2
  full_interval: 12h
  active_interval: 0s
  recheck_delay: 1s
  cooldown: 1s
  geo_interval: 5m
speedtest:
  url: {origin}/download
  liveness_url: {origin}/204
  geo_url: {origin}/geo
  trace_url: {origin}/trace
  timeout: 2s
  sweep_bytes: 4194304
  quick_bytes: 16384
  confirm_bytes: 65536
  monthly_bytes: 134217728
  billing_day: 1
selection:
  severe_drop_pct: 20
  near_best_pct: 20
  slow_confirmations: 2
  sync_direct_on_warp_switch: true
  max_sample_age: 36h
history:
  file: {json.dumps(str(history))}
  samples_per_node: 50
notifications:
  enabled: false
  summary: false
  switch: false
  helper: {json.dumps(str(path.parent / 'unused.app'))}
groups:
  - name: Managed
    chain: Chain
    direct: Mirror
    exclude_regex: "^$"
  - name: HK/TW
    mode: selector
    exclude_regex: "^$"
"""
    )


def assert_names(output: str, *names: str) -> None:
    missing = [name for name in names if name not in output]
    if missing:
        raise AssertionError(f"command output missing {missing}:\n{output}")


def exercise(core: Path, guard: Path) -> None:
    with tempfile.TemporaryDirectory(prefix="mi-guard-live-core-") as temp:
        scratch = Path(temp)
        origin = QuietThreadingHTTPServer(("127.0.0.1", 0), ProbeHandler)
        proxy_servers = {
            "Node Slow": ProxyServer(("127.0.0.1", 0), "Node Slow", 0.004),
            "Node Fast": ProxyServer(("127.0.0.1", 0), "Node Fast", 0.0),
            "Node Manual": ProxyServer(("127.0.0.1", 0), "Node Manual", 0.0001),
            "HK Slow": ProxyServer(("127.0.0.1", 0), "HK Slow", 0.004),
            "TW Fast": ProxyServer(("127.0.0.1", 0), "TW Fast", 0.0),
            "Exit Proxy": ProxyServer(("127.0.0.1", 0), "Exit Proxy", 0.0),
        }
        servers: list[socketserver.BaseServer] = [origin, *proxy_servers.values()]
        threads = [threading.Thread(target=server.serve_forever, daemon=True) for server in servers]
        for thread in threads:
            thread.start()
        try:
            controller_port = free_port()
            listener_port = free_port()
            stale_listener_port = free_port()
            selector_listener_port = free_port()
            chain_listener_port = free_port()
            dead_port = free_port()
            hk_dead_port = free_port()
            secret = "isolated-smoke-secret"
            core_config = scratch / "mihomo.yaml"
            guard_config = scratch / "mihomo-guard.yaml"
            history = scratch / "speed-history.json"
            provider = scratch / "manual-provider.yaml"
            write_core_config(
                core_config,
                controller_port,
                listener_port,
                stale_listener_port,
                selector_listener_port,
                chain_listener_port,
                secret,
                {
                    **{name: server.server_address[1] for name, server in proxy_servers.items()},
                    "Node Dead": dead_port,
                    "HK Dead": hk_dead_port,
                },
            )
            write_guard_config(
                guard_config,
                core,
                core_config,
                controller_port,
                listener_port,
                secret,
                origin.server_address[1],
                history,
            )
            stale_core_config = scratch / "stale-file-view.yaml"
            stale_view = core_config.read_text()
            stale_view = stale_view.replace(
                f"port: {listener_port}\n    proxy: mi-guard-bench",
                f"port: {listener_port}\n    proxy: stale-live-bench",
            )
            stale_view = stale_view.replace(
                f"port: {stale_listener_port}\n    proxy: stale-live-bench",
                f"port: {stale_listener_port}\n    proxy: mi-guard-bench",
            )
            stale_core_config.write_text(stale_view)
            stale_guard_config = scratch / "stale-mihomo-guard.yaml"
            write_guard_config(
                stale_guard_config,
                core,
                stale_core_config,
                controller_port,
                stale_listener_port,
                secret,
                origin.server_address[1],
                scratch / "stale-history.json",
            )
            cancel_guard_config = scratch / "cancel-mihomo-guard.yaml"
            cancel_history = scratch / "cancel-history.json"
            write_guard_config(
                cancel_guard_config,
                core,
                core_config,
                controller_port,
                listener_port,
                secret,
                origin.server_address[1],
                cancel_history,
            )
            quick_guard_config = scratch / "quick-mihomo-guard.yaml"
            quick_history = scratch / "quick-history.json"
            write_guard_config(
                quick_guard_config,
                core,
                core_config,
                controller_port,
                listener_port,
                secret,
                origin.server_address[1],
                quick_history,
            )
            quick_guard_config.write_text(
                quick_guard_config.read_text().replace(
                    "sweep_bytes: 4194304", "sweep_bytes: 1048576"
                )
            )
            stream_guard_config = scratch / "stream-mihomo-guard.yaml"
            stream_history = scratch / "stream-history.json"
            write_guard_config(
                stream_guard_config,
                core,
                core_config,
                controller_port,
                listener_port,
                secret,
                origin.server_address[1],
                stream_history,
            )
            provider.write_text(
                json.dumps(
                    {
                        "proxies": [
                            {
                                "name": "Node Manual",
                                "type": "http",
                                "server": "127.0.0.1",
                                "port": proxy_servers["Node Manual"].server_address[1],
                            }
                        ]
                    }
                )
            )

            core_log = scratch / "mihomo.log"
            with core_log.open("wb") as log_file:
                process = subprocess.Popen(
                    [str(core), "-d", str(scratch), "-f", str(core_config)],
                    cwd=scratch,
                    stdout=log_file,
                    stderr=subprocess.STDOUT,
                )
                try:
                    wait_for_core(process, controller_port, secret, core_log)
                    core_version = controller(controller_port, secret, "GET", "/version").get(
                        "version", ""
                    )
                    if not core_version.startswith("v"):
                        raise AssertionError(
                            f"--core is not a stable release build: {core_version!r}"
                        )
                    select_proxy(controller_port, secret, "Managed", "Node Slow")
                    select_proxy(controller_port, secret, "Mirror", "Node Slow")
                    select_proxy(controller_port, secret, "HK/TW", "HK Slow")
                    select_proxy(controller_port, secret, "mi-guard-bench", "Node Manual")
                    initial = {
                        name: selection(controller_port, secret, name)
                        for name in (
                            "Managed",
                            "Mirror",
                            "HK/TW",
                            "Other Region",
                            "mi-guard-bench",
                        )
                    }

                    status = run_guard(
                        guard, "status", "-c", str(guard_config), "--probe"
                    )
                    assert_names(status, "Managed", "Node Slow", "HK/TW", "heartbeat")
                    checked = run_guard(guard, "check", "-c", str(guard_config))
                    assert_names(checked, "ready:", "Chain", "Managed", "HK/TW")
                    origin_before_stale_check = STATS.count("Origin /download")
                    stale = run_guard_failure(
                        guard, "check", "-c", str(stale_guard_config)
                    )
                    if "route" not in stale.lower():
                        raise AssertionError(
                            f"stale live listener failed without a route-proof error:\n{stale}"
                        )
                    if STATS.count("Origin /download") != origin_before_stale_check:
                        raise AssertionError("stale route check sent an HTTP probe request")
                    stale_bench_origin = STATS.count("Origin /download")
                    stale_bench = run_guard_failure(
                        guard,
                        "bench",
                        "-c",
                        str(stale_guard_config),
                        "--node",
                        "Node Slow",
                    )
                    if "route" not in stale_bench.lower():
                        raise AssertionError(
                            f"stale bench failed without a route-proof error:\n{stale_bench}"
                        )
                    if STATS.count("Origin /download") != stale_bench_origin:
                        raise AssertionError("stale bench sent a download request")
                    stale_state = json.loads((scratch / "stale-history.json").read_text())
                    if stale_state.get("used_bytes") != 0 or stale_state.get("samples"):
                        raise AssertionError(
                            f"route failure was attributed to a candidate: {stale_state}"
                        )

                    dry = run_guard(
                        guard,
                        "bench",
                        "-c",
                        str(guard_config),
                        "--group",
                        "Managed",
                        "--filter",
                        "Node (Dead|Slow|Fast)",
                    )
                    assert_names(dry, "Node Slow", "Node Fast")
                    dead_row = next((row for row in dry.splitlines() if row.startswith("Node Dead ")), "")
                    if not dead_row.rstrip().endswith(("204 probe: HTTP 503", "204 probe: HTTP 504")):
                        raise AssertionError(f"unexpected failed-probe result row: {dead_row}")
                    after_dry = {
                        name: selection(controller_port, secret, name) for name in initial
                    }
                    if after_dry != initial:
                        raise AssertionError(f"dry bench changed selectors: {initial} -> {after_dry}")

                    single = run_guard(
                        guard,
                        "bench",
                        "-c",
                        str(guard_config),
                        "--node",
                        "Node Slow",
                    )
                    assert_names(single, "Node Slow")
                    if selection(controller_port, secret, "mi-guard-bench") != "Node Manual":
                        raise AssertionError("single-node bench did not restore the test selector")

                    manual_before = STATS.count("Node Manual")
                    other = run_guard(
                        guard,
                        "bench",
                        "-c",
                        str(guard_config),
                        "--group",
                        "Other Region",
                        "--filter",
                        "^Node Manual$",
                    )
                    assert_names(other, "Node Manual")
                    if STATS.count("Node Manual") <= manual_before:
                        raise AssertionError("manual non-managed group did not use Node Manual")
                    if selection(controller_port, secret, "Other Region") != initial["Other Region"]:
                        raise AssertionError("manual non-managed group selection changed")

                    source = run_guard(
                        guard,
                        "bench",
                        "-c",
                        str(guard_config),
                        "--source",
                        str(provider),
                        "--filter",
                        "Manual",
                    )
                    assert_names(source, "Node Manual")

                    applied = run_guard(
                        guard,
                        "bench",
                        "-c",
                        str(guard_config),
                        "--group",
                        "Managed",
                        "--filter",
                        "Node (Slow|Fast)",
                        "--apply",
                    )
                    assert_names(applied, "Node Fast")
                    final = {
                        name: selection(controller_port, secret, name) for name in initial
                    }
                    expected = dict(initial)
                    expected["Managed"] = "Node Fast"
                    expected["Mirror"] = "Node Fast"
                    if final != expected:
                        raise AssertionError(
                            f"apply changed unexpected selectors: {initial} -> {final}\n{applied}"
                        )

                    final_status = run_guard(guard, "status", "-c", str(guard_config))
                    assert_names(final_status, "Managed", "Node Fast", "Mirror")

                    selector_before = {
                        name: selection(controller_port, secret, name)
                        for name in (
                            "Managed",
                            "Mirror",
                            "HK/TW",
                            "Other Region",
                            "mi-guard-bench",
                        )
                    }
                    selector_applied = run_guard(
                        guard,
                        "bench",
                        "-c",
                        str(guard_config),
                        "--group",
                        "HK/TW",
                        "--filter",
                        "^(HK Slow|TW Fast)$",
                        "--apply",
                    )
                    assert_names(selector_applied, "HK Slow", "TW Fast", "selected HK/TW")
                    selector_final = {
                        name: selection(controller_port, secret, name)
                        for name in selector_before
                    }
                    selector_expected = dict(selector_before)
                    selector_expected["HK/TW"] = "TW Fast"
                    if selector_final != selector_expected:
                        raise AssertionError(
                            "standalone apply changed unrelated selectors: "
                            f"{selector_before} -> {selector_final}\n{selector_applied}"
                        )

                    selector_hits = STATS.count("TW Fast")
                    request_through_proxy(
                        selector_listener_port, origin.server_address[1]
                    )
                    if STATS.count("TW Fast") <= selector_hits:
                        raise AssertionError(
                            "standalone production listener did not route through TW Fast"
                        )

                    chain_hits = {
                        name: STATS.count(name) for name in ("Node Fast", "Exit Proxy")
                    }
                    request_through_proxy(chain_listener_port, origin.server_address[1])
                    if any(STATS.count(name) <= count for name, count in chain_hits.items()):
                        raise AssertionError(
                            "legacy chain listener no longer traversed Node Fast and Exit Proxy"
                        )

                    state = json.loads(history.read_text())
                    samples = state.get("samples", [])
                    if state.get("used_bytes", 0) <= 0 or not samples:
                        raise AssertionError(f"history did not record budget/samples: {state}")
                    measured = {sample.get("node") for sample in samples}
                    if not {
                        "Node Slow",
                        "Node Fast",
                        "Node Manual",
                        "HK Slow",
                        "TW Fast",
                    }.issubset(measured):
                        raise AssertionError(f"history missing measured nodes: {measured}")

                    quick = run_guard(
                        guard,
                        "bench",
                        "-c",
                        str(quick_guard_config),
                        "--node",
                        "Node Fast",
                    )
                    assert_names(quick, "Node Fast", "1048576")
                    quick_state = json.loads(quick_history.read_text())
                    if quick_state.get("used_bytes") != 1048576:
                        raise AssertionError(
                            f"1 MiB quick measurement was not strictly accounted: {quick_state}"
                        )

                    stream_downloads = STATS.count("Origin /download")
                    streamed = run_guard(
                        guard,
                        "bench",
                        "-c",
                        str(stream_guard_config),
                        "--node",
                        "Node Fast",
                        "--bytes",
                        "65536",
                        "--streams",
                        "2",
                    )
                    assert_names(streamed, "Node Fast", "65536")
                    stream_state = json.loads(stream_history.read_text())
                    if stream_state.get("used_bytes") != 65536:
                        raise AssertionError(
                            f"two streams did not share one 64 KiB grant: {stream_state}"
                        )
                    if STATS.count("Origin /download") != stream_downloads + 2:
                        raise AssertionError("two-stream bench did not issue exactly two downloads")

                    interrupt_active_bench(guard, cancel_guard_config)
                    cancelled = json.loads(cancel_history.read_text())
                    cancelled_bytes = cancelled.get("used_bytes", 0)
                    if not 0 < cancelled_bytes < 4194304:
                        raise AssertionError(
                            f"cancelled download accounting is not actual bytes: {cancelled}"
                        )
                    if cancelled.get("samples") or cancelled.get("reservations"):
                        raise AssertionError(
                            f"cancelled download left attributed or pending state: {cancelled}"
                        )
                    if selection(controller_port, secret, "mi-guard-bench") != "Node Manual":
                        raise AssertionError("cancelled bench did not restore the test selector")

                    recovery_history = scratch / "recovery-history.json"
                    recovery_state = json.loads(history.read_text())
                    for sample in recovery_state.get("samples", []):
                        sample["kind"] = "sweep"
                    recovery_history.write_text(json.dumps(recovery_state))
                    recovery_config = scratch / "recovery-mihomo-guard.yaml"
                    write_guard_config(
                        recovery_config,
                        core,
                        core_config,
                        controller_port,
                        listener_port,
                        secret,
                        origin.server_address[1],
                        recovery_history,
                    )
                    recovery_config.write_text(
                        recovery_config.read_text()
                        .replace("heartbeat_interval: 30s", "heartbeat_interval: 100ms")
                        .replace("heartbeat_timeout: 1s", "heartbeat_timeout: 200ms")
                        .replace(
                            "monthly_bytes: 134217728",
                            f"monthly_bytes: {recovery_state['used_bytes']}",
                        )
                    )

                    select_proxy(controller_port, secret, "HK/TW", "HK Dead")
                    selector_recovery_downloads = STATS.count("Origin /download")
                    selector_recovered = run_recovery_daemon(
                        guard,
                        recovery_config,
                        controller_port,
                        secret,
                        "HK/TW",
                        "HK Dead",
                        stable_groups={
                            "Managed": "Node Fast",
                            "Mirror": "Node Fast",
                            "Other Region": "Node Manual",
                            "mi-guard-bench": "Node Manual",
                        },
                    )
                    if selector_recovered not in {"HK Slow", "TW Fast"}:
                        raise AssertionError(
                            "standalone selector recovered to unexpected node: "
                            f"{selector_recovered}"
                        )
                    selector_recovered_state = json.loads(recovery_history.read_text())
                    if "HK/TW" not in selector_recovered_state.get("heartbeats", {}):
                        raise AssertionError(
                            "standalone selector recovery did not record an HK/TW heartbeat"
                        )
                    if STATS.count("Origin /download") != selector_recovery_downloads:
                        raise AssertionError(
                            "standalone hard recovery downloaded speed-test data"
                        )

                    select_proxy(controller_port, secret, "Managed", "Node Dead")
                    select_proxy(controller_port, secret, "Mirror", "Node Dead")
                    recovery_downloads = STATS.count("Origin /download")
                    recovered = run_recovery_daemon(
                        guard,
                        recovery_config,
                        controller_port,
                        secret,
                        "Managed",
                        "Node Dead",
                        follower_group="Mirror",
                        stable_groups={
                            "HK/TW": selector_recovered,
                            "Other Region": "Node Manual",
                            "mi-guard-bench": "Node Manual",
                        },
                    )
                    if recovered not in {"Node Slow", "Node Fast"}:
                        raise AssertionError(f"daemon recovered to unexpected nodes: {recovered}")
                    recovered_state = json.loads(recovery_history.read_text())
                    if recovered_state.get("used_bytes") != recovery_state["used_bytes"]:
                        raise AssertionError("hard recovery changed the exhausted byte budget")
                    if STATS.count("Origin /download") != recovery_downloads:
                        raise AssertionError("hard recovery downloaded speed-test data")
                    if selection(controller_port, secret, "mi-guard-bench") != "Node Manual":
                        raise AssertionError("recovery daemon changed the test selector")

                    print(
                        json.dumps(
                            {
                                "result": "PASS",
                                "core_version": core_version,
                                "initial": initial,
                                "final": final,
                                "selector_final": selector_final,
                                "used_bytes": state["used_bytes"],
                                "cancelled_bytes": cancelled_bytes,
                                "quick_bytes": quick_state["used_bytes"],
                                "stream_bytes": stream_state["used_bytes"],
                                "recovered": recovered,
                                "selector_recovered": selector_recovered,
                                "sample_count": len(samples),
                                "proxy_requests": STATS.snapshot(),
                            },
                            ensure_ascii=False,
                            sort_keys=True,
                        )
                    )
                finally:
                    process.send_signal(signal.SIGTERM)
                    try:
                        process.wait(timeout=3)
                    except subprocess.TimeoutExpired:
                        process.kill()
                        process.wait(timeout=3)
        finally:
            for server in servers:
                server.shutdown()
                server.server_close()
            for thread in threads:
                thread.join(timeout=2)


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--core", required=True, type=Path)
    parser.add_argument("--guard", required=True, type=Path)
    arguments = parser.parse_args()
    core = arguments.core.expanduser().resolve()
    guard = arguments.guard.expanduser().resolve()
    for label, path in (("core", core), ("guard", guard)):
        if not path.is_file() or not os.access(path, os.X_OK):
            parser.error(f"--{label} must be an executable file: {path}")
    exercise(core, guard)
    return 0


if __name__ == "__main__":
    sys.exit(main())
