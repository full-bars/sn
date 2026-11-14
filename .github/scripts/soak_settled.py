#!/usr/bin/env python3
"""Settled resource baseline and check for one provider process.

A single sample is a poor basis for a leak verdict. Right after a churn window
the provider still holds goroutines that drain on their own, and right after
start it is still opening its connections. A leaked goroutine stays. So both
sides use the minimum of several samples taken a few seconds apart, and the
check waits for the provider to settle first.

The provider also adds proxies from public feeds during the run, so the number
of running slots is not constant. Each slot owns its own goroutines. The helper
reads the running slot count from the metrics page. When the count at the check
differs from the baseline, goroutines are compared per slot, so a slot that came
up from a feed is not read as a leak.

When the goroutine bound fails, the check prints the goroutine stack groups
that grew, taken from the provider's loopback profile listener, so a red run
names what leaked.

usage:
  soak_settled.py baseline PID METRICS_URL PPROF_URL DIR
  soak_settled.py check    PID METRICS_URL PPROF_URL DIR

Environment: SOAK_SAMPLES (5), SOAK_INTERVAL seconds (10), SOAK_SETTLE seconds
before the check (60), SOAK_GOROUTINE_PCT (5), SOAK_RSS_PCT (10).
"""
import os
import pathlib
import re
import subprocess
import sys
import time
import urllib.request

HERE = pathlib.Path(__file__).resolve().parent
SAMPLER = HERE / 'soak-growth.sh'
FIELDS = ('rss_kib', 'fds', 'goroutines')


def settled(samples):
    """Minimum of each field. A leak stays in the minimum, a transient does not.

    The slot count is the one from the last sample, because it is a level and
    not a noisy count. It is None when the metrics page has no slot gauge.
    """
    out = {k: min(s[k] for s in samples) for k in FIELDS}
    out['slots'] = samples[-1].get('slots')
    return out


def compare(base, now, goroutine_pct=5, rss_pct=10, fd_limit=1000):
    """Return the list of failed bounds, empty when the process is within them."""
    failed = []
    if now['rss_kib'] * 100 > base['rss_kib'] * (100 + rss_pct):
        failed.append(f'RSS grew more than {rss_pct}%')
    if now['fds'] > fd_limit:
        failed.append(f'FD count exceeds {fd_limit}')
    bs, ns = base.get('slots'), now.get('slots')
    if bs and ns and bs != ns:
        # Compare the cost of one slot. With a fixed overhead this is lenient when
        # slots were added, which is the safe side for a bound on leaks.
        if now['goroutines'] * bs * 100 > base['goroutines'] * ns * (100 + goroutine_pct):
            failed.append(f'goroutines per slot grew more than {goroutine_pct}% (slots {bs} -> {ns})')
    elif now['goroutines'] * 100 > base['goroutines'] * (100 + goroutine_pct):
        failed.append(f'goroutines grew more than {goroutine_pct}%')
    return failed


_SLOT_GAUGES = ('urnet_provider_client_ids_reused', 'urnet_provider_client_ids_minted')


def slot_count(metrics_text):
    """Running provider slots: the reused and minted gauges added together.

    Returns None when neither gauge is on the page, so an older provider is
    still measured, only without the per-slot comparison.
    """
    found = {}
    for line in metrics_text.splitlines():
        parts = line.split()
        if len(parts) == 2 and parts[0] in _SLOT_GAUGES and parts[1].isdigit():
            found[parts[0]] = int(parts[1])
    return sum(found.values()) if found else None


_HEADER = re.compile(r'^(\d+) @ ')
_FRAME = re.compile(r'^#\s+0x[0-9a-f]+\s+(\S+?)(?:\+0x[0-9a-f]+)?\s')


def groups(text, depth=3):
    """Count goroutines per stack from a pprof goroutine?debug=1 dump.

    The key is the first `depth` frames, so the same code path groups together
    across runs even when addresses differ.
    """
    counts = {}
    n = None
    frames = []

    def flush():
        if n is not None and frames:
            key = ' < '.join(frames[:depth])
            counts[key] = counts.get(key, 0) + n

    for line in text.splitlines():
        m = _HEADER.match(line)
        if m:
            flush()
            n, frames = int(m.group(1)), []
            continue
        f = _FRAME.match(line)
        if f and n is not None:
            frames.append(f.group(1))
    flush()
    return counts


