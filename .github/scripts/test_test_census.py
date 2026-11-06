#!/usr/bin/env python3
import unittest
import zlib

import test_census
from test_census import SHARDS, partition, skip_selector, split_selector, units


class PartitionTest(unittest.TestCase):
    def test_partition_covers_once_balances_counts_and_is_deterministic(self):
        packages = {f"example/pkg{i}" for i in range(47)}
        shards, _ = partition(packages, "example")
        self.assertEqual(SHARDS, len(shards))
        self.assertEqual(packages, set().union(*map(set, shards)))
        self.assertEqual(len(packages), sum(map(len, shards)))
        self.assertEqual([15, 16, 16], sorted(map(len, shards)))
        self.assertEqual(shards, partition(list(reversed(sorted(packages))), "example")[0])

    def test_new_package_is_included(self):
        packages = {"example/miner", "example/mainnet", "example/crv4", "example/new"}
        shards, _ = partition(packages, "example")
        self.assertEqual(packages, set().union(*map(set, shards)))
        self.assertTrue(all(shards))

    def test_split_package_becomes_one_unit_per_split_in_different_shards(self):
        packages = {"example/validator", "example/miner", "example/internal/urnettools", "example/a", "example/b"}
        count = test_census.SPLITS["validator"][0]
        self.assertEqual(count + 4, len(units(packages, "example")))
        shards, _ = partition(packages, "example")
        placed = [unit for shard in shards for unit in shard]
        self.assertEqual(sorted(units(packages, "example")), sorted(placed))
        homes = [i for i, shard in enumerate(shards) for unit in shard if unit.startswith("example/validator ")]
        self.assertEqual(count, len(set(homes)))
        self.assertNotIn("example/validator", placed)


class SelectorTest(unittest.TestCase):
    def test_splits_cover_every_test_once(self):
        names = [f"TestCase{i}" for i in range(200)] + ["ExampleThing", "ok  \texample/pkg\t0.1s"]
        chosen = []
        for index in (1, 2, 3):
            selector = split_selector(names, index, 3, "example/pkg")
            self.assertTrue(selector.startswith("^(") and selector.endswith(")$"))
            chosen += selector[2:-2].split("|")
        self.assertEqual(sorted(names[:-1]), sorted(chosen))
        for name in names[:-1]:
            self.assertIn(name, split_selector(names, zlib.crc32(name.encode()) % 3 + 1, 3, "example/pkg"))

    def test_split_rejects_odd_names_empty_and_oversized_selectors(self):
        with self.assertRaises(SystemExit):
            split_selector(["Test-bad"], 1, 1, "example/pkg")
        with self.assertRaises(SystemExit):
            split_selector([], 1, 2, "example/pkg")
        with self.assertRaises(SystemExit):
            split_selector([f"Test{'x' * 100}{i}" for i in range(2000)], 1, 1, "example/pkg")

    def test_skip_selector_names_only_the_shards_quarantined_tests(self):
        saved = test_census.QUARANTINE
        try:
            test_census.QUARANTINE = {"p": {"TestB": "why", "TestA": "why"}, "q": {"TestC": "why"}}
            self.assertEqual("^(TestA|TestB)$", skip_selector({"example/p", "example/r"}, "example"))
            self.assertEqual("", skip_selector({"example/r"}, "example"))
        finally:
            test_census.QUARANTINE = saved


if __name__ == "__main__":
    unittest.main()
