# Rejected linked-worktree artifact

The `r1` compiler exited 0 and produced an artifact, but its outer owner exited 1. The recorded build information contains `-trimpath=true` and no `vcs`, `vcs.revision`, or `vcs.modified` fields. It therefore failed the required authentic VCS-attestation checks and is not a qualified release CLI.

This is distinct from the earlier pre-build admission refusal, which invoked no compiler. The review in `review/CLI-VCS-PRESENTATION-REVIEW.md` identifies the installed Go 1.26.6 `.git`-directory discovery constraint. The canonical standalone build records the same source, lock, toolchain family, and dependency graph with a directory-form Git presentation and passes all strict stamp checks.
