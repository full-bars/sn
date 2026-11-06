#!/usr/bin/env python3
"""Wait for the provider's live reload to apply one churn change.

usage: check-churn-reload.py LOG START added|removed PROXY [ADD_START]

LOG is the provider log, START the byte offset taken just before the proxy
change and PROXY the churn proxy as host:port. The check polls the log until
the reload watcher reports the change, or fails at the deadline.

An add must log `reload complete: added N` with N >= 1.

A removal must log `removed N` with N >= 1, with one exception. A slot whose
worker fails (for example, the public proxy stops answering) withdraws itself
from the running set, so the operator's removal then has nothing to stop and
the watcher truthfully logs `removed 0`. ADD_START is the offset taken before
the matching add: the removal also passes when the watcher applied the change
and the churn slot is logged as stopped between that add and START. That case prints a
warning, as it did not exercise a live removal.

CHURN_RELOAD_TIMEOUT and CHURN_RELOAD_INTERVAL (seconds) tune the poll.
"""
import os
import re
import sys
import time

COMPLETE = re.compile(r'reload complete: [^\n]*')


def read_from(path, offset, end=None):
    with open(path, 'rb') as f:
        f.seek(offset)
        data = f.read() if end is None else f.read(max(0, end - offset))
        return data.decode(errors='replace')


def verdict(path, start, expected, proxy, add_start):
    """Return (passed, message) for the log as it is now."""
    text = read_from(path, start)
    counted = re.compile(r'reload complete: [^\n]*\b' + expected + r' ([1-9][0-9]*)')
    match = counted.search(text)
    if match:
        return True, 'live reload ' + expected + ': ' + match.group(0)
    if expected != 'removed' or add_start is None:
        return False, None
    complete = COMPLETE.search(text)
    if not complete:
        return False, None
    stopped = re.compile(r'reloaded slot ' + re.escape(proxy) + r' \(provider [0-9]+\) stopped:[^\n]*')
    # Only a stop between the add and the removal excuses `removed 0`. A stop
    # logged after START happened after the operator's removal, so it cannot
    # be why that removal found nothing to stop.
    died = stopped.search(read_from(path, add_start, start))
    if died:
        return True, ('::warning::churn slot had already stopped before its removal, so the reload '
                      'had nothing to remove (' + died.group(0).strip() + '; ' + complete.group(0) + ')')
    return False, None


def diagnostics(path, offset):
    lines = [line for line in read_from(path, offset).splitlines() if '[proxy]' in line]
    return '\n'.join(lines[-40:])


def main(argv):
    if len(argv) not in (5, 6) or argv[3] not in ('added', 'removed'):
        sys.exit(__doc__.strip().splitlines()[2])
    path, start, expected, proxy = argv[1], int(argv[2]), argv[3], argv[4]
    add_start = int(argv[5]) if len(argv) == 6 else None
    timeout = float(os.environ.get('CHURN_RELOAD_TIMEOUT', '60'))
    interval = float(os.environ.get('CHURN_RELOAD_INTERVAL', '2'))
    deadline = time.monotonic() + timeout
    while True:
        passed, message = verdict(path, start, expected, proxy, add_start)
        if passed:
            print(message)
            return 0
        if time.monotonic() >= deadline:
            break
        time.sleep(interval)
    since = start if add_start is None else min(start, add_start)
    print('provider [proxy] lines since the change:\n' + diagnostics(path, since), file=sys.stderr)
    sys.exit('No live reload ' + expected + ' a slot within ' + str(int(timeout)) + 's')


if __name__ == '__main__':
    sys.exit(main(sys.argv))
