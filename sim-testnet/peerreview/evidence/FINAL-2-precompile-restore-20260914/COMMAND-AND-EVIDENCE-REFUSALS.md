# Retained command and evidence refusals

These records are deliberately retained and are not relabeled as product failures or qualification passes.

1. Source-only census preflight emitted filename-prefixed test names, so the anchored selector selected zero bare roots. It was corrected with `--no-filename` before compilation.
2. List admission incorrectly required empty stderr even though both binaries emitted retained initialization logging. Both lists themselves exited 0 and contained the exact 44 compiled roots; no list was rerun.
3. The first normal/race body pair ran from repository root instead of the package directory. It exited 1 in each mode because `../deploy/testnet/policy-v1.yml` resolved outside the checkout. The retained wrong-CWD failure set is 24 parents plus 4 explicit descendants in each mode.
4. The corrected-CWD package-only pair exited 0 but omitted `-test.v`, yielding no per-root `test2json` events. It is retained but uncredited.
5. A confirmation preflight initially treated four descendant event identities as top-level selector roots. It was corrected without starting a body: the final selector has 24 parents and declares all 28 inherited events.

Every correction reused the same compiled normal/race binaries and unchanged source/dependency snapshot where applicable.
