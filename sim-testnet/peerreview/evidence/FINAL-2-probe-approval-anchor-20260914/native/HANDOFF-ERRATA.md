# Native timestamp erratum

The original Astra handoff is retained by SHA-256 reference only: `1b6c45c2992b0167e0f860e32666e88e265ccb4efde5d6649a5435396deb6b57`.

For the original failed native apply, the authoritative closed `RESULT.json` records a start at `2026-09-14T14:35:48.496499604Z` and finish at `2026-09-14T14:37:49.704584485Z`, with exit 1 before application. This bundle uses those values; no alternative `15:` completion label is asserted.
