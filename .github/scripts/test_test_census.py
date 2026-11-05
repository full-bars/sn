#!/usr/bin/env python3
import unittest

from test_census import SHARDS, partition


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


if __name__ == "__main__":
    unittest.main()
