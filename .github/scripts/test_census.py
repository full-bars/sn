#!/usr/bin/env python3
"""Census Linux test packages and partition the full suite into shards.

The baseline records build.yml patterns at b5aa78e6a. Other workflows run
partial or conditional selections, not a full-suite PR gate. Go selects test
files for the current platform.

A shard prints one unit per line: a package path, or a package path with
"<index>/<count>" for one name split of a package too slow for one process.
"""
import argparse
import math
import os
import re
import subprocess
import sys
import zlib

# Seconds observed in the 2026-11-05 hosted -short shard runs; mainnet and
# sim-testnet timed out at 600 seconds, so theirs are lower bounds. Packages
# absent here took under 3 seconds and get a one-second weight.
TIMES = {
    "validator": 558, "miner": 334, "internal/urnettools": 268,
    "mainnet": 600, "sim-testnet": 600, "crv4": 87, "miner/onchain": 48,
    "payoutartifact": 8, "scripts/source-graph": 6, "clientauth": 5,
    "chain": 4, "internal/durablehead": 3, "evmrpc": 3,
}

# Packages the PR gate does not run, by module-relative path, each with the
# reason. Every entry needs an operator decision before it is permanent.
EXCLUSIONS = {
    "mainnet": "release-gate package: about 2,800 fsync-bound tests that run "
               "one at a time, hours of serial test time; "
               "scripts/test-release-1.0-local.sh runs it in 8 name shards "
               "with 510-minute deadlines",
    "sim-testnet": "release-gate package: about 3,600 tests sized for 5 normal "
                   "and 17 race name shards with 80 to 210-minute deadlines, "
                   "and its gate-contract tests read the sibling server, "
                   "connect and sdk checkouts of the release workspace; "
                   "scripts/test-release-1.0-local.sh runs it there",
}

# Packages too slow for one 600-second test process, by module-relative path:
# (number of name splits, reason). Split i of n runs the tests whose name's
# CRC-32 mod n is i - 1, the rule scripts/test-release-1.0-local.sh uses, so a
# new test joins a split without an edit.
SPLITS = {
    "validator": (2, "about 3,000 tests took 559 of the 600 seconds in one "
                     "hosted process"),
}

# Tests the PR gate skips by exact name, by module-relative package path, each
# with the reason. Every entry needs an operator decision.
QUARANTINE = {}
SHARDS = 3
# A -run selector is one argument; Linux caps one argument at 128 KiB.
MAX_SELECTOR = 120_000
TEST_NAME = re.compile(r"(Test|Fuzz|Example)[A-Za-z0-9_]*")


def go(*args):
    return subprocess.check_output(["go", *args], text=True).splitlines()


def test_packages(*patterns):
    lines = go("list", "-f", "{{if or .TestGoFiles .XTestGoFiles}}{{.ImportPath}}{{end}}", *patterns)
    return {line for line in lines if line}


def relative(package, module):
    return package.removeprefix(module + "/")


def units(packages, module):
    """Expand each split package into its name splits."""
    result = []
    for package in packages:
        count = SPLITS.get(relative(package, module), (1, ""))[0]
        if count == 1:
            result.append(package)
        else:
            result.extend(f"{package} {i}/{count}" for i in range(1, count + 1))
    return result


def partition(packages, module):
    """Assign expensive units first, while keeping unit counts balanced."""
    items = units(packages, module)
    shards = [[] for _ in range(SHARDS)]
    weights = [0] * SHARDS
    cap = math.ceil(len(items) / SHARDS)
    def weight(unit):
        package = unit.split(" ")[0]
        count = SPLITS.get(relative(package, module), (1, ""))[0]
        return TIMES.get(relative(package, module), 1) / count
    for unit in sorted(items, key=lambda u: (-weight(u), u)):
        index = min((i for i in range(SHARDS) if len(shards[i]) < cap),
                    key=lambda i: (weights[i], len(shards[i]), i))
        shards[index].append(unit)
        weights[index] += weight(unit)
    return shards, [round(w) for w in weights]


def split_selector(names, index, count, package):
    """The -run selector of split index of count, from go test -list output."""
    own = []
    for line in names:
        if TEST_NAME.fullmatch(line):
            if zlib.crc32(line.encode()) % count == index - 1:
                own.append(line)
        elif re.match("(Test|Fuzz|Example)", line):
            raise SystemExit(f"unexpected {package} test name {line!r}")
    if not own:
        raise SystemExit(f"{package} split {index}/{count} selects no test")
    selector = "^(" + "|".join(own) + ")$"
    if len(selector) > MAX_SELECTOR:
        raise SystemExit(f"{package} split {index}/{count} selector is too long; raise its split count")
    return selector


