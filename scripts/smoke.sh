#!/bin/sh
set -eu

: "${IFD_TARGET:?set IFD_TARGET to the app user and private gateway}"
: "${IFD_USER:?set IFD_USER to the configured app username}"
: "${IFD_KNOWN_HOSTS:?set IFD_KNOWN_HOSTS to a verified host-key file}"
: "${IFD_IDENTITY:?set IFD_IDENTITY to your existing private-key path}"
port=${IFD_PORT:-2222}
name="ifd-smoke-$(date +%s)-$$"
probe_name="${name}-cap"
test "${#probe_name}" -le 63
created=false
probe_created=false

run() {
  ssh -o BatchMode=yes -o StrictHostKeyChecking=yes -o IdentitiesOnly=yes \
    -o ForwardAgent=no -o ClearAllForwardings=yes -o ConnectTimeout=5 \
    -o "UserKnownHostsFile=$IFD_KNOWN_HOSTS" -i "$IFD_IDENTITY" -p "$port" \
    -- "$IFD_TARGET" "$@"
}

cleanup() {
  status=$?
  trap - 0
  if [ "$created" = true ]; then
    run rm "$name" --json || :
  fi
  if [ "$probe_created" = true ]; then
    run rm "$probe_name" --json || :
  fi
  exit "$status"
}
trap cleanup 0
trap 'exit 130' INT
trap 'exit 143' TERM

run whoami --json | jq -e --arg user "$IFD_USER" '.name == $user'
run help --json >/dev/null
run new "--name=$name" --cpu=1 --memory=2GiB --json
created=true
run ls --json | jq -e --arg name "$name" '.vms | any(.vm_name == $name)'
test "$(run ssh "$name" 'printf IFD_EXEC_OK')" = IFD_EXEC_OK
test "$(printf 'IFD_STDIN_OK' | run ssh "$name" cat)" = IFD_STDIN_OK
test "$(run ssh "$name" 'cat /sys/fs/cgroup/memory.max')" = 2147483648
test "$(run ssh "$name" 'cat /sys/fs/cgroup/pids.max')" = 256

set +e
run ssh "$name" 'exit 37'
code=$?
set -e
test "$code" = 37

if run pause "$name"; then
  printf '%s\n' 'FAIL: pause must not pretend to implement RAM snapshotting' >&2
  exit 1
fi
if run new "--name=$probe_name" --json; then
  probe_created=true
  printf '%s\n' 'FAIL: instance cap was not enforced' >&2
  exit 1
fi

run rm "$name" --json
created=false
run ls --json | jq -e --arg name "$name" '.vms | all(.vm_name != $name)'
printf '%s\n' 'PASS: authenticated create/list/exec/stdin/exit-status/limits/delete smoke'
