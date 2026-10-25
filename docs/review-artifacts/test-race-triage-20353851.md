# Full test and race run, triage at main 20353851

Scope: `go build ./...`, `go vet ./...` and `go test ./... -race -count=1 -json -timeout 60m`
at commit 20353851 (tip of main, 2026-10-25), followed by three re-runs of every failing
or slow (>30 s) test, a no-race run of every failure, and a comparison at the previous
merge. Nothing was fixed; this file is the only change on the branch.

## Environment and caveats

- Linux 6.18 amd64, go1.27.3, 4 cores shared with five other agents doing builds and test
  runs at the same time. **All timings are inflated by contention**, often by 3x to 30x
  (for example `TestProviderVersionFallbackToExec` took 239 s in the full run and 8 to 10 s
  alone; `TestValidatorStakeCapabilityRejectsApiDrift` 54 s versus 8 s).
- The tests ran as **root** (uid 0). Two failures depend on that.
- **No docker daemon** (`/var/run/docker.sock` missing). No test failed because of it.
- **No sibling `server` checkout** (`/home/user/server`, that is `../server` relative to the
  repo root). The upstream server repository is not available to this session.
- Global git config here signs every commit (`commit.gpgsign=true`, `gpg.format=ssh`).
- Default `TMPDIR` is `/tmp` (mode 1777).

## Commands and results

| Command (in /home/user/wt-b at 20353851) | Result |
|---|---|
| `go build ./...` | exit 0 (3m28s wall) |
| `go vet ./...` | exit 0, no findings (1m19s wall) |
| `go test ./... -race -count=1 -json -timeout 60m` | exit 1, 11:23:15 to 12:50:27 UTC (87 min wall) |

Full-run totals from the JSON: 57 packages: 34 ok, 14 with no test files, 9 failed.
Top-level and subtests: 8731 pass, 33 fail, 26 skip. 8 `WARNING: DATA RACE` blocks, in 3 tests.

Two of the nine failing packages, `mainnet` and `validator`, did not fail on a test
assertion first: their test binaries **hit the 60 minute `-timeout`** (`panic: test timed
out after 1h0m0s`, running `TestBootstrapContractRoleReviewWithoutDurableDeclaration` and
`TestReleaseSourceRoleMirrorV2IncidentLoopSteersAndFinalizes` respectively, each only
seconds into its run). See "Not run" below.

## Failures and flakes

"Prev merge" = the same test at 65910c22 (merge of pull request 230). The newest merge,
9f913750, differs from 20353851 only in `docs/Bittensor-Head-Tier.md`, so it was not a
useful comparison; 65910c22 is the merge before it. Every test was run there with
`go test -race -run '^Name$' -count=1` in a temporary worktree (since removed).

