#!/usr/bin/env python3
"""Census Linux test packages and partition the full suite into shards.

The baseline records build.yml patterns at b5aa78e6a. Other workflows run
partial or conditional selections, not a full-suite PR gate. Go selects test
files for the current platform.
"""
import argparse
import math
import subprocess

# Seconds observed in the 2026-11-05 Linux -short run. Five packages timed
# out at 600 seconds; those values are lower bounds, not finished runtimes.
# Packages absent here get a one-second provisional weight until measured.
TIMES = {
    "miner": 600, "internal/urnettools": 600, "mainnet": 600,
    "validator": 600, "sim-testnet": 600, "crv4": 175,
    "chain": 151, "clientauth": 128, "internal/durablehead": 115,
    "evmrpc": 48, "miner/onchain": 47, "commitmentoracle": 25,
    "hotkeywallet": 22, "internal/durablesync": 21, "ledger": 8,
    "payoutroster": 8, "scripts/server-fixture": 7,
}

# Every Linux test-bearing package is required; no exclusions currently.
# If one must be excluded, put its module-relative path and reason here.
EXCLUSIONS = {}
SHARDS = 3


def go(*args):
    return subprocess.check_output(["go", *args], text=True).splitlines()


def test_packages(*patterns):
    lines = go("list", "-f", "{{if or .TestGoFiles .XTestGoFiles}}{{.ImportPath}}{{end}}", *patterns)
    return {line for line in lines if line}


def partition(packages, module):
    """Assign expensive packages first, while keeping package counts balanced."""
    shards = [[] for _ in range(SHARDS)]
    weights = [0] * SHARDS
    cap = math.ceil(len(packages) / SHARDS)
    def weight(package):
        relative = package.removeprefix(module + "/")
        return TIMES.get(relative, 1)
    for package in sorted(packages, key=lambda p: (-weight(p), p)):
        index = min((i for i in range(SHARDS) if len(shards[i]) < cap),
                    key=lambda i: (weights[i], len(shards[i]), i))
        shards[index].append(package)
        weights[index] += weight(package)
    return shards, weights


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--shard", type=int, choices=range(SHARDS))
    parser.add_argument("--verify", action="store_true")
    args = parser.parse_args()
    module = go("list", "-m")[0]
    all_packages = test_packages("./...")
    excluded = {module + "/" + path for path in EXCLUSIONS}
    if excluded - all_packages or any(not reason.strip() for reason in EXCLUSIONS.values()):
        raise SystemExit("invalid or stale test exclusions")
    included = all_packages - excluded
    shards, weights = partition(included, module)
    if any(not shard for shard in shards) or set().union(*map(set, shards)) != included:
        raise SystemExit("invalid or empty shard partition")
    if sum(map(len, shards)) != len(included):
        raise SystemExit("overlapping shard partition")
    if args.shard is not None:
        print("\n".join(shards[args.shard]))
        return
    baseline = test_packages("./miner/...", "./internal/urnettools/...", "./cli/...", "./cmd/...")
    print(f"Linux test packages: {len(all_packages)}; baseline covered: {len(all_packages & baseline)}; baseline missing: {len(all_packages - baseline)}")
    print(f"Gate: {len(included)} included; {len(excluded)} excluded")
    for path, reason in sorted(EXCLUSIONS.items()):
        print(f"excluded {path}: {reason}")
    for i, shard in enumerate(shards):
        print(f"shard {i}: {len(shard)} packages, estimated aggregate {weights[i]}s")
        if args.verify:
            for package in shard:
                print(f"  {package}")


if __name__ == "__main__":
    main()
