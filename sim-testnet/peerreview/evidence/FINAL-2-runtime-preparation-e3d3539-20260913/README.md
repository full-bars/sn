# Report 2: final runtime preparation, 13 September 2026

This bundle retains completed preparation on SN commit
`e3d3539dd761f3e04d2c28e281b01735fa297f9c`. It does not establish a passed full
gate, an executed reserve repair, a running soak, or final acceptance.

- `build/` records the final native executable build: actual build/outer exit 0,
  unchanged observations of all 13 repositories, and binary SHA-256
  `c363685779f723b99befa4b143764769f26848781f4da825985898e8073c6c0b`.
- `doctor/` records the native read-only check: exit 0, 64 of 64 checks passed,
  with LAN and public observers agreeing at finalized block 7,998,850. Its
  retained approved-state budget row describes the still-installed older plan;
  it is not proof that the replacement plan has been applied.
- `setup/` records successful native read-only reconstruction and the reviewed
  delta: replace unsubmitted `alpha.repair.validator.1.6` (3,750 alpha) with
  `alpha.repair.validator.1.7` (6,000 alpha), retaining 31,250 alpha already
  charged and the 37,250 lifetime limit. The only other action change is the
  reserve-majority read dependency and intent. All 2,307 other actions remain
  identical. The source minimum remains 2,000 alpha, with 180 EVM within 200
  total TAO, 262 registrations and zero new subnets.
- `STATE-BEFORE.json` and the native results show that these read-only commands
  preserved the existing plan, journal and supervisor files.

The replacement plan hash is
`0xdbeb584008bbdbc6607a49a5118c1c82fdfa18a15ca8fe5b5c8cea9d2775c37a`.
Its full private native output remains at
`/home/by/urnetwork/temp/sn-full-finalization-20260912/native-readonly-e3d3539-20260913T1940Z/setup/stdout.json`,
with SHA-256
`ba9a19819c2b77f37bc5d31f425257afe50555c17523c88f6cf11b5e316a62d4`.
Plan `live_facts` retains the original sizing census for deterministic plan
reconstruction; those fields are not a fresh live reserve census. Native
pre-sign checks and finalized post-repair credit and reserve evidence remain
required. No chain transaction was submitted by the commands in this bundle.

The original native build-info bytes, including emitted trailing tabs, are
preserved. Paths in the raw receipts identify original captures. The relative
`SHA256SUMS` file checks the integrity of files within this portable bundle:

```sh
sha256sum -c SHA256SUMS
```
