#!/bin/sh
# Checks an installed toli works, as a new user would first use it: the
# daemon starts, and an agent can be added and given an MCP URL.
set -eu

log="${RUNNER_TEMP:-/tmp}/toli-daemon.log"
toli daemon >"$log" 2>&1 &
pid=$!
trap 'kill $pid 2>/dev/null || true' EXIT

i=0
until toli agent list >/dev/null 2>&1; do
  i=$((i + 1))
  if [ $i -ge 60 ]; then
    echo "the daemon didn't start:" >&2
    cat "$log" >&2
    exit 1
  fi
  sleep 0.5
done

toli agent add smoke
out="$(toli agent new-token smoke)"
echo "$out"
echo "$out" | grep -q '^mcp_url *http://127.0.0.1:7420/mcp/toli_'
echo "toli works"