def grown(base_text, now_text, top=15):
    """Stack groups that gained goroutines, largest gain first."""
    base, now = groups(base_text), groups(now_text)
    rows = [(now[k] - base.get(k, 0), base.get(k, 0), now[k], k) for k in now]
    rows = [r for r in rows if r[0] > 0]
    rows.sort(key=lambda r: (-r[0], r[3]))
    return rows[:top]


def sample(pid, url, tmp):
    subprocess.run(['sh', str(SAMPLER), 'baseline', str(pid), url, str(tmp)],
                   check=True, stdout=subprocess.DEVNULL)
    pid_, start, rss, fds, gor = tmp.read_text().split()
    row = {'pid': pid_, 'start': start, 'rss_kib': int(rss), 'fds': int(fds), 'goroutines': int(gor)}
    try:
        with urllib.request.urlopen(url, timeout=10) as r:
            row['slots'] = slot_count(r.read().decode('utf-8', 'replace'))
    except OSError:
        row['slots'] = None
    return row


def take(pid, url, count, interval, tmp):
    rows = []
    for i in range(count):
        if i:
            time.sleep(interval)
        rows.append(sample(pid, url, tmp))
    if len({(r['pid'], r['start']) for r in rows}) != 1:
        raise SystemExit('FAIL: provider restarted while sampling')
    return rows


def fetch_goroutines(pprof_url):
    with urllib.request.urlopen(pprof_url.rstrip('/') + '/debug/pprof/goroutine?debug=1', timeout=20) as r:
        return r.read().decode('utf-8', 'replace')


def main(argv):
    if len(argv) != 6 or argv[1] not in ('baseline', 'check'):
        print(__doc__, file=sys.stderr)
        return 2
    mode, pid, url, pprof_url, d = argv[1:]
    d = pathlib.Path(d)
    d.mkdir(parents=True, exist_ok=True)
    count = int(os.environ.get('SOAK_SAMPLES', '5'))
    interval = float(os.environ.get('SOAK_INTERVAL', '10'))
    tmp = d / 'sample.tmp'
    if mode == 'check':
        time.sleep(float(os.environ.get('SOAK_SETTLE', '60')))
    rows = take(pid, url, count, interval, tmp)
    now = settled(rows)
    print('samples goroutines:', [r['goroutines'] for r in rows], 'slots:', [r['slots'] for r in rows],
          'rss_kib:', [r['rss_kib'] for r in rows])
    print('settled', mode, now)
    try:
        stacks = fetch_goroutines(pprof_url)
    except OSError as e:
        stacks = ''
        print(f'warning: goroutine profile unavailable: {e}', file=sys.stderr)
    (d / f'goroutines-{mode}.txt').write_text(stacks)
    ident = {'pid': rows[0]['pid'], 'start': rows[0]['start']}
    if mode == 'baseline':
        (d / 'settled-baseline').write_text(
            ' '.join(str(v) for v in (ident['pid'], ident['start'], now['rss_kib'], now['fds'], now['goroutines'],
                                      now['slots'] if now['slots'] is not None else -1)) + '\n')
        return 0
    pid_, start, rss, fds, gor, slots = (d / 'settled-baseline').read_text().split()
    if (pid_, start) != (ident['pid'], ident['start']):
        print('FAIL: provider restarted since baseline', file=sys.stderr)
        return 1
    base = {'rss_kib': int(rss), 'fds': int(fds), 'goroutines': int(gor), 'slots': int(slots) if int(slots) >= 0 else None}
    print('baseline', base)
    failed = compare(base, now,
                     goroutine_pct=int(os.environ.get('SOAK_GOROUTINE_PCT', '5')),
                     rss_pct=int(os.environ.get('SOAK_RSS_PCT', '10')))
    if failed:
        for f in failed:
            print('FAIL:', f, file=sys.stderr)
        base_text = (d / 'goroutines-baseline.txt').read_text() if (d / 'goroutines-baseline.txt').exists() else ''
        print('goroutine stack groups that grew since the baseline (gain, before, after, top frames):')
        for gain, before, after, key in grown(base_text, stacks):
            print(f'  +{gain:<4} {before:>4} -> {after:<4} {key}')
        return 1
    print('PASS: settled resource growth within bounds')
    return 0


if __name__ == '__main__':
    sys.exit(main(sys.argv))
