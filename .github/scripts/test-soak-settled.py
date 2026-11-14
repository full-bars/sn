#!/usr/bin/env python3
"""Exercise the pure logic of soak_settled.py."""
import importlib.util
import pathlib
import unittest

spec = importlib.util.spec_from_file_location('soak_settled', pathlib.Path(__file__).with_name('soak_settled.py'))
assert spec is not None and spec.loader is not None
ss = importlib.util.module_from_spec(spec)
spec.loader.exec_module(ss)

BASE = """goroutine profile: total 7
4 @ 0x43e8ee 0x4499c6 0x6e1a05 0x4723a1
#	0x6e1a04	github.com/urnetwork/connect.(*Client).run+0x44	/src/transfer.go:100
#	0x4499c5	runtime.selectgo+0x2a5	/usr/lib/go/src/runtime/select.go:335

2 @ 0x43e8ee 0x4499c6 0x7f0001 0x4723a1
#	0x7f0000	github.com/urnetwork/connect.(*OutOfBandControl).retry+0x10	/src/oob.go:40
#	0x4499c5	runtime.selectgo+0x2a5	/usr/lib/go/src/runtime/select.go:335

1 @ 0x43e8ee 0x4723a1
#	0x4723a0	runtime.goexit+0x1	/usr/lib/go/src/runtime/asm_amd64.s:1700
"""

NOW = BASE.replace('2 @ 0x43e8ee 0x4499c6 0x7f0001', '9 @ 0x43e8ee 0x4499c6 0x7f0001') + """
3 @ 0x43e8ee 0x4499c6 0x7f0002 0x4723a1
#	0x7f0003	github.com/urnetwork/connect.(*Window).monitor+0x20	/src/window.go:7
#	0x4499c5	runtime.selectgo+0x2a5	/usr/lib/go/src/runtime/select.go:335
"""


def sample(g, rss=1000, fds=20):
    return {'rss_kib': rss, 'fds': fds, 'goroutines': g}


class Settled(unittest.TestCase):
    def test_minimum_drops_a_transient_spike(self):
        rows = [sample(330), sample(300), sample(272), sample(271), sample(290)]
        self.assertEqual(ss.settled(rows)['goroutines'], 271)

    def test_minimum_keeps_a_persistent_leak(self):
        rows = [sample(330), sample(329), sample(331), sample(329), sample(330)]
        self.assertEqual(ss.settled(rows)['goroutines'], 329)


class Compare(unittest.TestCase):
    def test_within_bounds(self):
        self.assertEqual(ss.compare(sample(272), sample(285)), [])

    def test_goroutine_bound(self):
        self.assertEqual(ss.compare(sample(272), sample(286)), ['goroutines grew more than 5%'])

    def test_rss_and_fds(self):
        out = ss.compare(sample(100, rss=1000), sample(100, rss=1200, fds=2000))
        self.assertEqual(out, ['RSS grew more than 10%', 'FD count exceeds 1000'])

    def test_exactly_at_the_bound_passes(self):
        self.assertEqual(ss.compare(sample(200), sample(210)), [])


class Groups(unittest.TestCase):
    def test_groups_by_leading_frames(self):
        g = ss.groups(BASE)
        self.assertEqual(sum(g.values()), 7)
        self.assertEqual(g['github.com/urnetwork/connect.(*Client).run < runtime.selectgo'], 4)

    def test_grown_names_the_leak(self):
        rows = ss.grown(BASE, NOW)
        self.assertEqual([r[0] for r in rows], [7, 3])
        self.assertIn('(*OutOfBandControl).retry', rows[0][3])
        self.assertEqual((rows[0][1], rows[0][2]), (2, 9))
        self.assertIn('(*Window).monitor', rows[1][3])

    def test_no_growth_is_empty(self):
        self.assertEqual(ss.grown(BASE, BASE), [])

    def test_empty_baseline_lists_everything_as_new(self):
        self.assertEqual(sum(r[0] for r in ss.grown('', BASE)), 7)


def slotted(g, slots, rss=1000, fds=20):
    return {'rss_kib': rss, 'fds': fds, 'goroutines': g, 'slots': slots}


class Slots(unittest.TestCase):
    def test_slot_count_adds_the_two_gauges(self):
        page = "urnet_goroutines 300\nurnet_provider_client_ids_reused 2\nurnet_provider_client_ids_minted 1\n"
        self.assertEqual(ss.slot_count(page), 3)

    def test_slot_count_is_none_without_the_gauges(self):
        self.assertIsNone(ss.slot_count("urnet_goroutines 300\n"))

    def test_slot_count_ignores_a_malformed_value(self):
        self.assertIsNone(ss.slot_count("urnet_provider_client_ids_reused abc\n"))

    def test_settled_keeps_the_last_slot_count(self):
        rows = [slotted(300, 3), slotted(290, 3), slotted(310, 4)]
        self.assertEqual(ss.settled(rows)['slots'], 4)

    def test_an_added_slot_is_not_a_leak(self):
        # 260 goroutines on 3 slots, 308 on 4: 87 against 77 per slot.
        self.assertEqual(ss.compare(slotted(260, 3), slotted(308, 4)), [])

    def test_a_leak_at_constant_slots_fails(self):
        self.assertEqual(ss.compare(slotted(260, 3), slotted(308, 3)), ['goroutines grew more than 5%'])

    def test_growth_per_slot_fails_when_slots_differ(self):
        out = ss.compare(slotted(260, 3), slotted(400, 4))
        self.assertEqual(out, ['goroutines per slot grew more than 5% (slots 3 -> 4)'])

    def test_a_missing_slot_count_falls_back_to_the_plain_bound(self):
        self.assertEqual(ss.compare(slotted(260, None), slotted(308, 4)), ['goroutines grew more than 5%'])


if __name__ == '__main__':
    unittest.main()
