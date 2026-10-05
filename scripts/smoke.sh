#!/usr/bin/env bash
# smoke.sh IMAGE: runs the image as the gateway's runner would and checks
# that it is healthy, refuses a wrong token, and lists each service's tools.
set -uo pipefail
image="$1"; token="smoke-$RANDOM$RANDOM"; port=$((17000 + RANDOM % 1000))
cid=$(docker run -d --rm -e MCPLOCKER_SHIM_TOKEN="$token" -p "127.0.0.1:$port:7777" "$image") || exit 1
trap 'docker stop "$cid" >/dev/null 2>&1' EXIT
for _ in $(seq 1 50); do
  curl -fs "http://127.0.0.1:$port/healthz" >/dev/null && break
  sleep 0.1
done
fail=0
code=$(curl -s -o /dev/null -w '%{http_code}' -X POST -H "Authorization: Bearer wrong" "http://127.0.0.1:$port/gmail/mcp" -d '{}')
[[ "$code" == 401 ]] || { echo "a wrong token got $code, not 401"; fail=1; }
check() { # service tool...
  local svc="$1"; shift
  local out
  out=$(curl -s -X POST -H "Authorization: Bearer $token" "http://127.0.0.1:$port/$svc/mcp" \
    -d '{"jsonrpc":"2.0","id":1,"method":"tools/list"}')
  for tool in "$@"; do
    [[ "$out" == *"\"name\":\"$tool\""* ]] || { echo "$svc tools/list is missing $tool"; fail=1; }
  done
}
check calendar list_events create_event delete_event
check tasks list_tasks create_task
check drive search_files get_file_metadata
check gmail search_messages send_draft create_filter
user=$(docker inspect -f '{{.Config.User}}' "$image")
[[ "$user" == "65534:65534" ]] || { echo "the image runs as '$user', not 65534:65534"; fail=1; }
[[ $fail == 0 ]] && echo "smoke: ok ($image)"
exit $fail
