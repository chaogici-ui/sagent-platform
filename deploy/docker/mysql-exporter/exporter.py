#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""MySQL 探针导出器（纯 stdlib，零第三方依赖——环境无外网，装不了 pymysql/mysqld-exporter）。

诚实口径：本导出器不伪装 mysqld-exporter。它做的是协议级探测——
  1. TCP 连通目标 MySQL 的 3306（活性 + 连接时延）；
  2. 读取 MySQL 握手包首个字段，取真实 server version（协议天然回报）。
指标名 mysql_probe_* 如实反映"探针看到的 MySQL"，不含查询计数/缓冲池等内部指标。
生产替换路径：换成官方 mysqld-exporter，指标名换 mysqld_*，vmagent 池调度机制与其无关。
"""
import http.server
import os
import socket
import struct
import threading
import time

MYSQL_HOST = os.environ.get("MYSQL_HOST", "mysql-1")
MYSQL_PORT = int(os.environ.get("MYSQL_PORT", "3306"))
LISTEN_PORT = int(os.environ.get("EXPORTER_PORT", "9104"))
PROBE_INTERVAL = float(os.environ.get("PROBE_INTERVAL_SEC", "10"))

_lock = threading.Lock()
_state = {
    "up": 0,                # 最近一次探测是否成功
    "duration": 0.0,        # 最近一次探测耗时（秒）
    "version": "",          # 握手包回报的 server version
    "probes": 0,            # 累计探测次数
    "errors": 0,            # 累计探测失败次数
    "started": time.time(),
}


def probe_once():
    """TCP 连通 + 读握手包取版本。MySQL 握手包：4 字节头(含 3 字节长度) + 0x0a + 版本串(\\0 结尾)。"""
    start = time.time()
    try:
        with socket.create_connection((MYSQL_HOST, MYSQL_PORT), timeout=5) as s:
            s.settimeout(5)
            header = s.recv(4)
            if len(header) < 4:
                raise OSError("握手包头不完整")
            body_len = header[0] | (header[1] << 8) | (header[2] << 16)
            body = b""
            while len(body) < body_len:
                chunk = s.recv(body_len - len(body))
                if not chunk:
                    break
                body += chunk
            if not body or body[0] != 0x0A:
                raise OSError("非 MySQL 协议握手包")
            version = body[1:body.index(b"\x00", 1)].decode("ascii", "replace") if b"\x00" in body[1:] else body[1:].decode("ascii", "replace")
            dur = time.time() - start
            with _lock:
                _state.update(up=1, duration=dur, version=version, probes=_state["probes"] + 1)
    except Exception:
        dur = time.time() - start
        with _lock:
            _state.update(up=0, duration=dur, probes=_state["probes"] + 1, errors=_state["errors"] + 1)


def probe_loop():
    while True:
        probe_once()
        time.sleep(PROBE_INTERVAL)


class MetricsHandler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path != "/metrics":
            self.send_response(404)
            self.end_headers()
            return
        with _lock:
            lines = [
                "# HELP mysql_probe_up 最近一次 MySQL 协议探测是否成功（1 成功 / 0 失败）",
                "# TYPE mysql_probe_up gauge",
                'mysql_probe_up{address="%s:%d"} %d' % (MYSQL_HOST, MYSQL_PORT, _state["up"]),
                "# HELP mysql_probe_duration_seconds 最近一次探测耗时",
                "# TYPE mysql_probe_duration_seconds gauge",
                "mysql_probe_duration_seconds %0.6f" % _state["duration"],
                "# HELP mysql_server_version_info 目标 MySQL 版本（协议握手包回报）",
                "# TYPE mysql_server_version_info gauge",
                'mysql_server_version_info{address="%s:%d",version="%s"} 1' % (MYSQL_HOST, MYSQL_PORT, _state["version"]),
                "# HELP mysql_probe_total 累计探测次数",
                "# TYPE mysql_probe_total counter",
                "mysql_probe_total %d" % _state["probes"],
                "# HELP mysql_probe_errors_total 累计探测失败次数",
                "# TYPE mysql_probe_errors_total counter",
                "mysql_probe_errors_total %d" % _state["errors"],
                "# HELP mysql_exporter_uptime_seconds 导出器自身存活时长",
                "# TYPE mysql_exporter_uptime_seconds gauge",
                "mysql_exporter_uptime_seconds %0.0f" % (time.time() - _state["started"]),
            ]
        body = ("\n".join(lines) + "\n").encode()
        self.send_response(200)
        self.send_header("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *args):
        pass


if __name__ == "__main__":
    threading.Thread(target=probe_loop, daemon=True).start()
    srv = http.server.ThreadingHTTPServer(("0.0.0.0", LISTEN_PORT), MetricsHandler)
    srv.serve_forever()
