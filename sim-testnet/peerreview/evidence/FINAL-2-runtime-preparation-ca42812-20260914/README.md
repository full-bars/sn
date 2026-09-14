# Report 2: owned-node runtime preparation, 14 September 2026

This bundle records the completed native build, LAN-only preflight and setup
review for SN `ca4281201077b6e3e9cde1004568efa219d0e12c`. It proves preparation
only: the gates were still running when this bundle was assembled, the approved
repair had not executed, the soak was stopped and `final_acceptance=false`.

The final executable has SHA-256
`b440fcaf46821278950cefc85bbcba751397cbd1870380e002b05109f1b2b5d4`.
Its actual build and outer exits are zero. The recorded source observations
before and after the build match across all 13 repositories. The original
receipt's `RESULT-BOOTSTRAP-CLI-BUILD.json` filename and `kind` were reused by
the capture recipe; the revision and executable recorded inside are the final
`ca42812` build. The separate earlier `4859378` bootstrap is not this artifact.
The release-lock file SHA-256 is
`cc6a275dee1a669c7f1f2861715c230609dd45772730326a9cd1e99931dce8ef`.
The source binding records its own earlier observation time; its then-false
`full_gates_started` field is preserved, not rewritten as current status.

Native `doctor` finished at 00:28:41 UTC with exit 0 and `ready=true`.
Exactly 62 of 64 checks have `ok=true`; all hard checks pass. The two soft
results disclose that configured aliases use the same physical node, so the
run cannot claim independent RPC backends. Every actual testnet request used
`192.168.1.162:9944` without RPC pacing, as instructed by the user. New evidence
has `independent_rpc=false`. Original public-node evidence is unchanged.

One native setup preview finished at 00:31:58 UTC with exit 0. Root compared
it with adopted plan
`0xdbeb584008bbdbc6607a49a5118c1c82fdfa18a15ca8fe5b5c8cea9d2775c37a`.
All 2,309 actions and the spending envelope are identical. The only changes
are the release-lock/resolved-input hashes, appended prior-plan identity,
new plan hash and fresh observation/generation fields. Plan revision
`0xae15ecdd37cac2a223533b4a1b3d9fa6431da33d78cfd9ccac006a98e9d8f414`
retains the already approved 6,000-alpha repair, 37,250-alpha lifetime ceiling,
180 EVM within 200 total TAO, 262 registrations and zero new subnets.
Maximum plus superseded liabilities total 37,250 alpha, 180 EVM,
185.748236 TAO and 262 registrations. The source minimum remains 2,000 alpha.
The approved repair's minimum finalized credit is 5,999,999,999,999 alpha-rao.
`APPLY-PREPARED.json` is the reviewed invocation, not an execution receipt.

The full private setup preview remains at
`/home/by/urnetwork/temp/sn-full-finalization-20260912/native-owned-ca42812-20260914T0027Z/setup/stdout.json`,
SHA-256 `ac2e33f432eea203d57a63c7d1c7773480774603f4d9dab90dfe143312d0b465`.
The plan's retained allocation census is not a fresh reserve census. Native
pre-sign checks, finalized debit/credit and the complete post-repair reserve
census remain required. Both read-only commands preserved the plan, journal
and both supervisor files and made zero chain transactions.

Raw receipts and emitted build-info bytes are copied unchanged. The executable,
caches, vault and private plan are omitted. Original paths identify the source
captures. The relative `SHA256SUMS` checks this portable bundle's bytes:

```sh
sha256sum -c SHA256SUMS
```
