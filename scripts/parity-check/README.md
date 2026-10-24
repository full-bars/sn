# parity-check

Reports provider behaviours that have no miner counterpart.

The miner is a port of the provider, so a behaviour that exists in one tree and
not the other is either an unfinished port or silent drift. Reading 350+ files
by hand does not scale; this tool does the census.

```sh
go run ./scripts/parity-check
```

Exit status is `0` when every gap is allowlisted, `1` when an unlisted gap
exists, and `2` when the tree or its configuration cannot be read. The output is
sorted and contains no paths or timestamps, so it can be diffed in CI.

## What counts as a gap

The tool scans `provider/*.go` and `miner/*.go`, skipping `_test.go` by default.
It compares:

| Level | Provider side | Miner side |
|---|---|---|
| file | `foo.go` | same stem, an explicit mapping, or a set of miner files that jointly cover half of its declarations (every exported one, by default) |
| function | every top-level `func` (no receiver) | exact name, `Provider`→`Miner` substitution, an export change, or an explicit mapping |
| type | every top-level `type` | same rules as functions |

Files are paired before declarations are compared, so a whole missing file is
one finding rather than one finding per helper. Declarations in files that did
cross over are still checked individually: a partial port reports exactly the
names that did not make it.

### One provider file, several miner files

A port does not have to be one-to-one. A single provider file is often split
across several miner files — the run loop, its status server and its warmup
path all came from one `provide.go` — and a provider file can also absorb code
that the miner consolidated elsewhere. Pairing therefore produces a *set* of
miner files, not a single target. Candidates are taken best-first (most
declarations contributed, closest stem, tightest file) until the chosen set
jointly holds at least half of the provider file's declarations; no individual
file has to clear the threshold.

Coverage is scored with the same name variants as the declaration comparison
(`Provider`→`Miner`, and an export or case change). A port that lower-cased its
types (`NodeSnapshot` → `nodeSnapshot`) or swapped the package noun therefore
still counts as shared declarations instead of reading as a file with zero
overlap.

Because only exported declarations are compared by default, a set that holds
*every* exported declaration also pairs the file: the unexported helpers it
omits are not compared, so requiring half of them would report a file whose
comparable surface crossed over completely. With `-all` every declaration
counts, half the file is required, and the surviving helpers are reported
individually.

Declarations are then looked up across the whole miner package, not just the
paired set: moving a function to another file is not drift. The paired set
answers only one question — is this file's behaviour present at all.

Only exported declarations are compared by default. Unexported helpers are
implementation detail and are expected to diverge; pass `-all` to include them.

Miner-only files are listed last and never affect the exit status. They are the
port's own additions, not drift. A miner file that paired with a provider file —
by stem, mapping or declaration overlap — is a counterpart rather than an
addition, so it is not listed even when its name differs, which is the normal
case for a rename or a split. The count therefore means "miner files no provider
file claimed", not "miner files with no provider ancestor": a paired file can
still hold new miner code, and this section does not itemise it.

## Allowlist

`allowlist.txt` is for gaps that are intentional. Every entry needs a one-line
reason; a name without one is rejected rather than silently trusted.

```text
# format: <file|func|type> <name> <reason>
file metrics_stubs.go compile-time bridges for connect symbols removed in v2026
func RunSystemAudit the miner audits only the disk, by design
```

A file entry covers every declaration in that file. Keep entries narrow: prefer
`func` over `file` when only one helper is intentional.

## Mappings

`mappings.txt` is for confirmed renames and split ports that the automatic
matcher cannot see (below the declaration threshold, or a name that changed
beyond the `Provider`/`Miner` substitution). It takes no reason, so the comment
above each entry is the record.

```text
# format: <file|func|type> <provider-name> <miner-name>
file renewal_watcher.go client_jwt_hotrestart.go
type ProxyReloader minerProxyReloader
```

A provider file may appear more than once. Each line adds a target, which is how
a split port is recorded; every target is paired, and the declarations are
compared against the whole miner package.

```text
file provider_auth.go client_jwt_hotrestart.go
file provider_auth.go provider_registration.go
file provider_auth.go provider_callbacks.go
```

A `file` mapping to a provider or miner file that does not exist is a
configuration error (exit `2`), not a silent pass. A `func` or `type` mapping is
not validated: a target that does not exist reappears in section `(b)` as a
declaration gap (exit `1`).

Validation sees the scan, not the disk. A run without `-tests` has no `_test.go`
files in it, so a `file` mapping that names one is rejected with a message
asking for `-tests`; a `func` or `type` mapping to a test-only symbol needs the
same flag, and without it the declaration is reported as a gap. `-all` plays no
part here — unexported declarations are always in the lookup tables, and the
flag only decides which of them are compared.

## Flags

```
-root        repository root (default ".")
-provider    provider package directory, relative to -root (default "provider")
-miner       miner package directory, relative to -root (default "miner")
-allowlist   allowlist path, relative to -root
-mappings    mappings path, relative to -root
-tests       include _test.go files
-all         also compare unexported declarations
-v           print each allowlisted entry and every file pairing
-miner-only  list miner-only files (default true)
```

## Reading the report

Sections `(a)` and `(b)` are the work: files and declarations the provider has
and the miner does not. Section `(c)` is informative. `matched by declaration
overlap` is the count of renamed or consolidated files the matcher recognised;
`paired with more than one miner file` is the subset that needed several files
to reach the threshold. If either number moves unexpectedly, a port probably
reorganised files again. `-v` prints the pairing itself, so a surprising file
can be checked against its miner counterparts without guessing.
