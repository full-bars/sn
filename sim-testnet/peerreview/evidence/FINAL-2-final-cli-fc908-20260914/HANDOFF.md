# Handoff

The canonical CLI is the artifact identified in `canonical/ARTIFACT-IDENTITY.tsv`, produced from the standalone physical workspace after the adoption records in `publication/`. The preceding linked-worktree binary must remain rejected because it lacks the required VCS fields even though its compiler exit was zero.

Publication-ready paragraph:

> The final simulator CLI was rebuilt from standalone source `fc9086852125227d4e497f4d54b6590a527032ba` with the reviewed lock and an unchanged 13-repository fence. The build passed with an authentic Git VCS stamp and `-trimpath`; a prior linked-worktree artifact that compiled successfully was retained but rejected because Go could not attest its VCS identity. This CLI result is separate from the remaining full-gate and soak outcomes.
