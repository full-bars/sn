#!/usr/bin/env python3
"""Exercise only the extracted post-swap helper, never an installer or entrypoint."""
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[1]
SCRIPTS = (
    "scripts/Provider_Install_Linux.sh",
    "docker/miner/urnet-tools.sh",
    "docker/scripts/urnet-tools.sh",
    "docker/scripts/start_nightly.sh",
)


def helper(path):
    text = (ROOT / path).read_text()
    start = text.index("ensure_extender_cap ()\n(\n")
    end = text.index("\n)\n", start) + 3
    return text[start:end]


class ExtenderCapabilityTest(unittest.TestCase):
    def run_helper(self, script, initial, mode="success", platform="Linux", uid="0", sudo="none", env_sudo="", capbnd=None):
        with tempfile.TemporaryDirectory() as tmp:
            state = Path(tmp) / "state"
            calls = Path(tmp) / "calls"
            binary = Path(tmp) / "fake provider's binary"
            binary.touch()
            state.write_text(initial)
            status = Path(tmp) / "status"
            if capbnd is not None:
                status.write_text("CapBnd:\t" + capbnd + "\n")
            mock_path = Path(tmp) / "bin"
            mock_path.mkdir()
            for tool in ("sh", "awk", "cat", "sed"):
                executable = shutil.which(tool)
                if executable is None:
                    raise RuntimeError(f"missing test dependency: {tool}")
                (mock_path / tool).symlink_to(executable)
            env = dict(os.environ, STATE=str(state), CALLS=str(calls), PATH=str(mock_path),
                       MODE=mode, PLATFORM=platform, BINARY=str(binary), SUDO=env_sudo, UID_=uid, SUDO_MODE=sudo)
            harness = r'''
set -eu
uname() { printf '%s\n' "$PLATFORM"; }
getcap() { printf '%s %s\n' "$1" "$(cat "$STATE")"; }
setcap() {
    printf '%s\n' "$*" >> "$CALLS"
    [ "$MODE" != fail ] || return 1
    [ "$MODE" != unverified ] || return 0
    if [ "$1" = -r ]; then printf '%s' '' > "$STATE"; else printf '%s' 'cap_net_bind_service=ep' > "$STATE"; fi
}
id() { printf '%s\n' "$UID_"; }
sudo() {
    [ "$SUDO_MODE" = passwordless ] || return 1
    [ "$1" = -n ] && { shift; [ "$1" = true ] && return 0; }
    printf 'sudo ' >> "$CALLS"; "$@"
}
[ "$SUDO_MODE" != none ] || unset -f sudo
[ "$MODE" != missing-setcap ] || unset -f setcap
[ "$MODE" != missing-getcap ] || unset -f getcap
''' + helper(script).replace('/proc/self/status', str(status)) + '\nensure_extender_cap "$BINARY"\n'
            result = subprocess.run(["/bin/sh", "-c", harness], env=env,
                                    text=True, capture_output=True, check=False)
            return result, calls.read_text() if calls.exists() else "", str(binary)

    def test_scenarios(self):
        for script in SCRIPTS:
            for initial, mode, platform, expect_call, warn in (
                ("", "success", "Linux", True, False),
                ("cap_net_bind_service=ep", "success", "Linux", False, False),
                ("cap_chown,cap_net_bind_service=ep", "success", "Linux", False, False),
                ("cap_net_bind_service=p", "success", "Linux", True, False),
                ("", "fail", "Linux", True, True),
                ("", "unverified", "Linux", True, True),
                ("", "missing-setcap", "Linux", False, True),
                ("", "missing-getcap", "Linux", True, True),
                ("", "success", "Darwin", False, False),
            ):
                with self.subTest(script=script, initial=initial, mode=mode, platform=platform):
                    result, calls, binary = self.run_helper(script, initial, mode, platform)
                    self.assertEqual(bool(calls), expect_call)
                    if expect_call:
                        self.assertEqual(calls, f"cap_net_bind_service=+ep {binary}\n")
                    self.assertEqual("WARNING:" in result.stderr, warn)
                    if warn:
                        self.assertEqual(len(result.stderr.splitlines()), 1)
                        quoted = "'" + binary.replace("'", "'\\''") + "'"
                        self.assertIn("sudo setcap cap_net_bind_service=+ep " + quoted, result.stderr)

    def test_excluded_bounding_set_warns(self):
        for script in SCRIPTS:
            for initial in ("", "cap_net_bind_service=ep"):
                with self.subTest(script=script, initial=initial):
                    result, calls, _ = self.run_helper(script, initial, capbnd="00000000a80421fb")
                    if initial:
                        self.assertTrue(calls.startswith("-r "))
                    else:
                        self.assertEqual(calls, "")
                    self.assertIn("bounding set", result.stderr)

    def test_blocked_cap_does_not_remove_other_capabilities(self):
        for script in SCRIPTS:
            with self.subTest(script=script):
                result, calls, _ = self.run_helper(script, "cap_chown,cap_net_bind_service=ep", capbnd="00000000a80421fb")
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(calls, "")
                self.assertIn("other capabilities", result.stderr)

    def test_allowed_bounding_set(self):
        for script in SCRIPTS:
            for mask in ("000001ffffffffff", "00000000a80425fb"):
                for initial in ("", "cap_net_bind_service=ep"):
                    with self.subTest(script=script, mask=mask, initial=initial):
                        result, calls, binary = self.run_helper(script, initial, capbnd=mask)
                        self.assertEqual(calls, "" if initial else f"cap_net_bind_service=+ep {binary}\n")
                        self.assertEqual(result.stderr, "")

    def test_legacy_getcap_format_is_recognised(self):
        # libcap 2.2x (Ubuntu 16.04/18.04) prints "<file> = <caps>+<flags>".
        for script in SCRIPTS:
            for initial, expect_call in (
                ("= cap_net_bind_service+ep", False),
                ("= cap_chown,cap_net_bind_service+ep", False),
                ("= cap_net_bind_service+p", True),
                ("= cap_chown+ep", True),
            ):
                with self.subTest(script=script, initial=initial):
                    result, calls, _ = self.run_helper(script, initial)
                    self.assertEqual(bool(calls), expect_call)
                    self.assertEqual(result.stderr, "")

    def test_blocked_cap_removal_uses_passwordless_sudo(self):
        result, calls, binary = self.run_helper(SCRIPTS[0], "cap_net_bind_service=ep", uid="1000", sudo="passwordless", capbnd="00000000a80421fb")
        self.assertEqual(result.returncode, 0)
        self.assertEqual(calls, f"sudo -r {binary}\n")

    def test_installer_elevates_with_passwordless_sudo_when_SUDO_unset(self):
        installer = SCRIPTS[0]
        _, calls, binary = self.run_helper(installer, "", uid="1000", sudo="passwordless")
        self.assertEqual(calls, f"sudo cap_net_bind_service=+ep {binary}\n")
        # no passwordless sudo: plain setcap, as before
        _, calls, binary = self.run_helper(installer, "", uid="1000", sudo="none")
        self.assertEqual(calls, f"cap_net_bind_service=+ep {binary}\n")
        # root never wraps in sudo
        _, calls, binary = self.run_helper(installer, "", uid="0", sudo="passwordless")
        self.assertEqual(calls, f"cap_net_bind_service=+ep {binary}\n")
        # sudo needing a password must not be used
        _, calls, _ = self.run_helper(installer, "", uid="1000", sudo="password")
        self.assertNotIn("sudo", calls)

    def test_swap_sites_and_openrc_finalization(self):
        installer = (ROOT / SCRIPTS[0]).read_text()
        self.assertEqual(installer.count('ensure_extender_cap "$install_path/bin/urnetwork"'), 5)
        self.assertIn('chmod 0755 "$install_path"', installer)
        for script in SCRIPTS[1:]:
            text = (ROOT / script).read_text()
            self.assertEqual(text.count('    ensure_extender_cap "'), 1)


if __name__ == "__main__":
    unittest.main(verbosity=2)
