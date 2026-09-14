# e982 preparation and closed-refusal evidence

This external bundle records the closed release-lock preparation, e982 CLI build, reviewed read-only setup preview, and the later `setup --apply` refusal before any state or transaction change.

| Phase | Closed outcome |
| --- | --- |
| Release-lock preview and apply | Both exit 0. Exactly five reviewed lock fields changed; no RPC or transaction occurred during lock preparation. |
| Publication and final CLI | Publication exits are recorded; the final CLI is bound to e982 and its 13-repository source-pair hash. |
| Setup preview | Exit 0; read-only; all six state files unchanged; reviewed plan hash `0xa0f74d…37297a`. |
| Setup apply | Exit 1 before application because the recomputed plan hash was `0xee290d…763f6c`, not the reviewed preview hash. All six state files stayed unchanged, with zero repair journal entries and zero new transactions. |

The native refusal is a refusal, not a successful repair. This bundle does not claim a final source qualification, gate result, native completion, or later correction.

The source-publication closure before the lock update is bound to `55f4030b19ce91b6c70834759bb6dd5dc687b978`. The separate lock-only publication after the reviewed release-lock lifecycle, and the independently closed final CLI build, are bound to `e982b3fbd74f76c8afe79cd8ef7067b19b044238`. These records remain distinct.
