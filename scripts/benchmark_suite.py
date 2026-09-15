#!/usr/bin/env python3
"""Formal benchmark suite for Bluestone.

The suite assumes the gateway is already running and mounted. Destructive
scenarios such as crash-safety require explicit opt-in flags.
"""

from __future__ import annotations

import argparse
import csv
import hashlib
import json
import os
import shutil
import signal
import socket
import subprocess
import sys
import threading
import time
import urllib.request
from dataclasses import dataclass, field
from datetime import datetime, timezone
from pathlib import Path
from typing import Any


MIB = 1024 * 1024
DEFAULT_DEBUG_URL = "http://127.0.0.1:8082/debug/staging/sync"
DEFAULT_METRICS_URL = "http://127.0.0.1:8080/metrics"
DEFAULT_HEALTH_URL = "http://127.0.0.1:8081/health/live"


@dataclass
class BenchmarkResult:
    category: str
    name: str
    status: str
    started_at: str
    ended_at: str
    duration_seconds: float
    metrics: dict[str, Any] = field(default_factory=dict)
    samples: list[dict[str, Any]] = field(default_factory=list)
    artifacts: dict[str, str] = field(default_factory=dict)
    error: str = ""


class BenchmarkSuite:
    def __init__(self, args: argparse.Namespace) -> None:
        self.args = args
        self.mount = Path(args.mount).resolve()
        self.results_root = Path(args.results_root).resolve()
        self.run_id = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ")
        self.results_dir = self.results_root / f"benchmark-{self.run_id}"
        self.raw_dir = self.results_dir / "raw"
        self.test_root = self.mount / f".benchmark-{self.run_id}"
        self.results: list[BenchmarkResult] = []
        self.monitor_stop = threading.Event()
        self.monitor_samples: list[dict[str, Any]] = []
        self._monitor_thread: threading.Thread | None = None

    def run(self) -> int:
        if not self.args.skip_mount_check:
            self.require_mounted()

        self.results_dir.mkdir(parents=True, exist_ok=False)
        self.raw_dir.mkdir(parents=True, exist_ok=True)
        self.test_root.mkdir(parents=True, exist_ok=True)

        env = self.capture_environment()
        (self.results_dir / "environment.json").write_text(json.dumps(env, indent=2, sort_keys=True))

        self.start_monitor()
        try:
            self.run_categories()
        finally:
            self.stop_monitor()
            self.cleanup_test_root()

        self.write_outputs(env)
        self.print_summary()
        return 0 if all(r.status in {"pass", "skip"} for r in self.results) else 1

    def run_categories(self) -> None:
        requested = set(self.args.categories or [])
        categories = [
            ("frontend-write", self.bench_frontend_write),
            ("sync", self.bench_sync_performance),
            ("read", self.bench_read_performance),
            ("backpressure", self.bench_backpressure),
            ("small-files", self.bench_small_files),
            ("crash-safety", self.bench_crash_safety),
            ("mixed", self.bench_mixed_workload),
        ]
        for category, fn in categories:
            if requested and category not in requested:
                continue
            fn()

    def bench_frontend_write(self) -> None:
        sizes = self.profile_sizes()
        for size_name, size_mib in sizes:
            for direct in (0, 1):
                name = f"seq-write-{size_name}-direct{direct}"
                target = self.test_root / f"{name}.dat"
                self.run_case("frontend-write", name, lambda: self.run_fio(
                    name=name,
                    rw="write",
                    filename=target,
                    size_mib=size_mib,
                    bs="1M",
                    direct=direct,
                    extra=["--iodepth=1"],
                ))

    def bench_sync_performance(self) -> None:
        size_mib = 100 if self.args.profile != "quick" else 32
        name = f"sync-durable-{size_mib}MiB"
        target = self.test_root / f"{name}.dat"

        def body() -> dict[str, Any]:
            write = self.run_fio(
                name=name,
                rw="write",
                filename=target,
                size_mib=size_mib,
                bs="1M",
                direct=0,
                extra=["--iodepth=1"],
            )
            sync = self.wait_for_sync("/" + str(target.relative_to(self.mount)), timeout=self.args.sync_timeout)
            return {
                **prefix_metrics("write_", write),
                "sync_latency_seconds": sync.get("duration_seconds"),
                "sync_queue_max_depth": max_sample("sync_queue_depth", sync.get("samples", [])),
                "sync_queue_max_bytes": max_sample("sync_queue_bytes", sync.get("samples", [])),
                "dirty_bytes_max": max_sample("dirty_bytes", sync.get("samples", [])),
                "sync_throughput_mib_s": sync.get("upload_mib_per_second"),
            }

        self.run_case("sync", name, body)

    def bench_read_performance(self) -> None:
        size_mib = 512 if self.args.profile == "full" else 128 if self.args.profile == "standard" else 32
        target = self.ensure_read_file(size_mib)

        self.drop_os_cache()
        self.run_case("read", "cold-read-from-cos", lambda: self.run_fio(
            name="cold-read-from-cos",
            rw="read",
            filename=target,
            size_mib=size_mib,
            bs="1M",
            direct=0,
            extra=["--iodepth=1"],
        ))

        self.drop_os_cache()
        self.run_case("read", "warm-read-local-cache", lambda: self.run_fio(
            name="warm-read-local-cache",
            rw="read",
            filename=target,
            size_mib=size_mib,
            bs="1M",
            direct=0,
            extra=["--iodepth=1"],
        ))

        partial_size_mib = max(1, min(64, size_mib // 2))
        partial_offset_mib = max(0, min(16, size_mib - partial_size_mib))
        self.run_case("read", "partial-range-read", lambda: self.run_fio(
            name="partial-range-read",
            rw="read",
            filename=target,
            size_mib=partial_size_mib,
            bs="1M",
            direct=0,
            extra=[f"--offset={partial_offset_mib}M"],
        ))

        self.run_case("read", "random-4k-read", lambda: self.run_fio(
            name="random-4k-read",
            rw="randread",
            filename=target,
            size_mib=size_mib,
            bs="4K",
            direct=0,
            extra=["--runtime=30", "--time_based", "--iodepth=16"],
        ))

        self.run_case("read", "large-sequential-read", lambda: self.run_fio(
            name="large-sequential-read",
            rw="read",
            filename=target,
            size_mib=size_mib,
            bs="4M",
            direct=0,
            extra=["--iodepth=4"],
        ))

    def bench_backpressure(self) -> None:
        if not self.args.allow_backpressure:
            for name in (
                "below-high-watermark",
                "above-high-watermark",
                "above-critical-watermark",
                "block-mode",
                "fail-fast-mode",
                "sync-drain-releases-pressure",
            ):
                self.skip("backpressure", name, "requires --allow-backpressure and a gateway configured with test watermarks")
            return

        size_mib = self.args.backpressure_write_mib
        for name in ("below-high-watermark", "above-high-watermark", "above-critical-watermark"):
            self.run_case("backpressure", name, lambda n=name: self.write_until_pressure(n, size_mib))
        self.run_case("backpressure", "sync-drain-releases-pressure", self.measure_sync_drain_pressure)
        self.skip("backpressure", "block-mode", "mode-specific validation requires restarting gateway with block mode")
        self.skip("backpressure", "fail-fast-mode", "mode-specific validation requires restarting gateway with fail_fast mode")

    def bench_small_files(self) -> None:
        counts = [100] if self.args.profile == "quick" else [100, 1000] if self.args.profile == "standard" else [100, 1000, 10000]
        for count in counts:
            self.run_case("small-files", f"create-read-delete-{count}", lambda c=count: self.small_file_workload(c))

    def bench_crash_safety(self) -> None:
        cases = ("crash-before-sync", "crash-during-sync", "crash-during-multipart")
        if not self.args.allow_crash:
            for case in cases:
                self.skip("crash-safety", case, "requires --allow-crash and --gateway-command")
            return
        if not self.args.gateway_command:
            for case in cases:
                self.skip("crash-safety", case, "missing --gateway-command")
            return
        self.run_case("crash-safety", "crash-before-sync", lambda: self.crash_recovery_case("before-sync", 256))
        self.run_case("crash-safety", "crash-during-sync", lambda: self.crash_recovery_case("during-sync", 512))
        self.run_case("crash-safety", "crash-during-multipart", lambda: self.crash_recovery_case("during-multipart", 1024))

    def bench_mixed_workload(self) -> None:
        self.run_case("mixed", "concurrent-readers-writers", lambda: self.mixed_workload(readers=4, writers=2, seconds=30))
        self.run_case("mixed", "large-writes-plus-small-reads", lambda: self.mixed_workload(readers=8, writers=1, seconds=30, large_writes=True))
        self.run_case("mixed", "dirty-file-reads-during-sync", self.dirty_file_reads_during_sync)

    def run_case(self, category: str, name: str, fn: Any) -> None:
        started = iso_now()
        start = time.monotonic()
        before_metrics = self.scrape_metrics()
        sample_start = len(self.monitor_samples)
        status = "pass"
        error = ""
        metrics: dict[str, Any] = {}
        artifacts: dict[str, str] = {}
        try:
            metrics = fn() or {}
        except Exception as exc:  # keep suite running and record deterministic failures
            status = "fail"
            error = str(exc)
        ended = iso_now()
        duration = time.monotonic() - start
        after_metrics = self.scrape_metrics()
        metrics.update(metric_delta(before_metrics, after_metrics))
        metrics.setdefault("error_count", 1 if status == "fail" else 0)
        samples = self.monitor_samples[sample_start:]
        self.results.append(BenchmarkResult(category, name, status, started, ended, duration, metrics, samples, artifacts, error))

    def skip(self, category: str, name: str, reason: str) -> None:
        now = iso_now()
        self.results.append(BenchmarkResult(category, name, "skip", now, now, 0, {}, [], {}, reason))

    def run_fio(self, name: str, rw: str, filename: Path, size_mib: int, bs: str, direct: int, extra: list[str] | None = None) -> dict[str, Any]:
        require_tool("fio")
        output = self.raw_dir / f"{safe_name(name)}.fio.json"
        command = [
            "fio",
            f"--name={name}",
            f"--filename={filename}",
            f"--rw={rw}",
            f"--bs={bs}",
            f"--size={size_mib}M",
            "--numjobs=1",
            f"--direct={direct}",
            "--group_reporting",
            "--output-format=json",
            f"--output={output}",
        ]
        if extra:
            command.extend(extra)
        run(command, timeout=self.args.command_timeout)
        data = json.loads(output.read_text())
        metrics = fio_metrics(data, rw)
        metrics["artifact_fio_json"] = str(output)
        metrics["size_mib"] = size_mib
        metrics["direct"] = direct
        metrics["block_size"] = bs
        return metrics

    def ensure_read_file(self, size_mib: int) -> Path:
        target = self.test_root / f"read-source-{size_mib}MiB.dat"
        if target.exists() and target.stat().st_size == size_mib * MIB:
            return target
        metrics = self.run_fio(
            name=f"prepare-read-source-{size_mib}MiB",
            rw="write",
            filename=target,
            size_mib=size_mib,
            bs="1M",
            direct=0,
            extra=["--iodepth=1"],
        )
        _ = metrics
        self.wait_for_sync("/" + str(target.relative_to(self.mount)), timeout=self.args.sync_timeout)
        return target

    def wait_for_sync(self, nfs_path: str, timeout: float) -> dict[str, Any]:
        start = time.monotonic()
        samples: list[dict[str, Any]] = []
        last_sync: dict[str, Any] = {}
        while time.monotonic() - start < timeout:
            sample = self.fetch_debug_sample()
            if sample:
                samples.append(sample)
                maybe_last = sample.get("last_sync") or {}
                if maybe_last.get("path") == nfs_path:
                    last_sync = maybe_last
                if sample.get("sync_queue_depth") == 0 and (last_sync or not nfs_path):
                    break
            time.sleep(self.args.monitor_interval)
        return {
            "duration_seconds": time.monotonic() - start,
            "samples": samples,
            "upload_mib_per_second": last_sync.get("upload_mib_per_second"),
            "last_sync": last_sync,
        }

    def write_until_pressure(self, name: str, size_mib: int) -> dict[str, Any]:
        target = self.test_root / f"{name}.dat"
        started = time.monotonic()
        rc = subprocess.call(["dd", "if=/dev/zero", f"of={target}", "bs=1M", f"count={size_mib}", "conv=fsync", "status=none"])
        elapsed = time.monotonic() - started
        return {
            "return_code": rc,
            "duration_seconds": elapsed,
            "throughput_mib_s": size_mib / elapsed if elapsed > 0 else 0,
            **self.current_debug_metrics(),
        }

    def measure_sync_drain_pressure(self) -> dict[str, Any]:
        before = self.current_debug_metrics()
        sync = self.wait_for_sync("", timeout=self.args.sync_timeout)
        after = self.current_debug_metrics()
        return {
            "before_staging_used_bytes": before.get("staging_used_bytes"),
            "after_staging_used_bytes": after.get("staging_used_bytes"),
            "before_sync_queue_bytes": before.get("sync_queue_bytes"),
            "after_sync_queue_bytes": after.get("sync_queue_bytes"),
            "duration_seconds": sync.get("duration_seconds"),
        }

    def small_file_workload(self, count: int) -> dict[str, Any]:
        directory = self.test_root / f"small-files-{count}"
        directory.mkdir(parents=True, exist_ok=True)
        payload = b"x" * 1024

        create_lat = []
        read_lat = []
        delete_lat = []

        for i in range(count):
            path = directory / f"file-{i:06d}.txt"
            create_lat.append(time_call(lambda p=path: p.write_bytes(payload)))
        for i in range(count):
            path = directory / f"file-{i:06d}.txt"
            read_lat.append(time_call(lambda p=path: p.read_bytes()))
        for i in range(count):
            path = directory / f"file-{i:06d}.txt"
            delete_lat.append(time_call(lambda p=path: p.unlink(missing_ok=True)))

        return {
            "file_count": count,
            "create_iops": count / sum(create_lat) if sum(create_lat) > 0 else 0,
            "read_iops": count / sum(read_lat) if sum(read_lat) > 0 else 0,
            "delete_iops": count / sum(delete_lat) if sum(delete_lat) > 0 else 0,
            **latency_metrics("create_latency", create_lat),
            **latency_metrics("read_latency", read_lat),
            **latency_metrics("delete_latency", delete_lat),
            **self.current_debug_metrics(),
        }

    def crash_recovery_case(self, label: str, size_mib: int) -> dict[str, Any]:
        target = self.test_root / f"crash-{label}-{size_mib}MiB.dat"
        write_pattern_file(target, size_mib)
        gateway_pids = pgrep("bluestone")
        if not gateway_pids:
            raise RuntimeError("bluestone process not found")
        os.kill(gateway_pids[0], signal.SIGKILL)
        time.sleep(2)
        run_shell(self.args.gateway_command, timeout=self.args.command_timeout)
        if self.args.post_restart_command:
            run_shell(self.args.post_restart_command, timeout=self.args.command_timeout)
        self.wait_for_health()
        deadline = time.monotonic() + self.args.crash_recovery_timeout
        while time.monotonic() < deadline:
            if target.exists() and verify_pattern_file(target, size_mib):
                return {"size_mib": size_mib, "recovered": True}
            time.sleep(1)
        raise RuntimeError(f"{label} did not recover within timeout")

    def mixed_workload(self, readers: int, writers: int, seconds: int, large_writes: bool = False) -> dict[str, Any]:
        stop = threading.Event()
        errors: list[str] = []
        counts = {"reads": 0, "writes": 0}
        source = self.ensure_read_file(32)

        def reader() -> None:
            while not stop.is_set():
                try:
                    with source.open("rb") as f:
                        f.read(1024 * 1024)
                    counts["reads"] += 1
                except Exception as exc:
                    errors.append(str(exc))

        def writer(index: int) -> None:
            size = 64 if large_writes else 8
            while not stop.is_set():
                try:
                    path = self.test_root / f"mixed-writer-{index}-{time.time_ns()}.dat"
                    write_zero_file(path, size)
                    counts["writes"] += 1
                except Exception as exc:
                    errors.append(str(exc))

        threads = [threading.Thread(target=reader) for _ in range(readers)]
        threads += [threading.Thread(target=writer, args=(i,)) for i in range(writers)]
        for thread in threads:
            thread.start()
        time.sleep(seconds)
        stop.set()
        for thread in threads:
            thread.join(timeout=5)
        return {
            "duration_seconds": seconds,
            "reader_threads": readers,
            "writer_threads": writers,
            "read_ops": counts["reads"],
            "write_ops": counts["writes"],
            "read_iops": counts["reads"] / seconds,
            "write_iops": counts["writes"] / seconds,
            "error_count": len(errors),
            "errors_sample": errors[:5],
            **self.current_debug_metrics(),
        }

    def dirty_file_reads_during_sync(self) -> dict[str, Any]:
        target = self.test_root / "dirty-read-during-sync.dat"
        write_zero_file(target, 128 if self.args.profile != "quick" else 32)
        latencies = []
        for _ in range(20):
            latencies.append(time_call(lambda: target.read_bytes()[:4096]))
        return {
            **latency_metrics("dirty_read_latency", latencies),
            **self.current_debug_metrics(),
        }

    def start_monitor(self) -> None:
        self._monitor_thread = threading.Thread(target=self.monitor_loop, daemon=True)
        self._monitor_thread.start()

    def stop_monitor(self) -> None:
        self.monitor_stop.set()
        if self._monitor_thread:
            self._monitor_thread.join(timeout=5)
        samples_path = self.results_dir / "monitor_samples.csv"
        write_dict_csv(samples_path, self.monitor_samples)

    def monitor_loop(self) -> None:
        while not self.monitor_stop.is_set():
            sample = self.fetch_debug_sample()
            if sample:
                sample.update(self.scrape_selected_metrics())
                self.monitor_samples.append(sample)
            time.sleep(self.args.monitor_interval)

    def fetch_debug_sample(self) -> dict[str, Any]:
        try:
            data = http_json(self.args.debug_url, timeout=1)
            return normalize_debug_sample(data)
        except Exception:
            return {}

    def scrape_metrics(self) -> dict[str, float]:
        try:
            return parse_prometheus(http_text(self.args.metrics_url, timeout=1))
        except Exception:
            return {}

    def scrape_selected_metrics(self) -> dict[str, Any]:
        metrics = self.scrape_metrics()
        selected = {}
        for key in (
            'cache_hits_total{cache_type="data"}',
            'cache_misses_total{cache_type="data"}',
            "writes_blocked_total",
            "writes_rejected_total",
            "staging_used_bytes",
            "staging_available_bytes",
            "staging_pressure_level",
            "sync_queue_bytes",
        ):
            if key in metrics:
                selected[key.replace('{cache_type="data"}', "_data")] = metrics[key]
        hits = metrics.get('cache_hits_total{cache_type="data"}', 0)
        misses = metrics.get('cache_misses_total{cache_type="data"}', 0)
        selected["cache_hit_ratio"] = hits / (hits + misses) if hits + misses > 0 else None
        return selected

    def current_debug_metrics(self) -> dict[str, Any]:
        return self.fetch_debug_sample() | self.scrape_selected_metrics()

    def capture_environment(self) -> dict[str, Any]:
        config_path = Path(self.args.config).resolve() if self.args.config else None
        instance_metadata = ibm_cloud_instance_metadata()
        env = {
            "run_id": self.run_id,
            "timestamp_utc": iso_now(),
            "hostname": socket.gethostname(),
            "vm_size": detect_vm_size(instance_metadata),
            "cloud_zone": detect_cloud_zone(instance_metadata),
            "mount": str(self.mount),
            "mount_options": command_output(["findmnt", "-no", "OPTIONS", str(self.mount)]),
            "mount_source": command_output(["findmnt", "-no", "SOURCE", str(self.mount)]),
            "kernel": command_output(["uname", "-a"]),
            "os_release": read_file("/etc/os-release"),
            "cpu": command_output(["sh", "-c", "lscpu 2>/dev/null || sysctl -a machdep.cpu 2>/dev/null | head -50"]),
            "memory": command_output(["sh", "-c", "free -h 2>/dev/null || vm_stat 2>/dev/null"]),
            "disk": command_output(["sh", "-c", f"df -h {shell_quote(str(self.mount))} /tmp 2>/dev/null; lsblk 2>/dev/null || true"]),
            "fio_version": command_output(["sh", "-c", "fio --version 2>/dev/null || true"]),
            "git_commit": command_output(["git", "rev-parse", "HEAD"]),
            "git_dirty": command_output(["sh", "-c", "git status --short"]),
            "config_path": str(config_path) if config_path else None,
            "config_sha256": sha256_file(config_path) if config_path and config_path.exists() else None,
            "debug_url": self.args.debug_url,
            "metrics_url": self.args.metrics_url,
            "health_url": self.args.health_url,
        }
        if config_path and config_path.exists():
            shutil.copy2(config_path, self.results_dir / "config.yaml")
        return env

    def require_mounted(self) -> None:
        if not self.mount.exists():
            raise RuntimeError(f"mount path does not exist: {self.mount}")
        if shutil.which("findmnt") is None:
            return
        found = subprocess.run(
            ["findmnt", "--mountpoint", str(self.mount)],
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
            check=False,
        )
        if found.returncode != 0:
            raise RuntimeError(f"mount path is not mounted: {self.mount}")

    def wait_for_health(self) -> None:
        deadline = time.monotonic() + self.args.crash_recovery_timeout
        while time.monotonic() < deadline:
            try:
                http_text(self.args.health_url, timeout=1)
                return
            except Exception:
                time.sleep(1)
        raise RuntimeError(f"gateway health endpoint did not recover: {self.args.health_url}")

    def cleanup_test_root(self) -> None:
        if self.args.keep_data:
            return
        shutil.rmtree(self.test_root, ignore_errors=True)

    def write_outputs(self, env: dict[str, Any]) -> None:
        payload = {
            "schema_version": "cos-nfs-gateway-benchmark/v1",
            "environment": env,
            "results": [result.__dict__ for result in self.results],
        }
        (self.results_dir / "results.json").write_text(json.dumps(payload, indent=2, sort_keys=True))
        self.write_results_csv()
        self.write_baseline_json(env)
        self.write_summary_markdown(env)

    def write_results_csv(self) -> None:
        rows = []
        for result in self.results:
            row = {
                "category": result.category,
                "name": result.name,
                "status": result.status,
                "duration_seconds": result.duration_seconds,
                "throughput_mib_s": result.metrics.get("throughput_mib_s"),
                "iops": result.metrics.get("iops"),
                "p50_latency_ms": result.metrics.get("p50_latency_ms"),
                "p95_latency_ms": result.metrics.get("p95_latency_ms"),
                "p99_latency_ms": result.metrics.get("p99_latency_ms"),
                "sync_latency_seconds": result.metrics.get("sync_latency_seconds"),
                "cache_hit_ratio": result.metrics.get("cache_hit_ratio"),
                "staging_used_bytes": result.metrics.get("staging_used_bytes"),
                "dirty_bytes": result.metrics.get("dirty_bytes"),
                "sync_queue_bytes": result.metrics.get("sync_queue_bytes"),
                "writes_rejected_delta": result.metrics.get("writes_rejected_delta"),
                "writes_blocked_delta": result.metrics.get("writes_blocked_delta"),
                "error_count": result.metrics.get("error_count"),
                "error": result.error,
            }
            rows.append(row)
        write_dict_csv(self.results_dir / "results.csv", rows)

    def write_baseline_json(self, env: dict[str, Any]) -> None:
        comparable = {}
        for result in self.results:
            if result.status != "pass":
                continue
            key = f"{result.category}.{result.name}"
            comparable[key] = {
                "throughput_mib_s": result.metrics.get("throughput_mib_s"),
                "iops": result.metrics.get("iops"),
                "p50_latency_ms": result.metrics.get("p50_latency_ms"),
                "p95_latency_ms": result.metrics.get("p95_latency_ms"),
                "p99_latency_ms": result.metrics.get("p99_latency_ms"),
                "sync_latency_seconds": result.metrics.get("sync_latency_seconds"),
                "cache_hit_ratio": result.metrics.get("cache_hit_ratio"),
                "error_count": result.metrics.get("error_count", 0),
            }
        baseline = {
            "schema_version": "cos-nfs-gateway-baseline/v1",
            "commit": env.get("git_commit"),
            "timestamp_utc": env.get("timestamp_utc"),
            "profile": self.args.profile,
            "benchmarks": comparable,
        }
        (self.results_dir / "baseline.json").write_text(json.dumps(baseline, indent=2, sort_keys=True))

    def write_summary_markdown(self, env: dict[str, Any]) -> None:
        lines = [
            "# Bluestone Benchmark Summary",
            "",
            f"- Run ID: `{self.run_id}`",
            f"- Commit: `{env.get('git_commit', '').strip()}`",
            f"- Profile: `{self.args.profile}`",
            f"- Mount: `{self.mount}`",
            "",
            "| Category | Benchmark | Status | MiB/s | IOPS | p50 ms | p95 ms | p99 ms | Error |",
            "|---|---|---:|---:|---:|---:|---:|---:|---|",
        ]
        for result in self.results:
            metrics = result.metrics
            lines.append(
                "| {category} | {name} | {status} | {throughput} | {iops} | {p50} | {p95} | {p99} | {error} |".format(
                    category=result.category,
                    name=result.name,
                    status=result.status,
                    throughput=format_cell(metrics.get("throughput_mib_s")),
                    iops=format_cell(metrics.get("iops")),
                    p50=format_cell(metrics.get("p50_latency_ms")),
                    p95=format_cell(metrics.get("p95_latency_ms")),
                    p99=format_cell(metrics.get("p99_latency_ms")),
                    error=result.error.replace("|", "\\|")[:120],
                )
            )
        (self.results_dir / "SUMMARY.md").write_text("\n".join(lines) + "\n")

    def print_summary(self) -> None:
        print(f"Benchmark results: {self.results_dir}")
        for result in self.results:
            throughput = result.metrics.get("throughput_mib_s")
            suffix = f" {throughput:.2f} MiB/s" if isinstance(throughput, (int, float)) else ""
            print(f"{result.status.upper():4} {result.category}/{result.name}{suffix}")

    def profile_sizes(self) -> list[tuple[str, int]]:
        if self.args.profile == "quick":
            return [("100MiB", 100)]
        if self.args.profile == "standard":
            return [("100MiB", 100), ("1GiB", 1024)]
        return [("100MiB", 100), ("1GiB", 1024), ("10GiB", 10 * 1024)]

    def drop_os_cache(self) -> None:
        if os.geteuid() != 0:
            return
        run(["sync"], check=False)
        Path("/proc/sys/vm/drop_caches").write_text("3\n")


def fio_metrics(data: dict[str, Any], rw: str) -> dict[str, Any]:
    job = data["jobs"][0]
    section = job.get(rw) or job.get("read" if "read" in rw else "write") or {}
    bw_kib = section.get("bw", 0)
    iops = section.get("iops", 0)
    lat = section.get("clat_ns", {}).get("percentile", {})
    return {
        "throughput_mib_s": bw_kib / 1024 if bw_kib is not None else None,
        "iops": iops,
        "p50_latency_ms": ns_percentile_ms(lat, "50.000000"),
        "p95_latency_ms": ns_percentile_ms(lat, "95.000000"),
        "p99_latency_ms": ns_percentile_ms(lat, "99.000000"),
    }


def ns_percentile_ms(percentiles: dict[str, Any], key: str) -> float | None:
    value = percentiles.get(key)
    return value / 1_000_000 if isinstance(value, (int, float)) else None


def latency_metrics(prefix: str, values: list[float]) -> dict[str, float | None]:
    if not values:
        return {
            f"{prefix}_p50_ms": None,
            f"{prefix}_p95_ms": None,
            f"{prefix}_p99_ms": None,
        }
    ordered = sorted(values)
    return {
        f"{prefix}_p50_ms": percentile(ordered, 50) * 1000,
        f"{prefix}_p95_ms": percentile(ordered, 95) * 1000,
        f"{prefix}_p99_ms": percentile(ordered, 99) * 1000,
    }


def percentile(ordered: list[float], pct: float) -> float:
    if len(ordered) == 1:
        return ordered[0]
    rank = (len(ordered) - 1) * pct / 100
    lower = int(rank)
    upper = min(lower + 1, len(ordered) - 1)
    weight = rank - lower
    return ordered[lower] * (1 - weight) + ordered[upper] * weight


def time_call(fn: Any) -> float:
    start = time.monotonic()
    fn()
    return time.monotonic() - start


def write_zero_file(path: Path, size_mib: int) -> None:
    with path.open("wb", buffering=0) as f:
        block = b"\0" * MIB
        for _ in range(size_mib):
            f.write(block)
        os.fsync(f.fileno())


def write_pattern_file(path: Path, size_mib: int) -> None:
    with path.open("wb", buffering=0) as f:
        for i in range(size_mib):
            value = i.to_bytes(8, "big")
            f.write(value * (MIB // 8))
        os.fsync(f.fileno())


def verify_pattern_file(path: Path, size_mib: int) -> bool:
    if not path.exists() or path.stat().st_size != size_mib * MIB:
        return False
    samples = sorted(set([0, 1, max(0, size_mib // 2), max(0, size_mib - 2), max(0, size_mib - 1)]))
    with path.open("rb", buffering=0) as f:
        for sample in samples:
            f.seek(sample * MIB)
            if int.from_bytes(f.read(8), "big") != sample:
                return False
    return True


def normalize_debug_sample(data: dict[str, Any]) -> dict[str, Any]:
    return {
        "timestamp": iso_now(),
        "sync_queue_depth": data.get("sync_queue_depth"),
        "sync_queue_bytes": data.get("sync_queue_bytes"),
        "dirty_bytes": data.get("total_size"),
        "dirty_files": data.get("dirty_files"),
        "syncing_files": data.get("syncing_files"),
        "staging_used_bytes": data.get("staging_used_bytes"),
        "staging_available_bytes": data.get("staging_available_bytes"),
        "staging_pressure_level": data.get("staging_pressure_level"),
        "last_sync": data.get("last_sync"),
    }


def metric_delta(before: dict[str, float], after: dict[str, float]) -> dict[str, Any]:
    mapping = {
        "writes_rejected_total": "writes_rejected_delta",
        "writes_blocked_total": "writes_blocked_delta",
        'cache_hits_total{cache_type="data"}': "cache_hits_delta",
        'cache_misses_total{cache_type="data"}': "cache_misses_delta",
    }
    out: dict[str, Any] = {}
    for source, target in mapping.items():
        if source in after:
            out[target] = after.get(source, 0) - before.get(source, 0)
    hits = after.get('cache_hits_total{cache_type="data"}', 0) - before.get('cache_hits_total{cache_type="data"}', 0)
    misses = after.get('cache_misses_total{cache_type="data"}', 0) - before.get('cache_misses_total{cache_type="data"}', 0)
    if hits + misses > 0:
        out["cache_hit_ratio"] = hits / (hits + misses)
    return out


def max_sample(key: str, samples: list[dict[str, Any]]) -> Any:
    values = [sample.get(key) for sample in samples if isinstance(sample.get(key), (int, float))]
    return max(values) if values else None


def prefix_metrics(prefix: str, metrics: dict[str, Any]) -> dict[str, Any]:
    return {prefix + key: value for key, value in metrics.items()}


def http_json(url: str, timeout: float) -> dict[str, Any]:
    return json.loads(http_text(url, timeout))


def http_text(url: str, timeout: float) -> str:
    with urllib.request.urlopen(url, timeout=timeout) as response:
        return response.read().decode("utf-8", errors="replace")


def ibm_cloud_instance_metadata() -> dict[str, Any]:
    request = urllib.request.Request(
        "http://169.254.169.254/metadata/v1/instance?version=2022-03-01",
        headers={"Metadata-Flavor": "ibm"},
    )
    try:
        with urllib.request.urlopen(request, timeout=1) as response:
            return json.loads(response.read().decode("utf-8", errors="replace"))
    except Exception:
        return {}


def detect_vm_size(instance_metadata: dict[str, Any]) -> str:
    profile = instance_metadata.get("profile")
    if isinstance(profile, dict):
        return str(profile.get("name") or profile.get("href") or "unavailable")
    if isinstance(profile, str):
        return profile
    return "unavailable"


def detect_cloud_zone(instance_metadata: dict[str, Any]) -> str:
    zone = instance_metadata.get("zone")
    if isinstance(zone, dict):
        return str(zone.get("name") or zone.get("href") or "unavailable")
    if isinstance(zone, str):
        return zone
    return "unavailable"


def parse_prometheus(text: str) -> dict[str, float]:
    metrics: dict[str, float] = {}
    for line in text.splitlines():
        if not line or line.startswith("#"):
            continue
        parts = line.rsplit(None, 1)
        if len(parts) != 2:
            continue
        try:
            metrics[parts[0]] = float(parts[1])
        except ValueError:
            continue
    return metrics


def command_output(command: list[str]) -> str:
    try:
        return subprocess.check_output(command, text=True, stderr=subprocess.STDOUT, timeout=10).strip()
    except Exception as exc:
        return f"unavailable: {exc}"


def run(command: list[str], timeout: float | None = None, check: bool = True) -> subprocess.CompletedProcess[str]:
    return subprocess.run(command, text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=timeout, check=check)


def run_shell(command: str, timeout: float | None = None, check: bool = True) -> subprocess.CompletedProcess[str]:
    return subprocess.run(command, shell=True, text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=timeout, check=check)


def require_tool(tool: str) -> None:
    if shutil.which(tool) is None:
        raise RuntimeError(f"required tool not found: {tool}")


def pgrep(name: str) -> list[int]:
    output = command_output(["pgrep", "-x", name])
    pids = []
    for line in output.splitlines():
        try:
            pids.append(int(line.strip()))
        except ValueError:
            continue
    return pids


def read_file(path: str) -> str:
    try:
        return Path(path).read_text(errors="replace")
    except Exception as exc:
        return f"unavailable: {exc}"


def sha256_file(path: Path | None) -> str | None:
    if path is None:
        return None
    digest = hashlib.sha256()
    with path.open("rb") as f:
        for block in iter(lambda: f.read(1024 * 1024), b""):
            digest.update(block)
    return digest.hexdigest()


def write_dict_csv(path: Path, rows: list[dict[str, Any]]) -> None:
    if not rows:
        path.write_text("")
        return
    keys: list[str] = []
    for row in rows:
        for key in row:
            if key not in keys:
                keys.append(key)
    with path.open("w", newline="") as f:
        writer = csv.DictWriter(f, fieldnames=keys)
        writer.writeheader()
        writer.writerows(rows)


def format_cell(value: Any) -> str:
    if isinstance(value, float):
        return f"{value:.3f}"
    if isinstance(value, int):
        return str(value)
    if value is None:
        return ""
    return str(value)


def shell_quote(value: str) -> str:
    return "'" + value.replace("'", "'\\''") + "'"


def safe_name(name: str) -> str:
    return "".join(ch if ch.isalnum() or ch in "-_." else "_" for ch in name)


def iso_now() -> str:
    return datetime.now(timezone.utc).isoformat()


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--mount", default="/mnt/cos-nfs", help="Mounted NFS path")
    parser.add_argument("--results-root", default="benchmark-results", help="Directory for timestamped result runs")
    parser.add_argument("--profile", choices=["quick", "standard", "full"], default="standard")
    parser.add_argument("--categories", nargs="*", choices=[
        "frontend-write",
        "sync",
        "read",
        "backpressure",
        "small-files",
        "crash-safety",
        "mixed",
    ])
    parser.add_argument("--config", default="configs/config.yaml")
    parser.add_argument("--debug-url", default=DEFAULT_DEBUG_URL)
    parser.add_argument("--metrics-url", default=DEFAULT_METRICS_URL)
    parser.add_argument("--health-url", default=DEFAULT_HEALTH_URL)
    parser.add_argument("--monitor-interval", type=float, default=1.0)
    parser.add_argument("--sync-timeout", type=float, default=900)
    parser.add_argument("--command-timeout", type=float, default=3600)
    parser.add_argument("--keep-data", action="store_true")
    parser.add_argument("--allow-backpressure", action="store_true")
    parser.add_argument("--backpressure-write-mib", type=int, default=2048)
    parser.add_argument("--allow-crash", action="store_true")
    parser.add_argument("--gateway-command", default="", help="Non-blocking shell command used to restart the gateway during crash tests")
    parser.add_argument("--post-restart-command", default="", help="Optional shell command run after gateway restart, for example remounting NFS")
    parser.add_argument("--crash-recovery-timeout", type=float, default=300)
    parser.add_argument("--skip-mount-check", action="store_true", help="Skip findmnt validation before running the suite")
    return parser.parse_args()


def main() -> int:
    args = parse_args()
    suite = BenchmarkSuite(args)
    return suite.run()


if __name__ == "__main__":
    sys.exit(main())