| Package | Test | First failing assertion | Race re-runs (x3) | Without -race | Prev merge 65910c22 | Judgement |
|---|---|---|---|---|---|---|
| crv4 | TestValidatorStakeCapabilityRejectsApiDrift | `validator_stake_runtime_test.go:185: API [["0x8375104b299b74c5",3]] escaped: {...SpecVersion:484...}, <nil>` | 3/3 fail | fails | fails | **Test defect (stale).** `crv4/runtime_profile.go:326-332` admits SubnetInfoRuntimeApi version 3 for node-subtensor 481 to 484 (484 added in 6e686aab). The fixture runs spec 484, so API 3 is now legitimately accepted; the test still lists API 3 as drift. |
| validator | TestProductionRuntime481RetainsAuthorityAndInterfaceRefusals/future-api3-spec | `production_runtime_successor481_test.go:129: runtime481 boundary accepted future-api3-spec` | 3/3 fail | fails | fails | **Test defect (stale).** The case sets `specVersion` 482 (line 80) as the "future" API3 spec, but 482 has been admitted since f55c6070 (`crv4/runtime_profile.go:330`). |
| internal/urnettools | TestOpenRCLifecycleStillGuardedWithoutSudo | `openrc_elevation_test.go:159: with no usable sudo the teardown must print the manual commands, got:` (empty) | 3/3 fail | fails | fails | **Environment (root), test defect.** `scripts/Provider_Install_Linux.sh:1317` takes the privileged branch when `id -u` is 0, whatever `SUDO` is. Passes as uid 65534 (checked with `setpriv --reuid=65534`). Side effect worth knowing: run as root, the test really executes `rm -f /etc/init.d/urnetwork` and the `/etc/periodic/*/urnetwork-update*` removals on the host. |
| internal/urnettools | TestProxyDelegation | `proxy_delegation_test.go:61: cmdProxy(summary): provider test-provider.service: resolve user "test-user": user: unknown user test-user` | 3/3 fail | fails | fails | **Environment (root), test defect.** As root, `dropPrivilegesTo` (`internal/urnettools/drop_privileges_unix.go:25-35`) looks up the provider's user and refuses on failure, by design. The fixture user does not exist. Passes as uid 65534. |
| miner | TestIncidentCaptureAbandonsAStuckWriter | `testing.go:1867: race detected during execution of test` (2 race blocks) | 3/3 fail (race) | passes | fails (race) | **Test defect (data race).** The test makes the writer goroutine get abandoned, then `t.Cleanup` restores package variables while that goroutine is still reading them. See races R1 and R2. |
| miner | TestIncidentCaptureDoesNotWaitOnAStuckNote | `testing.go:1867: race detected during execution of test` (5 race blocks) | 3/3 fail (race) | passes | fails (race) | **Test defect (data race)**, same pattern as above. See R1. |
| miner | TestLegacyImportUnreachableApiStopsHammering | `testing.go:1867: race detected during execution of test` (1 race block) | 0/3 fail; with `-count=30`, 2 of 2 batches failed (1 race block each) | passes | race reproduced (1 in `-count=30`) | **Intermittent data race, mostly a test defect, though the code also leaves goroutines running.** The proof workers spawned at `miner/legacy_import_proof.go:366` are not joined when the collector stops early, so they outlive `runLegacyProviderImport`. The test's deferred restore of `legacyImportTransientBackoff` (`legacy_import_observability_test.go:377`) then races with a worker reading it at `legacy_import_proof.go:292`. See R3. |
| miner | TestProfilingServesEndpoints | `profile_listen_test.go:176: read /debug/pprof/allocs?debug=1: context deadline exceeded` | 0/3 fail (3.4 s each) | passes | passes (1 run) | **Flake caused by load.** Client timeout reading a pprof allocs dump under heavy CPU contention (12.85 s in the full run). |
| miner | TestResourceConfigWarningsCostDoesNotGrowWithThePool | full run: `resource_config_warn_test.go:382: 10 allocations per check; the startup check must stay cheap`; re-runs: `:379: warnings allocate per proxy: 9 allocations at 1 proxy, 14 at 4000` | 3/3 fail | passes | fails | **Test defect, fails only under -race.** It uses `testing.AllocsPerRun` with fixed limits (`<= 8`, growth `<= 4`) and has no race-build skip; race instrumentation adds allocations. |
| scripts/mainnet-release-build | TestReleaseBuildLocalModeRefusesDirtyMovedOrLinkedSibling | `dependency_mode_test.go:198: .../git [commit --quiet -m synthetic sibling]: exit status 128: error: could not create temporary file: No such file or directory` | 3/3 fail | fails | fails | **Environment (git signing) and test defect.** `buildEnvironment` sets `TMPDIR=<output>/tmp` (`scripts/mainnet-release-build/main.go:170`). The test never creates that directory, and the inherited global `commit.gpgsign=true` with ssh signing needs a temporary file there. Passes with `GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=commit.gpgsign GIT_CONFIG_VALUE_0=false`. |
| scripts/source-graph | TestClientauthSourceGraphSelectsSdkSiblingOrPin | `source_graph_test.go:73: clientauth SDK resolved outside its reviewed pin: replacement=&{Path:github.com/full-bars/sdk ...}` | 3/3 fail | fails | fails | **Test defect (stale against go.mod).** `go.mod:226` replaces `github.com/urnetwork/sdk` with `github.com/full-bars/sdk`; the test expects `github.com/urnetwork/sdk` at a 2026-10-01 pin. |
| scripts/source-graph | TestClientauthSourceGraphSelectsConnectSiblingOrPin | `source_graph_test.go:79: clientauth Connect resolved outside its reviewed pin: replacement=&{Path:github.com/full-bars/connect ...}` | 3/3 fail | fails | fails | **Test defect (stale).** `go.mod:241` replaces connect with `github.com/full-bars/connect`. |
| scripts/source-graph | TestClientauthSourceGraphSelectsSctpForkSiblingOrPin | `source_graph_test.go:85: clientauth SCTP is neither a sibling checkout nor pinned: {Path:github.com/pion/sctp Version:v1.11.1 ...}` | 3/3 fail | fails | fails | **Test defect (stale).** go.mod has no sctp replace (`go.mod:146`, plain v1.11.1). |
| scripts/source-graph | TestSourceGraphSelectsGvisorForkSiblingOrPin | `source_graph_test.go:91: gVisor is neither a sibling checkout nor pinned: {Path:gvisor.dev/gvisor ...}` | 3/3 fail | fails | fails | **Test defect (stale).** go.mod has no gvisor fork replace (`go.mod:203`). |
| scripts/server-fixture | 15 tests `TestServerFixtureSuite*` (CliRequiresExplicitModeAndCompleteAuthorities, HeliusWebhookVerifiesSyntheticSecret, LocalTunIpv4/Ipv6ResolvesWithoutDatabases, LoopbackResolvesWithoutDatabases, PlacesHaveSyntheticSchemaAndValues, ProConfigIsSyntheticAndParseable, RejectsAliasedOrExcessiveSourceBeforeMutation, RejectsManifestDriftBeforeMutation, RequiresDistinctCanonicalLoopbackAuthorities, SatisfiesActualResourceCensus, SelectsExactGeographyProfile, UsesDocumentationIpOverrideWithoutMmdb, UsesSyntheticCertificatesForRequiredAliases, X402ConfigIsInertAndParseable) | `suite_test.go:68` (and others): `lstat /home/user/server: no such file or directory` | 3/3 fail | fails | fails | **Environment (no sibling server checkout).** `suite_test.go:38` resolves `../../../server`. Could not be run meaningfully here. |
| sim-testnet | TestAdversarialMatrixReferencesOnlyCheckedInTests | `adversary_test.go:485: lstat /home/user/server: no such file or directory` | 3/3 fail | fails | fails | **Environment (no sibling server checkout).** |
| sim-testnet | TestRepositoryDiscoveryUsesModuleIdentity | `config_test.go:613: cannot discover server repository; use --server-repo` | 3/3 fail | fails | fails | **Environment (no sibling server checkout).** |
| validator | TestProductionBootstrapCommittedProtectedStatePath | `production_bootstrap_committed_test.go:212: protected physical control committed prefix state has an unprotected or differently owned ancestor` | 3/3 fail | fails | fails | **Environment (TMPDIR under a world-writable /tmp).** The test comment (line 206-207) says qualification puts TMPDIR under a protected parent. Passes with `TMPDIR=/root/b-ptmp` (mode 0700, every ancestor root-owned, not world-writable). Fails with TMPDIR under `/tmp/...` even when the leaf is 0700. |
| mainnet | (package) | `panic: test timed out after 1h0m0s` | not re-run as a package | not run | not run | **Environment/suite size.** Under -race on this shared box the package needs far more than 60 min; see "Not run". |
| validator | (package) | `panic: test timed out after 1h0m0s` | not re-run as a package | not run | not run | Same as mainnet. |