def skip_selector(packages, module):
    """The -skip selector of the quarantined tests of these packages."""
    names = sorted(name for package in packages
                   for name in QUARANTINE.get(relative(package, module), {}))
    return "^(" + "|".join(names) + ")$" if names else ""


def quarantine_errors(root, all_packages, module):
    """Each quarantined name must be one test, defined in its own package only."""
    errors = []
    definitions = {}
    for directory, dirs, files in os.walk(root):
        dirs[:] = [d for d in dirs if not d.startswith(".")]
        for file in files:
            if file.endswith("_test.go"):
                path = os.path.join(directory, file)
                with open(path, encoding="utf-8", errors="replace") as source:
                    for name in re.findall(r"^func (Test[A-Za-z0-9_]*)\(", source.read(), re.M):
                        definitions.setdefault(name, set()).add(os.path.relpath(directory, root))
    for path, tests in QUARANTINE.items():
        if module + "/" + path not in all_packages:
            errors.append(f"quarantine names no test package: {path}")
        for name, reason in tests.items():
            if not reason.strip():
                errors.append(f"quarantined {path} {name} has no reason")
            if definitions.get(name) != {path}:
                errors.append(f"quarantined {name} must be defined in {path} only: {sorted(definitions.get(name, []))}")
    return errors


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--shard", type=int, choices=range(SHARDS))
    parser.add_argument("--skip", type=int, choices=range(SHARDS), help="print the shard's -skip selector")
    parser.add_argument("--select", nargs=2, metavar=("PACKAGE", "INDEX/COUNT"), help="print a split's -run selector")
    parser.add_argument("--verify", action="store_true")
    args = parser.parse_args()
    if args.select:
        package, split = args.select
        index, count = map(int, split.split("/"))
        names = go("test", "-list", ".", package)
        print(split_selector(names, index, count, package), end="")
        return
    module = go("list", "-m")[0]
    all_packages = test_packages("./...")
    excluded = {module + "/" + path for path in EXCLUSIONS}
    if excluded - all_packages or any(not reason.strip() for reason in EXCLUSIONS.values()):
        raise SystemExit("invalid or stale test exclusions")
    included = all_packages - excluded
    if any(module + "/" + path not in included or count < 2 or not reason.strip()
           for path, (count, reason) in SPLITS.items()):
        raise SystemExit("invalid or stale test splits")
    errors = quarantine_errors(go("list", "-m", "-f", "{{.Dir}}")[0], included, module)
    if errors:
        raise SystemExit("\n".join(errors))
    shards, weights = partition(included, module)
    packages = [{unit.split(" ")[0] for unit in shard} for shard in shards]
    if any(not shard for shard in shards) or set().union(*packages) != included:
        raise SystemExit("invalid or empty shard partition")
    if sum(map(len, shards)) != len(units(included, module)) or len(set().union(*map(set, shards))) != sum(map(len, shards)):
        raise SystemExit("overlapping shard partition")
    if args.shard is not None:
        print("\n".join(shards[args.shard]))
        return
    if args.skip is not None:
        print(skip_selector(packages[args.skip], module), end="")
        return
    baseline = test_packages("./miner/...", "./internal/urnettools/...", "./cli/...", "./cmd/...")
    print(f"Linux test packages: {len(all_packages)}; baseline covered: {len(all_packages & baseline)}; baseline missing: {len(all_packages - baseline)}")
    quarantined = sum(len(tests) for tests in QUARANTINE.values())
    print(f"Gate: {len(included)} included; {len(excluded)} excluded; {len(SPLITS)} split by test name; {quarantined} tests quarantined")
    for path, reason in sorted(EXCLUSIONS.items()):
        print(f"excluded {path}: {reason}")
    for path, (count, reason) in sorted(SPLITS.items()):
        print(f"split {path} into {count}: {reason}")
    for path, tests in sorted(QUARANTINE.items()):
        for name, reason in sorted(tests.items()):
            print(f"quarantined {path} {name}: {reason}")
    for i, shard in enumerate(shards):
        print(f"shard {i}: {len(shard)} units, estimated aggregate {weights[i]}s")
        if args.verify:
            for unit in shard:
                print(f"  {unit}")
    sys.stdout.flush()


if __name__ == "__main__":
    main()
