# Static finding and correction

The original d714 static gate found one Medium `uninitialized-local` finding: `STSubnetProbe.readBattery(...).selfMapped` at `src/probe/STSubnetProbe.sol:157`. That original candidate failure is copied in `results/d714-static-medium.RESULT` and is never relabeled as a pass.

`bac5748` initializes the caught-local to `false`. Three fresh corrected probe scans each returned zero High/Medium findings. The exact one-line source patch is `patches/static-initialization.patch`.
