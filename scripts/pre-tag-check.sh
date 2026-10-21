#!/usr/bin/env bash
# Run before pushing a release tag. A tag push starts build.yml and release.yml,
# and a failure there leaves a published release without its images. This runs
# the same checks locally, in the same shape, and stops at the first failure:
# the gofmt and vet scopes, the cross-build matrix and the test split are the
# ones build.yml uses, and the Docker image is built (amd64, no push) so a
# Dockerfile break cannot reach a tag.
#
# If docker is missing or not running that is a FAILURE unless --skip-docker is
# passed, and the run must then say why it was skipped.
#
#   scripts/pre-tag-check.sh                # everything, including the docker build
#   scripts/pre-tag-check.sh --quick        # gofmt, vet and the cross-build only
#   scripts/pre-tag-check.sh --skip-docker  # everything except the docker build
set -euo pipefail
cd "$(dirname "$0")/.."

# Some tests bind a unix socket under t.TempDir(), whose path embeds the test
# name; a long TMPDIR pushes it past the 108 byte sun_path limit ("bind: invalid
# argument"). A hosted runner uses /tmp, so use /tmp here too.
export TMPDIR=/tmp

quick=0
skip_docker=0
for arg in "$@"; do
  case "$arg" in
    --quick) quick=1 ;;
    --skip-docker) skip_docker=1 ;;
    *)
      echo "unknown option: $arg" >&2
      echo "usage: scripts/pre-tag-check.sh [--quick] [--skip-docker]" >&2
      exit 2
      ;;
  esac
done

echo "== gofmt (build.yml lint job scope: provider/)"
unformatted=$(gofmt -l provider/)
if [ -n "$unformatted" ]; then
  echo "Files not gofmt-clean:"
  echo "$unformatted"
  exit 1
fi
echo "  ok"

echo "== go vet ./provider/... (build.yml lint job)"
go vet ./provider/...
echo "  ok"

echo "== go vet ./internal/urnettools/ for freebsd (build.yml freebsd-vet job)"
for arch in amd64 arm64; do
  GOOS=freebsd GOARCH=$arch go vet ./internal/urnettools/ >/dev/null
  echo "  ok freebsd/$arch"
done

echo "== cross-build every shippable binary (build.yml cross-build job)"
for goos in linux darwin windows; do
  for goarch in amd64 arm64; do
    for bin in provider urnet-tools urnet-docker; do
      GOOS=$goos GOARCH=$goarch CGO_ENABLED=0 go build -o /dev/null ./cmd/$bin
    done
    echo "  ok $goos/$goarch (provider, urnet-tools, urnet-docker)"
  done
done

if [ "$quick" = 1 ]; then
  echo "quick checks passed (tests and the docker build not run)"
  exit 0
fi

echo "== docker build (amd64, no push; build.yml build-and-push job)"
if [ "$skip_docker" = 1 ]; then
  echo "  SKIPPED via --skip-docker: build.yml and release.yml still build the image at the tag;"
  echo "  the run/report must state why this check was skipped."
elif command -v docker >/dev/null 2>&1 && docker info >/dev/null 2>&1; then
  docker build --progress=plain -t sn-pre-tag-check:local .
  echo "  docker build ok"
else
  echo "ERROR: docker is not installed or its daemon is not running, so the Dockerfile cannot be checked." >&2
  echo "  Install/start docker and re-run, or re-run with --skip-docker and state why it is skipped." >&2
  exit 1
fi

echo "== go test -short -race ./provider/... (build.yml test job)"
go test -short -race -timeout 600s ./provider/...

echo "== go test -short ./miner/... ./internal/urnettools/... ./cli/... ./cmd/... (build.yml test job)"
go test -short -timeout 900s ./miner/... ./internal/urnettools/... ./cli/... ./cmd/...

echo "all pre-tag checks passed"
