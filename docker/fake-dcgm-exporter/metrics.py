#!/usr/bin/env python3
"""Fake dcgm-exporter for manually testing the GPU fleet + per-service
dashboards in the docker-compose demo stack, without real GPU hardware.

Serves static Prometheus-text on /metrics for 2 fake GPUs on 1 fake node,
with pod/namespace labels on one of them so the per-service inference panel
(pkg/emit/grafana's gpuUtilizationPanel, selector
namespace="ml", pod=~"llama-70b-server-.*") has something real to match --
mirrors what dcgm-exporter's Kubernetes pod-mapping would add once turned on
against a real cluster. Not part of the compiled program; a manual-testing
aid only, run via docker/docker-compose.gpu-test.yml.
"""
import http.server
import random

HOSTNAME = "gpu-node-1"
GPUS = [
    {"gpu": "0", "uuid": "GPU-aaaa0000", "pod": "llama-70b-server-7f9c8d-abcde", "namespace": "ml", "container": "llama-70b-server"},
    {"gpu": "1", "uuid": "GPU-bbbb1111", "pod": "", "namespace": "", "container": ""},
]


def render():
    lines = []

    def metric(name, help_text, unit_type, rows):
        lines.append(f"# HELP {name} {help_text}")
        lines.append(f"# TYPE {name} {unit_type}")
        for g, value in rows:
            labels = f'gpu="{g["gpu"]}",UUID="{g["uuid"]}",Hostname="{HOSTNAME}",pci_bus_id="0000:00:1E.0"'
            if g["pod"]:
                labels += f',pod="{g["pod"]}",namespace="{g["namespace"]}",container="{g["container"]}"'
            lines.append(f"{name}{{{labels}}} {value}")

    metric("DCGM_FI_DEV_GPU_UTIL", "GPU utilization (in %).", "gauge",
           [(g, random.randint(20, 95)) for g in GPUS])
    metric("DCGM_FI_DEV_MEM_COPY_UTIL", "Memory utilization (in %).", "gauge",
           [(g, random.randint(10, 70)) for g in GPUS])
    metric("DCGM_FI_DEV_GPU_TEMP", "GPU temperature (in C).", "gauge",
           [(GPUS[0], 88), (GPUS[1], random.randint(55, 75))])  # GPU 0 deliberately over thermalCriticalC (85)
    metric("DCGM_FI_DEV_POWER_USAGE", "Power draw (in W).", "gauge",
           [(g, random.randint(150, 280)) for g in GPUS])
    metric("DCGM_FI_DEV_POWER_MGMT_LIMIT", "Power management limit (in W).", "gauge",
           [(g, 300) for g in GPUS])
    metric("DCGM_FI_DEV_ECC_DBE_VOL_TOTAL", "Total number of double-bit ECC errors.", "counter",
           [(g, 0) for g in GPUS])
    metric("DCGM_FI_DEV_XID_ERRORS", "Value of the last XID error encountered.", "gauge",
           [(g, 0) for g in GPUS])

    return "\n".join(lines) + "\n"


class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path != "/metrics":
            self.send_response(404)
            self.end_headers()
            return
        body = render().encode()
        self.send_response(200)
        self.send_header("Content-Type", "text/plain; version=0.0.4")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, fmt, *args):
        pass


if __name__ == "__main__":
    http.server.HTTPServer(("0.0.0.0", 9400), Handler).serve_forever()
