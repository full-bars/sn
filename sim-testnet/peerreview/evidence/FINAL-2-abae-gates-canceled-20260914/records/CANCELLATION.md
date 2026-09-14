# Directed cancellation record

At `2026-09-14T10:35:29Z`, the producer inner gate owner `2358565` and the
aggregate inner gate owner `2363570` received `TERM` only after their parent
capture-wrapper identity and command paths were verified. The outer capture
wrappers remained alive long enough to run their existing cleanup and join
paths, then each recorded outer exit `143` at the same second.

The cancellation followed the determination that a production source
correction was required. It therefore supersedes the `abae9a6` source scope;
it does not convert any partial phase success into a gate pass.

`producer-admitted-child-records.log` contains all 13 started/joined records.
`aggregate-admitted-child-records.log` contains all three. The corresponding
outer stdout records show each cancellation exit as `143`; no child joined a
nonzero exit before cancellation. `cleanup-no-live-owner.txt` records the
post-closure process census for the two gate roots and their inner scripts.
