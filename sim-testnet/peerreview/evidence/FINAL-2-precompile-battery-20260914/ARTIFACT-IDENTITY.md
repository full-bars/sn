# Corrected artifact and executable comparison

Comparison report: `bac57484` against `afd7b26`; `protected_executables_exact=true`, with zero protected mismatches. `storage entries` compares decoded entries; serialized layout-reference hashes can differ when compiler graph IDs differ without changing entries or executable bytes.

| Contract | ABI | Constructor | Storage entries | Immutable groups | Creation | Runtime |
| --- | --- | --- | --- | --- | --- | --- |
| coordinator | exact | exact | equal | equal | exact (24778) | exact (24564) |
| vault | exact | exact | equal | equal | exact (10366) | exact (9797) |
| reserve | exact | exact | equal | equal | exact (1884) | exact (1558) |
| fleet_batcher | exact | exact | equal | equal | exact (4293) | exact (4003) |
| validator_evidence | exact | exact | equal | equal | exact (12924) | exact (12192) |

Probe ABI, constructor, and decoded storage entries are equal. Its intended payload changed: creation `7508 -> 7005` bytes; runtime `7265 -> 6755` bytes. Creation SHA-256 `c8600b93605d7ab75aaa361df22508e6c4fa7308ea39e0e628dacd36e83dcf7a -> 2048b3b2230a3d9c2b62a6b97b7d866f493b7300e782d22e114fc67146b8a7e3`; runtime SHA-256 `778d8943daed65d13a194d284cf7fb3c4f8c909ccf236974c545d8b2524e19d3 -> 3146f9516dc4571c481b049a7dcc06bbe5bcddf79847cac7ce70a2b8535fe3e3`.

`STSubnet` intentionally changes with the custody library and is not a protected active release executable identity.
