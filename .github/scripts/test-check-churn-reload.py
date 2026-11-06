#!/usr/bin/env python3
"""Exercise check-churn-reload.py against crafted provider logs."""
import os
import pathlib
import subprocess
import sys
import tempfile
import threading
import time

SCRIPT = pathlib.Path(__file__).with_name('check-churn-reload.py')
PROXY = '45.74.31.40:4052'
OTHER = '176.65.132.138:1080'


def complete(added=0, removed=0):
    return f'✅ [proxy] reload complete: added {added}, removed {removed}, rotated 0, unchanged 3, lost 0, skipped 0\n'


def stopped(proxy):
    return f'❌ [proxy] reloaded slot {proxy} (provider 5) stopped: auth failed\n'


def run(name, before, after, expected, want_pass, add_before='', late='', delay=0.0, timeout='3'):
    """before is logged before ADD_START, add_before between ADD_START and
    START, after right after START and late after delay seconds."""
    with tempfile.TemporaryDirectory() as tmp:
        log = os.path.join(tmp, 'provider.log')
        with open(log, 'w') as f:
            f.write(before)
        add_start = os.path.getsize(log)
        with open(log, 'a') as f:
            f.write(add_before)
        start = os.path.getsize(log)
        with open(log, 'a') as f:
            f.write(after)

        def append_late():
            time.sleep(delay)
            with open(log, 'a') as f:
                f.write(late)

        writer = threading.Thread(target=append_late)
        writer.start()
        env = dict(os.environ, CHURN_RELOAD_TIMEOUT=timeout, CHURN_RELOAD_INTERVAL='0.2')
        result = subprocess.run([sys.executable, str(SCRIPT), log, str(start), expected, PROXY, str(add_start)],
                                text=True, capture_output=True, env=env)
        writer.join()
    passed = result.returncode == 0
    status = 'ok' if passed == want_pass else 'FAIL'
    print(f'{status}: {name}: rc={result.returncode} {result.stdout.strip()} {result.stderr.strip()[-160:]}', flush=True)
    return passed == want_pass, result


def main():
    results = [
        run('add logged late passes', '', '', 'added', True, late=complete(added=1), delay=1.0),
        run('removal logged late passes', '', '', 'removed', True, late=complete(removed=1), delay=1.0),
        run('removal never logged fails', '', complete(), 'removed', False),
        run('no reload at all fails', '', '', 'removed', False),
        run('removal before START is ignored', complete(removed=1), '', 'removed', False),
        run('add never logged fails', '', complete(), 'added', False),
        run('slot stopped since add, removed 0 passes', '', complete(), 'removed', True, add_before=stopped(PROXY)),
        run('slot stopped since add, reload logged late passes', '', '', 'removed', True,
            add_before=stopped(PROXY), late=complete(), delay=1.0),
        run('slot stopped before the add fails', stopped(PROXY), complete(), 'removed', False),
        run('another slot stopped fails', '', complete(), 'removed', False, add_before=stopped(OTHER)),
        run('slot stopped but no reload applied fails', '', '', 'removed', False, add_before=stopped(PROXY)),
        run('a stopped slot never excuses an add', '', complete(), 'added', False, add_before=stopped(PROXY)),
    ]
    failures = sum(1 for good, _ in results if not good)
    print(f'{len(results) - failures}/{len(results)} cases ok')
    if 'No live reload removed a slot within 3s' not in results[3][1].stderr:
        print('FAIL: deadline message missing', flush=True)
        failures += 1
    if '::warning::' not in results[6][1].stdout:
        print('FAIL: dead-slot pass did not warn', flush=True)
        failures += 1
    sys.exit(1 if failures else 0)


if __name__ == '__main__':
    main()
