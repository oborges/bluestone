# Bluestone Benchmarking

The formal benchmark framework is documented in [BENCHMARK_SUITE.md](BENCHMARK_SUITE.md). It produces timestamped result directories with human-readable summaries, JSON, CSV, raw fio output, monitor samples, environment capture, and a compact baseline format for future comparisons.

Run a standard benchmark against an already-running and mounted gateway:

```bash
./scripts/run_benchmark_suite.sh
```

Run a short smoke benchmark:

```bash
PROFILE=quick ./scripts/run_benchmark_suite.sh
```

The older `scripts/run_stress_tests.sh` script is still available for ad-hoc stress exploration, but benchmark comparisons should use `scripts/benchmark_suite.py` or the wrapper above so outputs remain repeatable and machine-comparable.
