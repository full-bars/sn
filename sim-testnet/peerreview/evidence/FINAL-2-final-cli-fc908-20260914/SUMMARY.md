# Final CLI presentation summary

The canonical standalone build passed: compiler exit 0, outer exit 0, authentic `vcs=git`, `vcs.revision=fc9086852125227d4e497f4d54b6590a527032ba`, `vcs.modified=false`, and `-trimpath=true`. Its pre/post 13-repository pair is byte-identical and its reviewed release-lock digest is unchanged.

The earlier linked-worktree `r1` compiler also exited 0, but the outer owner correctly rejected its unstamped artifact with exit 1. That artifact is retained only by hash and is not a qualified release CLI. The earlier path-only pre-build admission refusal invoked no compiler.

This is build-presentation evidence only. It does not claim complete producer or aggregate gate results, a deployment, a native operation, or a soak pass. Complete gates were still running when this bundle was requested; stopped soak work is not represented as a pass.