Summary: no failure is new since the previous merge, and none is a confirmed product-code
regression. These fail on every run:
- the 3 stale-test failures (two runtime API3 tests, four source-graph tests),
- the 2 incident-capture data races,
- the race-only allocation test.

These depend on the environment:
- running as root (2),
- the missing server sibling (17),
- the signing git config (1),
- TMPDIR under `/tmp` (1).

These are flaky:
- TestProfilingServesEndpoints (load),
- the intermittent legacy-import race.

## Slow tests (>30 s) re-run three times

All slow tests passed in every re-run (no failures, no race reports). Package-level loops
ran in parallel, so times are still inflated. Times in seconds; "full" is the full-run time.

| Package | Test | Full | Re-run 1 | Re-run 2 | Re-run 3 |
|---|---|---|---|---|---|
| mainnet | TestBootstrapContractInstallationPublicReadbackRecoversExactAnchor | 305.1 | 294.6 | 297.1 | 298.1 |
| mainnet | TestBootstrapContractReceiptAdmissionCommandRetainsHistory | 239.6 | 241.6 | 238.1 | 241.2 |
| internal/urnettools | TestProviderVersionFallbackToExec | 239.2 | 10.0 | 8.8 | 8.0 |
| mainnet | TestBootstrapContractInstallationLostBorrowedMarkerRefusesReadback | 217.0 | 209.9 | 214.5 | 223.9 |
| internal/urnettools | TestProviderVersionBuildinfoPreferred | 195.1 | 8.3 | 9.6 | 6.1 |
| mainnet | TestBootstrapContractInstallationRejectsHiddenAndChangedStorage | 192.2 | 187.6 | 186.0 | 192.8 |
| mainnet | TestBootstrapContractCurrentCommandChecksPinnedBootstrapState | 180.3 | 186.2 | 177.1 | 185.2 |
| mainnet | TestBootstrapContractReadinessReusesAuthenticatedPrefixWithoutReplay | 158.4 | 166.7 | 84.5 | 84.9 |
| mainnet | TestBootstrapContractSuccessorCommandAdoptsCompleteV3Custody | 156.8 | 149.0 | 149.7 | not run |
| mainnet | TestBootstrapContractReceiptLostBorrowedMarkerRefusesReadback | 155.9 | 156.2 | 154.1 | 153.6 |
| crv4 | TestRuntimeSuccessionRefusesNonSuccessorsAndChangedInterfaces | 118.8 | 44.6 | 27.4 | 31.1 |
| validator | TestProductionAuthenticationRunReleaseInvalidApiKeepsObservation | 117.9 | 185.7 | 110.8 | 110.6 |
| crv4 | TestSourceRuntimeCapabilityRejectsConsumedChanges | 102.3 | 42.5 | 37.9 | 30.7 |
| mainnet | TestBootstrapChainReadinessMissingStateNeverRepairsCustody | 101.6 | 91.5 | 86.5 | 89.6 |
| mainnet | TestBootstrapChainNoTrimRefusesRemovalsAndUnprovenScope | 101.0 | 142.0 | 66.6 | 64.3 |
| mainnet | TestBootstrapContractSuccessorAdoptsCompletedCustodyAndIncrementalBudgets | 93.2 | 92.5 | 91.4 | not run |
| mainnet | TestBootstrapContractInstallationCurrentPolicyKeepsOriginalReceiptClock | 88.1 | 85.8 | 87.8 | 85.8 |
| mainnet | TestBootstrapContractSuccessorRetainsUnexecutedOriginalReservation | 87.3 | 84.4 | 86.2 | not run |
| validator | TestProductionStartupActivationJoinsIndependentReadCauses | 85.0 | 85.5 | 85.2 | 84.8 |
| mainnet | TestBootstrapContractSuccessorRequiresCompleteOriginalCustody | 82.9 | 78.1 | 78.2 | not run |
| validator | TestAdjacentHeaderProductionApplicationPreservesJournalOnCanonicalSwitch | 81.3 | 126.8 | 43.8 | 43.3 |
| crv4 | TestSourceRuntimeCapabilityRetainsFutureSignedBytes | 76.2 | 19.3 | 21.9 | 27.7 |
| crv4 | TestValidatorProducerRuntimeRejectsConsumedChanges | 74.6 | 33.0 | 29.4 | 29.8 |
| mainnet | TestBootstrapChainRootRejectsMissingOrMismatchedRoleInputs | 72.4 | 68.6 | 62.3 | 64.9 |
| sim-testnet | TestScenarioCampaignRecoveryLargeJournalSinglePhaseEntry | 71.4 | 122.4 | 94.8 | 53.0 |
| validator | TestReleaseArchiveV2RejectsMissingReplicaAndOriginalPinMutation | 68.9 | 66.2 | 63.6 | 70.6 |
| validator | TestReleaseArchiveFullLineageCountsNonemptyHeadAndRefusesDoubleFold | 68.6 | 69.9 | 68.8 | 66.9 |
| validator | TestReleaseArchiveFullLineageOwnedReadsDoNotReplayHistory | 67.1 | 73.2 | 67.5 | 71.0 |
| mainnet | TestBootstrapChainMissingCompletedStateNeverRecreates | 66.8 | 61.4 | 53.4 | 55.2 |
| miner | TestFleetEvmRuntimeWebsocketImplicitHeadRetainsOriginalBlock | 66.0 | 64.7 | 63.2 | 63.3 |
| validator | TestReleaseArchiveFullLineageBlockedReplayAllowsOwnedForegroundRead | 65.7 | 64.2 | 65.5 | 65.1 |
| validator | TestReleaseArchiveFullLineageCancellationRetainsFreshReplay | 64.3 | 63.9 | 63.5 | 63.0 |
| crv4 | TestValidatorStakeCapabilityRejectsConsumedStorageChanges | 63.7 | 49.9 | 38.0 | 37.2 |
| miner | TestFleetEvmRuntimeActualWebsocketLateReadRepeatsOriginalView | 63.6 | 66.1 | 65.0 | 3.3 |
| sim-testnet | TestScenarioCampaignRecoveryLargeJournalCache | 63.1 | 75.9 | 89.8 | 42.8 |
| validator | TestProductionStartupRunReleaseReconcilesAcrossCompatibleUpgrade | 63.0 | 63.3 | 60.1 | 59.0 |
| validator | TestReleaseArchiveFullLineageTerminalOwnerCancellation | 61.1 | 60.9 | 59.8 | 59.4 |
| validator | TestProductionAuthenticationRunReleaseRefreshTimeoutKeepsObservation | 59.8 | 60.5 | 58.4 | 57.6 |
| validator | TestReleaseForegroundProgressDuringBlockedHttpAndReplay | 59.7 | 58.3 | 59.0 | 59.0 |
| miner | TestClaimProgressCapacityCannotBlockSignedOutcome | 59.0 | 27.3 | 22.2 | 10.8 |
| validator | TestReleaseArchiveFullLineageTerminalRefusesLostOwner | 58.9 | 61.3 | 59.5 | 59.5 |
| validator | TestProductionStartupRunReleaseReconcilesBeforeCurrentPreparation | 58.4 | 61.7 | 58.5 | 58.2 |
| validator | TestProductionAuthenticationRunReleaseObservesRetainedWithoutClient | 57.4 | 61.5 | 57.6 | 59.4 |
| crv4 | TestProvisionalRuntimeProofConcurrentEvictionPreservesBoundView | 56.7 | 24.5 | 16.2 | 19.2 |
| validator | TestProductionAuthenticationRunReleaseRevocationKeepsObservation | 56.5 | 57.2 | 56.3 | 57.5 |
| validator | TestReleaseArchiveFullLineageLateObservationPublishesNoCache | 53.5 | 51.9 | 52.2 | 54.6 |
| validator | TestReleaseArchiveFullLineageRejectsLateEnvelopeWithoutPartialOwner | 52.3 | 51.7 | 50.4 | 52.0 |
| miner | TestClaimProgressPriorOptionalMetadataYieldsToCustodyGrowth | 51.5 | 38.1 | 23.9 | 13.3 |
| validator | TestReleaseArchiveFullLineageRejectsRemovedPredecessor | 51.0 | 53.4 | 51.4 | 49.9 |
| mainnet | TestBootstrapChainSoleSchemaBoundaries | 49.5 | 35.8 | 34.0 | 33.9 |
| crv4 | TestValidatorStakeCapabilityRejectsMixedBlockIdentity | 48.5 | 17.5 | 14.1 | 17.9 |
| mainnet | TestBootstrapContractReadinessLeavesMissingOriginalCustodyUnresolved | 46.9 | 30.1 | 8.7 | 9.0 |
| mainnet | TestBootstrapChainStaleInputsRefuseBeforeMutation | 46.7 | 46.6 | 46.9 | 45.6 |
| validator | TestReleaseArchiveV2RejectsSelfDeclaredEMAAndChangedSourceBytes | 46.5 | 51.6 | 49.0 | 48.9 |
| crv4 | TestValidatorStakeCapabilityKeepsExactArtifactAuthority | 45.5 | 18.9 | 14.9 | 18.2 |
| miner | TestClaimV2BatchHoldsAtMostTheCap | 44.7 | 62.0 | 59.7 | 30.5 |
| miner/onchain | TestV2CheckDemandDoubling | 43.2 | 75.5 | 65.9 | 47.8 |
| validator | TestProductionStartupRefusesAMissingIntentSegment | 41.3 | 70.5 | 37.4 | 36.2 |
| mainnet | TestBootstrapChainPendingConfigRefusals | 41.0 | 39.1 | 37.2 | 36.7 |
| mainnet | TestBootstrapChainRejectsConflictingRolesAndChildApprovals | 38.3 | 38.8 | 38.9 | 37.4 |
| validator | TestProductionContinuationEndsPendingBytesReplacedByCompatibleUpgrade | 38.3 | 38.5 | 39.3 | 37.2 |
| crv4 | TestSourceRuntimeCapabilityPreservesOriginalSigningDomain | 38.2 | 14.9 | 11.9 | 11.8 |
| mainnet | TestBootstrapChainReadinessReportsActivityPermitAndSubnetConflict | 37.7 | 21.2 | 19.2 | 20.5 |
| merkle | TestLedgerUnfaultedAccountsStayProvable | 37.5 | 19.9 | 23.3 | 25.3 |
| mainnet | TestBootstrapContractReadinessRetainsOriginalSignedCustody | 36.9 | 23.0 | 7.2 | 6.9 |
| mainnet | TestBootstrapChainNoTrimContractRolePlan | 35.9 | 27.9 | 13.1 | 12.8 |
| validator | TestIntentV2VerifiedHistorySkipsRepeatedReplay | 34.9 | 38.3 | 24.6 | 24.2 |
| crv4 | TestTempoDriftProfileGuardsFreshProductionPreparation | 34.4 | 22.6 | 16.9 | 18.6 |
| mainnet | TestBootstrapChainValidatorRejectsSignedScopeMismatch | 34.3 | 37.5 | 35.0 | 35.5 |
| crv4 | TestProvisionalRuntimeCompatibilityRejectsConsumedChanges | 33.1 | 16.8 | 8.3 | 8.7 |
| validator | TestProductionContinuationRecoversOriginalAcrossEpochAndRestart | 31.8 | 66.9 | 32.4 | 30.5 |
| crv4 | TestValidatorStakeCapabilityScheduleKeepsItsOwnPurpose | 31.4 | 13.2 | 9.6 | 13.8 |
| mainnet | TestBootstrapChainRootRestartRequiresOriginalApprovalSource | 30.6 | 29.8 | 27.6 | 27.6 |
| mainnet | TestBootstrapChainReadinessRejectsStaleRoleGenerations | 30.6 | 37.8 | 19.8 | 19.7 |

