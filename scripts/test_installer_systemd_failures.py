"""Exercise installer service changes with shell stubs, never host systemd."""
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

INSTALLER = Path(__file__).resolve().parent / "Provider_Install_Linux.sh"


class InstallerSystemdFailures(unittest.TestCase):
    def run_install(self, failure="", backup=False, existing=False, stopped=True):
        script = INSTALLER.read_text()
        start = script.index("install_systemd_units ()\n{\n")
        end = script.index("\n}\n", start) + 3
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            (root / "bin").mkdir()
            (root / "bin/urnetwork").write_text("new")
            if backup:
                (root / "bin/urnetwork.old").write_text("old")
            if existing:
                (root / "units").mkdir()
                (root / "units/urnetwork.service").write_text("[Service]\nType=simple\n")
            harness = r'''
set -eu
systemd_units_stopped="$STOPPED"
systemd_userdir="$ROOT/units"
systemd_service="$systemd_userdir/urnetwork.service"
systemd_update_service="$systemd_userdir/urnetwork-update.service"
systemd_update_timer="$systemd_userdir/urnetwork-update.timer"
install_path="$ROOT"
update_timer_oncalendar=daily
update_timer_randomized_delay=5m
sanitize_restart_dropins() { :; }
ensure_extender_cap() { :; }
id() { printf '1000\n'; }
pr_info() { printf 'INFO: %s\n' "$*"; }
pr_warn() { printf 'WARN: %s\n' "$*"; }
pr_err() { printf 'ERROR: %s\n' "$*"; }
systemctl() {
    printf '%s\n' "$*" >> "$ROOT/calls"
    case "$FAILURE:$*" in
        service:'--user enable urnetwork.service'|timer:'--user enable --now urnetwork-update.timer'|reload:'--user daemon-reload'|start:'--user start urnetwork.service') printf 'stub failure\n' >&2; return 1 ;;
    esac
    return 0
}
''' + script[start:end] + '\ninstall_systemd_units\n'
            result = subprocess.run(["sh", "-c", harness], env=dict(os.environ, ROOT=tmp, FAILURE=failure, STOPPED=str(int(stopped))), text=True, capture_output=True)
            return result, (root / "bin/urnetwork").read_text(), (root / "calls").read_text(), (root / "units/urnetwork.service").read_text()

    def test_fresh_and_existing_units_notify(self):
        for existing, stopped in ((False, False), (True, True), (True, False)):
            with self.subTest(existing=existing, stopped=stopped):
                result, _, _, unit = self.run_install(existing=existing, stopped=stopped)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertIn("\nType=notify\nNotifyAccess=all\n", unit)

    def test_partial_failures(self):
        for failure, text in (("service", "service enable failed"), ("timer", "timer enable/start did not complete"), ("reload", "daemon-reload failed")):
            with self.subTest(failure=failure):
                result, binary, calls, _ = self.run_install(failure)
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(binary, "new")
                self.assertIn(text, result.stdout)
                self.assertNotIn("--user start", calls)

    def test_failed_rollback_start_is_reported(self):
        result, binary, calls, _ = self.run_install("start", backup=True)
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(binary, "old")
        self.assertIn("did NOT restart on the previous binary", result.stdout)
        self.assertEqual(calls.count("--user start urnetwork.service"), 2)


if __name__ == "__main__":
    unittest.main()
