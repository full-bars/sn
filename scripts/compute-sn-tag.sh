#!/usr/bin/env bash
# Prints this release's tag.
#
# The shape is v<date>-<build>-sn. The build number is the seconds since the
# 2023-05-23 founding epoch, times ten:
#
#   build = ($(date -u '+%s') - 1684800000) * 10
#
# It is not a raw unix stamp. Every tag in this repo uses the derived number,
# and a raw stamp produces a shape nothing else matches.
#
# The -sn suffix names the engine: this repo is the only one that runs it, while
# meso-miner runs the old engine and marks its builds -meso, and the 3.23-fix
# lineage keeps its own semver. The shape alone cannot tell the lineages apart,
# because 3.23-fix also reports a date-epoch stamp to the backend, so the suffix
# carries that job.
#
# The build number is taken at cut time, so each repo stamps its own and two
# repos never share a tag by accident. An upstream tag carries no suffix.
#
# Usage:
#   tag=$(scripts/compute-sn-tag.sh)
#   git tag -s "$tag" -m "..."
#   git push origin "$tag"
set -euo pipefail

founding=1684800000
now=$(date -u '+%s')

printf 'v%s-%s-sn\n' "$(date -u '+%Y.%-m.%-d')" "$(( (now - founding) * 10 ))"