"not run": the 2 hour budget for the re-run loop ran out during mainnet's third pass, so those 4 mainnet tests have two re-runs instead of three.

## Ten slowest tests in the full run

| # | Seconds | Package | Test |
|---|---|---|---|
| 1 | 305.15 | mainnet | TestBootstrapContractInstallationPublicReadbackRecoversExactAnchor |
| 2 | 239.62 | mainnet | TestBootstrapContractReceiptAdmissionCommandRetainsHistory |
| 3 | 239.15 | internal/urnettools | TestProviderVersionFallbackToExec (8.0 to 10.0 s in re-runs) |
| 4 | 217.04 | mainnet | TestBootstrapContractInstallationLostBorrowedMarkerRefusesReadback |
| 5 | 195.09 | internal/urnettools | TestProviderVersionBuildinfoPreferred (6.1 to 9.6 s in re-runs) |
| 6 | 192.20 | mainnet | TestBootstrapContractInstallationRejectsHiddenAndChangedStorage |
| 7 | 180.29 | mainnet | TestBootstrapContractCurrentCommandChecksPinnedBootstrapState |
| 8 | 158.44 | mainnet | TestBootstrapContractReadinessReusesAuthenticatedPrefixWithoutReplay |
| 9 | 156.85 | mainnet | TestBootstrapContractSuccessorCommandAdoptsCompleteV3Custody |
| 10 | 155.92 | mainnet | TestBootstrapContractReceiptLostBorrowedMarkerRefusesReadback |

