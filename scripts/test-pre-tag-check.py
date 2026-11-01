#!/usr/bin/env python3
"""Exercise the quick gate's build matrix without running a real cross-build."""
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest


class PreTagMatrixTest(unittest.TestCase):
    def run_gate(self, fail_package=""):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            (root / "scripts").mkdir()
            (root / "bin").mkdir()
            shutil.copy(Path(__file__).with_name("pre-tag-check.sh"), root / "scripts")
            gofmt = root / "bin" / "gofmt"
            gofmt.write_text("#!/bin/sh\nexit 0\n")
            gofmt.chmod(0o755)
            go = root / "bin" / "go"
            go.write_text(
                '#!/bin/sh\n'
                'if [ "$1" = build ]; then\n'
                '  printf "%s/%s %s\\n" "$GOOS" "$GOARCH" "$4" >> "$BUILD_LOG"\n'
                '  [ "$4" != "$FAIL_PACKAGE" ] || exit 1\n'
                'fi\n'
            )
            go.chmod(0o755)
            env = os.environ.copy()
            env.update(PATH=f"{root / 'bin'}:{env['PATH']}", BUILD_LOG=str(root / "builds"),
                       FAIL_PACKAGE=fail_package, TMPDIR=tmp)
            result = subprocess.run(["bash", str(root / "scripts/pre-tag-check.sh"), "--quick"],
                                    env=env, text=True, capture_output=True)
            builds = (root / "builds").read_text().splitlines()
            return result, builds

    def test_every_shippable_package_in_every_matrix_cell(self):
        result, builds = self.run_gate()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        expected = [f"{osname}/{arch} {pkg}" for osname in ("linux", "darwin", "windows")
                    for arch in ("amd64", "arm64")
                    for pkg in ("./cli/miner", "./cmd/urnet-tools", "./cmd/urnet-docker")]
        self.assertEqual(builds, expected)

    def test_miner_failure_fails_gate(self):
        result, builds = self.run_gate("./cli/miner")
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(builds, ["linux/amd64 ./cli/miner"])
        self.assertNotIn("quick checks passed", result.stdout)


if __name__ == "__main__":
    unittest.main()
