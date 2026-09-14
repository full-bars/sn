# Probe provenance and limits

No separate probe capture files were written. `TRANSCRIPT-DERIVED-OBSERVATIONS.tsv` is a public-safe record created from two closed `functions.exec` output chunks: `chunk502971` and `chunk74cfb4`. Its checksum identifies this derived record, not an original HTTP capture.

The first observation confirms a direct guarded-LAN chain-ID read. The second confirms direct guarded-LAN reads for one pinned canonical block and the latest finalized receipt for the named action. Both reported HTTP 200 and the receipt reported status `0x1`.

These reduced, read-only observations do not replay the complete binding postcondition, identify the HTTP request that produced the recorded 429, or establish a source defect. They include no endpoint address, request body, response body, retry, or write.