Slowest packages: mainnet 3601 s (timed out), validator 3600 s (timed out), miner 1296 s,
crv4 866 s, internal/urnettools 552 s, sim-testnet 536 s, miner/onchain 129 s, merkle 67 s.

## Data races

Eight `WARNING: DATA RACE` blocks were reported, all in `miner`, in three tests. They fall
into three distinct patterns. Stacks are trimmed to repository frames. Line numbers are at
20353851.

### R1: incident capture, cleanup restore against the abandoned bundle writer

TestIncidentCaptureDoesNotWaitOnAStuckNote (5 blocks) and TestIncidentCaptureAbandonsAStuckWriter
(1 of its 2 blocks, the `incidentBundleMaxBytes` one) share this shape. The writer goroutine
is the one the code deliberately abandons on timeout (`miner/incident_capture.go:241-258`).
Representative block:

```
Write at 0x0000085b0368 by goroutine 23524:
  miner.incidentTestRestore.func1()        miner/incident_capture_test.go:44
  testing.(*common).Cleanup.func1()

Previous read at 0x0000085b0368 by goroutine 23525:
  miner.(*incidentBundle).write()          miner/incident_capture.go:436   (incidentHookAfterFile)
  miner.writeIncidentBundle()              miner/incident_capture.go:516
  miner.captureIncidentBundleWithinLog.func1()  miner/incident_capture.go:242

Goroutine 23525 created at:
  miner.captureIncidentBundleWithinLog()   miner/incident_capture.go:241
  miner.TestIncidentCaptureDoesNotWaitOnAStuckNote()  miner/incident_capture_test.go:237
```

