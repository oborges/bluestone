# Bluestone Benchmark Suite

This suite provides repeatable, machine-comparable benchmarks for Bluestone. It assumes the gateway is already running and mounted, and it does not change gateway behavior.

The suite writes every run into a timestamped directory under `benchmark-results/` by default. Each run captures the environment, raw fio output, monitor samples, summary tables, JSON, CSV, and a compact baseline file for future comparisons.

## Prerequisites

- A running Bluestone gateway.
- The NFS export mounted, normally at `/mnt/cos-nfs`.
- `fio` installed on the benchmark host for write/read performance tests.
- Debug and metrics endpoints enabled for sync, staging, cache, and backpressure metrics.
- Sufficient COS credentials and bucket configuration for the mounted gateway.

Recommended mount command:

```bash
sudo mount -t nfs4 -o vers=4.0,tcp,port=2049 localhost:/ /mnt/cos-nfs
```

## Quick Start

Run a short smoke benchmark:

```bash
PROFILE=quick ./scripts/run_benchmark_suite.sh
```

Run the standard benchmark profile:

```bash
PROFILE=standard ./scripts/run_benchmark_suite.sh
```

Run only selected categories:

```bash
./scripts/run_benchmark_suite.sh --categories frontend-write sync read
```

Run the full profile, including 10 GiB frontend write coverage:

```bash
PROFILE=full ./scripts/run_benchmark_suite.sh
```

## Output Files

Each run creates `benchmark-results/benchmark-<timestamp>/` with:

- `SUMMARY.md`: human-readable benchmark summary.
- `results.json`: full structured results with per-test metrics and samples.
- `results.csv`: tabular results for spreadsheets and dashboards.
- `baseline.json`: compact machine-comparable baseline.
- `environment.json`: config, mount options, host details, disk details, fio version, and gateway commit hash.
- `monitor_samples.csv`: sync queue, dirty bytes, staging pressure, cache, and backpressure samples over time.
- `raw/*.fio.json`: raw fio output for fio-backed tests.

## Benchmark Categories

- `frontend-write`: sequential writes at 100 MiB, 1 GiB, and 10 GiB depending on profile, in direct and non-direct modes.
- `sync`: time-to-durable in COS, sync throughput, queue depth, and dirty bytes.
- `read`: cold COS read, warm local-cache read, partial/range read, random 4K read, and large sequential read.
- `backpressure`: below-high, above-high, above-critical, block-mode, fail-fast-mode, and sync-drain pressure scenarios.
- `small-files`: create/read/delete 100, 1k, and 10k small files depending on profile.
- `crash-safety`: crash before sync, during sync, and during multipart upload.
- `mixed`: concurrent readers/writers, large writes plus small reads, and dirty-file reads during sync.

## Safety Modes

Backpressure and crash tests are intentionally opt-in because they can fill staging or kill the gateway process.

Enable backpressure scenarios:

```bash
./scripts/run_benchmark_suite.sh \
  --categories backpressure \
  --allow-backpressure \
  --backpressure-write-mib 2048
```

Enable crash-safety scenarios with a non-blocking restart command:

```bash
./scripts/run_benchmark_suite.sh \
  --categories crash-safety \
  --allow-crash \
  --gateway-command 'cd ~/bluestone && sudo nohup ./bin/bluestone --config configs/config.yaml >/tmp/bluestone-benchmark.log 2>&1 &' \
  --post-restart-command 'sudo umount /mnt/cos-nfs -f || true; sudo mount -t nfs4 -o vers=4.0,tcp,port=2049 localhost:/ /mnt/cos-nfs'
```

## Profiles

- `quick`: smoke-sized run for validation before a longer benchmark.
- `standard`: default profile for regular comparisons.
- `full`: includes the largest write and small-file workloads and should be used for release-level validation.

## Baseline Comparison

Use `baseline.json` as the stable comparison artifact across commits, VM sizes, and gateway configurations. The schema is intentionally compact:

```json
{
  "schema_version": "cos-bluestone-baseline/v1",
  "commit": "<git commit>",
  "profile": "standard",
  "benchmarks": {
    "read.warm-read-local-cache": {
      "throughput_mib_s": 80.0,
      "p95_latency_ms": 4.2,
      "error_count": 0
    }
  }
}
```

The full run context stays in `environment.json`, so baseline files remain small enough for CI comparisons while still being traceable.
