#!/usr/bin/env python3
"""Print Linux test packages missing from the build workflow's full suite.

The baseline records the package patterns in build.yml at b5aa78e6a. Other
workflows run partial selections, or run only for specific paths or tags; they
are not a full-suite PR gate. Go selects files for the current platform.
"""
import subprocess


def go(*args):
    return subprocess.check_output(["go", *args], text=True).splitlines()


def test_packages(*patterns):
    lines = go("list", "-f", "{{if or .TestGoFiles .XTestGoFiles}}{{.ImportPath}}{{end}}", *patterns)
    return {line for line in lines if line}


def main():
    all_packages = test_packages("./...")
    covered = test_packages("./miner/...", "./internal/urnettools/...", "./cli/...", "./cmd/...")
    missing = sorted(all_packages - covered)
    print(f"Linux test packages: {len(all_packages)}; covered: {len(all_packages & covered)}; missing: {len(missing)}")
    for package in missing:
        print(package)


if __name__ == "__main__":
    main()