The other blocks in this test are the same pair of goroutines on other restored variables:
- `incident_capture_test.go:42` against `incident_capture.go:524` and `:873` (via `:533`),
- `incident_capture_test.go:43` against `incident_capture.go:663` (via `:530`) and `:408` (via `:516`).

### R2: incident capture, `incidentLogTailBytes` read by the abandoned writer

```
Write at 0x0000083a5488 by goroutine 23529:
  miner.incidentTestRestore.func1()        miner/incident_capture_test.go:43
  testing.(*common).Cleanup.func1()

Previous read at 0x0000083a5488 by goroutine 23530:
  miner.incidentRamlogTail()               miner/incident_capture.go:682
  miner.writeIncidentBundle()              miner/incident_capture.go:531
  miner.captureIncidentBundleWithinLog.func1()  miner/incident_capture.go:242

Goroutine 23530 created at:
  miner.captureIncidentBundleWithinLog()   miner/incident_capture.go:241
  miner.captureIncidentBundleWithin()      miner/incident_capture.go:212
  miner.captureIncidentBundle()            miner/incident_capture.go:202
  miner.TestIncidentCaptureAbandonsAStuckWriter()  miner/incident_capture_test.go:301
```

The second block in this test: `incident_capture_test.go:42` against `incident_capture.go:387`
(via `:511`).

