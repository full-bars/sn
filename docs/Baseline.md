# Baseline recorder

Every provider keeps a small local record of its own behaviour in `~/.urnetwork/baseline.jsonl`. It is on by default and needs no setup, so an upgrade can be judged against what the box did before it, instead of against a baseline somebody remembered to take.

It holds counts and totals only, never proxy addresses, usernames or passwords. A field that could not be measured is omitted rather than written as zero, because a zero reads as "measured, and the answer was nothing", which for host memory would make a healthy box look like a catastrophic one.

> [!NOTE]
> The recorder starts with the release that ships it. A box coming from an older release has no earlier record, so `baseline compare` becomes useful from the next upgrade. Run `urnet-tools baseline mark "before <version>"` just before updating to mark the boundary yourself.

## What is recorded

A start mark at launch (with the version and the previous version), then one sample every 15 minutes, the first at 5 minutes because a sample at t=0 measures a pool that has not launched yet. The file is bounded at 1 MiB and keeps the newest whole lines. A hot swap has both a parent and a candidate running, so appends and rotation are serialized with an inter-process lock.

A sample carries the node state and its reason, the proxies running against the desired count, the trim cap, billable and total traffic and the lifetime billable counter, connected clients, contracts, process memory, host memory available, swap used, pressure stall (PSI) averages, load, the restart reason, the OOM kill counter and the top error categories. On Linux, host memory is the tighter of the host and the provider's cgroup, with `avail_source` naming which supplied it, so a container is not compared against a systemd box as if it were like for like. Off Linux there is no `/proc`, cgroup or PSI to read, so those fields are omitted and the rest still records.

## Commands

| Command | What it does |
|---|---|
| `urnet-tools baseline show [-n N] [--json]` | Print the newest rows (default 20, at most 200). Reads the file directly, so it works on a box whose provider is stopped. |
| `urnet-tools baseline mark <label>` | Annotate the timeline, for example just before an upgrade. Goes through the control socket so the provider stays the only writer. A label is required, because an unlabelled mark is a boundary `compare` cannot use. Works while recording is off and never deletes the file. |
| `urnet-tools baseline compare [--from A] [--to B] [--skip-ramp 20m] [--json]` | Compare the box before and after an upgrade. With no arguments it splits at the most recent start whose version differs from the previous one. `A` and `B` are a mark label or a timestamp prefix. |
| `urnet-tools set baseline on\|off` | Start or stop recording at once, without a restart. The existing file is kept, because it is the only copy of the box's pre-upgrade behaviour. Clearing the key restores `on`. |

`URNETWORK_BASELINE_INTERVAL` changes the sample interval (default `15m`, floor one minute), for a faster canary or a shorter soak.

## What `baseline compare` computes

The billable rate is the lifetime byte counter's delta divided by the time delta, never an average of the instantaneous rates. Instantaneous readings are bursty (4.6 KiB/s now and 56 KiB/s a minute later is ordinary), and averaging them reports a figure that never happened.

- A counter that went down was reset (a fresh lifetime store or a wiped state directory), and that interval is skipped rather than reported.
- Rows with no lifetime total are skipped, since the store may not have been running for the whole segment.
- The first `--skip-ramp` (default 20 minutes) after every start mark is excluded from both sides. A pool that just restarted under-earns while it ramps, and those minutes would otherwise make the comparison a story about the ramp. The settled rows between two restarts in one segment are kept.
- A segment with fewer than 4 samples prints `insufficient data` and no percentages, because a segment that short may sit entirely inside one burst.
- Capacity and availability-source changes are warned about. A box trimmed from 1170 to 500 proxies is not a regression, and a container reporting host memory is not comparable to a systemd box. The `desired` and `trim cap` rows are always printed so the two sides can be read side by side.

Rows: billable KiB/s, proxies up (mean), clients (mean), RSS (mean and max), host available MiB (min), swap used MiB (max), PSI full avg60 (mean and max), desired, trim cap, restarts, OOM kills. Every row shows before, after and the change.

## Log line

| Message | Meaning |
|---|---|
| `[baseline] cannot write baseline.jsonl` | A sample could not be appended: a full or read-only disk, or no state directory. At most one line per hour, so a full disk does not log once per sample. Recording resumes by itself when the write succeeds, and nothing else changes: the recorder never affects the proxy pool. |

## How this relates to Prometheus

Prometheus is the fleet view: it answers "how is this node doing right now" across every box. The baseline recorder answers the one question it does not, which is whether an upgrade made this box worse. It is not a replacement for the fleet view and it is not a time-series database.
