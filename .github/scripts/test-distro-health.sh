#!/bin/sh
# Exercise the actual health probe with and without a streaming child.
set -eu
HARNESS=${1:?path to distro-features.sh required}
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT HUP INT TERM
awk '/^probe_health\(\) \{/{copy=1} /^b3_surface\(\) \{/{copy=0} copy{print}' "$HARNESS" > "$work/probe.sh"
. "$work/probe.sh"
have() { command -v "$1" >/dev/null 2>&1; }
cat > "$work/tools" <<'SCRIPT'
#!/bin/sh
case "$TEST_MODE" in
    stream)
        printf 'Streaming proxy health events\n'
        sleep 60 &
        printf '%s\n' "$!" > "$CHILD_PID_FILE"
        wait ;;
    silent) sleep 60 ;;
    error) printf 'fixture error\n'; exit 7 ;;
    normal) printf 'No event log yet\n' ;;
esac
SCRIPT
chmod +x "$work/tools"
WORK=$work STATE_HOME=$work STATE_DIR=$work TOOLS_BIN=$work/tools FEATURE_HEALTH_TIMEOUT=1
CHILD_PID_FILE=$work/child; export CHILD_PID_FILE
for TEST_MODE in stream normal error silent; do
    export TEST_MODE
    probe_health
    case "$TEST_MODE:$HEALTH_TIMED_OUT:$HEALTH_RC" in
        stream:1:*|silent:1:*|normal:0:0|error:0:7) : ;;
        *) printf 'FAIL %s: timeout=%s rc=%s\n' "$TEST_MODE" "$HEALTH_TIMED_OUT" "$HEALTH_RC"; exit 1 ;;
    esac
    if [ "$TEST_MODE" = stream ]; then
        child=$(cat "$CHILD_PID_FILE")
        # A reaping init removes it; without one, a zombie is harmless and dead.
        if [ -r "/proc/$child/stat" ]; then
            state=$(awk '{print $3}' "/proc/$child/stat")
            [ "$state" = Z ] || { printf 'FAIL stream: child still running\n'; exit 1; }
        fi
    fi
    printf 'PASS health probe %s\n' "$TEST_MODE"
done