Cause, R1 and R2: the abandoned writer is intended behavior. The race is between the test
cleanup restoring package-level bounds and a goroutine the test chose not to wait for. This
is a test defect. It also shows that those bounds, being mutable package variables, are not
safe to change while a capture may still be running.

### R3: legacy import, test restore against a non-joined proof worker

```
Write at 0x0000083a54d0 by goroutine 23690:
  miner.TestLegacyImportUnreachableApiStopsHammering.func1()  miner/legacy_import_observability_test.go:377
  (deferred restore of legacyImportTransientBackoff)

Previous read at 0x0000083a54d0 by goroutine 23699:
  miner.legacyImportProveSlot()            miner/legacy_import_proof.go:292   (time.After(legacyImportTransientBackoff))
  miner.legacyImportProveAll.func3()       miner/legacy_import_proof.go:373
  miner.legacyImportProveAll.gowrap2()     miner/legacy_import_proof.go:374

Goroutine 23699 created at:
  miner.legacyImportProveAll()             miner/legacy_import_proof.go:366
  miner.runLegacyProviderImport()          miner/legacy_import.go:518
  miner.TestLegacyImportUnreachableApiStopsHammering()  miner/legacy_import_observability_test.go:386
```

Cause: when the collector stops early (unreachable streak), `legacyImportProveAll` returns
without waiting for its worker goroutines (`legacy_import_proof.go:364-375`; there is no
WaitGroup). A worker can still be inside `legacyImportProveSlot` after
`runLegacyProviderImport` returns. The race needs that timing, so it shows up about once
in 30 runs, at both 20353851 and 65910c22. The direct trigger is the test mutating a
package variable. The underlying characteristic, workers that outlive the call, is in the
code; whether that matters in production is UNVERIFIED.

## Not run, or only partly run

- **mainnet**: 112 top-level tests finished before the 60 min timeout. The package has
  about 2777 `func Test...(t *testing.T)` declarations (grep count; build tags not
  considered), so roughly 2600 tests were **not run** under -race in this session.
- **validator**: 1671 top-level tests finished (1667 pass, 2 fail, 2 skip) of about 2893
  declarations, so roughly 1200 tests were **not run**.
- Neither package was re-run with a longer timeout or without -race: a full -race pass
  would take several hours on this shared machine. A dedicated run with
  `-timeout 6h`, or sharding with `-run`, is needed to cover them.
- Slow-test re-runs: 70 of the 74 slow tests got three re-runs; 4 mainnet tests got two (the loop hit its 2 hour limit). See the slow-test table.
- scripts/server-fixture and the two sim-testnet failures above could not exercise their
  logic without a `../server` checkout.
- No test needed docker or outbound network as far as the output shows. Skipped tests (26)
  were not individually triaged.

## Reproduction notes

- Root-only failures: run as an unprivileged uid, e.g. build with
  `go test -c -o /tmp/urnettools.test ./internal/urnettools`, then from the package dir
  `HOME=/tmp setpriv --reuid=65534 --regid=65534 --clear-groups /tmp/urnettools.test -test.run '^(TestOpenRCLifecycleStillGuardedWithoutSudo|TestProxyDelegation)$'` (both PASS).
- Release-build signing: `GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=commit.gpgsign GIT_CONFIG_VALUE_0=false go test -run '^TestReleaseBuildLocalModeRefusesDirtyMovedOrLinkedSibling$' -count=1 ./scripts/mainnet-release-build` (ok).
- Protected TMPDIR: `mkdir -m 700 /root/b-ptmp && TMPDIR=/root/b-ptmp go test -run '^TestProductionBootstrapCommittedProtectedStatePath$' -count=1 ./validator` (ok).
- Legacy race: `go test -race -run '^TestLegacyImportUnreachableApiStopsHammering$' -count=30 ./miner`.
