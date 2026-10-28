#!/bin/sh
set -eu

# Real-gateway smoke test. It only touches uniquely named containers it creates.
# Set IFD_TEST_GENERATED_NAME=1 to also exercise an unnamed (fruit) create.
: "${IFD_TARGET:?set IFD_TARGET to the app user and private gateway}"
: "${IFD_USER:?set IFD_USER to the configured app username}"
: "${IFD_KNOWN_HOSTS:?set IFD_KNOWN_HOSTS to a verified host-key file}"
: "${IFD_IDENTITY:?set IFD_IDENTITY to your existing private-key path}"
port=${IFD_PORT:-2222}

command -v jq >/dev/null 2>&1 || {
  printf '%s\n' 'FAIL: jq is required' >&2
  exit 1
}

name="ifd-smoke-$(date +%s)-$$"
probe_name="${name}-cap"
created=false
probe_created=false
generated_created=false
generated_name=""
tmpdir=$(mktemp -d "${TMPDIR:-/tmp}/ifd-smoke.XXXXXX")

run() {
  ssh -o BatchMode=yes -o StrictHostKeyChecking=yes -o IdentitiesOnly=yes \
    -o ForwardAgent=no -o ClearAllForwardings=yes -o ConnectTimeout=5 \
    -o ServerAliveInterval=5 -o ServerAliveCountMax=2 \
    -o "UserKnownHostsFile=$IFD_KNOWN_HOSTS" -i "$IFD_IDENTITY" -p "$port" \
    -- "$IFD_TARGET" "$@"
}

fail() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
}

safe_name() {
  case $1 in
    ''|-*|*-|[0-9]*|*[!a-z0-9-]*) return 1 ;;
  esac
  [ "${#1}" -le 63 ]
}

cleanup() {
  status=$?
  trap - 0 INT TERM
  if [ "$created" = true ] && safe_name "$name"; then
    run rm "$name" --json >/dev/null 2>&1 || printf 'cleanup: could not remove %s\n' "$name" >&2
  fi
  if [ "$probe_created" = true ] && safe_name "$probe_name"; then
    run rm "$probe_name" --json >/dev/null 2>&1 || printf 'cleanup: could not remove %s\n' "$probe_name" >&2
  fi
  if [ "$generated_created" = true ] && safe_name "$generated_name"; then
    run rm "$generated_name" --json >/dev/null 2>&1 || printf 'cleanup: could not remove %s\n' "$generated_name" >&2
  fi
  rm -rf "$tmpdir" 2>/dev/null || :
  exit "$status"
}
trap cleanup 0
trap 'exit 130' INT
trap 'exit 143' TERM

test "${#name}" -le 63
test "${#probe_name}" -le 63
safe_name "$name" || fail "generated smoke name is not a valid slug"

run whoami --json | jq -e --arg user "$IFD_USER" '.name == $user' >/dev/null

help_json=$(run help --json)
printf '%s' "$help_json" | jq -e '.program == "incus-for-devs"' >/dev/null
printf '%s' "$help_json" | jq -e '.commands[] | select(.name == "new") | .usage | contains("--processes=N")' >/dev/null
printf '%s' "$help_json" | jq -e '.limits | (.max_instances > 0) and (.cpu > 0) and (.processes > 0)' >/dev/null

max_instances=$(printf '%s' "$help_json" | jq -er '.limits.max_instances')
max_cpu=$(printf '%s' "$help_json" | jq -er '.limits.cpu')
max_processes=$(printf '%s' "$help_json" | jq -er '.limits.processes')
expected_processes=256
if [ "$max_processes" -lt 256 ]; then
  expected_processes=$max_processes
fi

pre_list=$(run ls --json)
existing_count=$(printf '%s' "$pre_list" | jq -er '.vms | length')
free_slots=$((max_instances - existing_count))
[ "$free_slots" -ge 1 ] || fail "no free instance slot; refusing to alter existing containers"
printf '%s' "$pre_list" | jq -e --arg main "$name" --arg probe "$probe_name" \
  '.vms | type == "array" and all(.vm_name != $main and .vm_name != $probe)' >/dev/null \
  || fail "refusing to continue: a smoke-test name already exists"

cpu_over=$((max_cpu + 1))
if run new "--name=$probe_name" "--cpu=$cpu_over" --memory=1GiB --json \
  >"$tmpdir/cpu.json" 2>"$tmpdir/cpu.err"; then
  probe_created=true
  fail "CPU over-limit unexpectedly created $probe_name"
fi
jq -e '.error | test("^CPU request [0-9]+ exceeds maximum [0-9]+$")' "$tmpdir/cpu.json" >/dev/null || {
  cat "$tmpdir/cpu.json" "$tmpdir/cpu.err" >&2
  fail "CPU over-limit was not rejected by the CPU admission check"
}

processes_over=$((max_processes + 1))
if run new "--name=$probe_name" --cpu=1 --memory=1GiB "--processes=$processes_over" --json \
  >"$tmpdir/processes.json" 2>"$tmpdir/processes.err"; then
  probe_created=true
  fail "process over-limit unexpectedly created $probe_name"
fi
jq -e '.error | test("^process request [0-9]+ exceeds maximum [0-9]+$")' "$tmpdir/processes.json" >/dev/null || {
  cat "$tmpdir/processes.json" "$tmpdir/processes.err" >&2
  fail "process over-limit was not rejected by the process admission check"
}

if [ "${IFD_TEST_GENERATED_NAME:-0}" = 1 ]; then
  if ! generated_json=$(run new --json); then
    fail "unnamed create failed"
  fi
  generated_name=$(printf '%s' "$generated_json" | jq -er \
    '.vm_name | select(type == "string") | select(test("^[a-z]([a-z0-9-]{0,61}[a-z0-9])?$"))') \
    || fail "unnamed create returned no strict slug name: $generated_json"
  printf '%s' "$pre_list" | jq -e --arg name "$generated_name" '.vms | all(.vm_name != $name)' >/dev/null \
    || fail "refusing to manage generated name $generated_name because it existed before"
  generated_created=true
  run ls --json | jq -e --arg name "$generated_name" '.vms | any(.vm_name == $name)' >/dev/null \
    || fail "generated instance $generated_name is missing from ls"
  if [ "$free_slots" -lt 2 ]; then
    run rm "$generated_name" --json | jq -e --arg name "$generated_name" '.removed == [$name]' >/dev/null
    generated_created=false
  fi
fi

if ! main_json=$(run new "--name=$name" --json); then
  fail "explicit create failed"
fi
created=true
printf '%s' "$main_json" | jq -e --arg name "$name" '.vm_name == $name' >/dev/null \
  || fail "create returned an unexpected name: $main_json"
printf '%s' "$main_json" | jq -e '.cpu == "1" and .memory == "2GiB"' >/dev/null \
  || fail "create defaults are not 1 CPU and 2GiB: $main_json"

run ls --json | jq -e --arg name "$name" '.vms | any(.vm_name == $name)' >/dev/null
run ls -l "$name" | awk -F '\t' -v name="$name" -v processes="$expected_processes" \
  '{if (NF != 6 || $1 != name || $5 != processes) exit 1} END {if (NR != 1) exit 1}' \
  || fail "ls -l did not report six columns with the applied process limit"
status_of() {
  run ls --json | jq -er --arg name "$1" '.vms[] | select(.vm_name == $name) | .status'
}
[ "$(status_of "$name")" = Running ] || fail "new container is not Running"

test "$(run ssh "$name" 'printf IFD_EXEC_OK')" = IFD_EXEC_OK
test "$(run ssh "$name" 'cat /sys/fs/cgroup/memory.max')" = 2147483648
test "$(run ssh "$name" 'cat /sys/fs/cgroup/pids.max')" = "$expected_processes"

printf '\000\001\010\011\012\015\177\200\376\377' >"$tmpdir/in.bin"
set +e
run ssh "$name" 'cat; exit 37' <"$tmpdir/in.bin" >"$tmpdir/out.bin" 2>"$tmpdir/err.bin"
code=$?
set -e
[ "$code" = 37 ] || fail "binary ssh command exited $code, want 37"
cmp "$tmpdir/in.bin" "$tmpdir/out.bin" >/dev/null 2>&1 || fail "binary stdin/stdout bytes changed"
[ ! -s "$tmpdir/err.bin" ] || fail "binary ssh command wrote unexpected stderr"

run ssh "$name" 'printf IFD_FS_OK > /root/ifd-smoke-marker'
test "$(run ssh "$name" 'cat /root/ifd-smoke-marker')" = IFD_FS_OK

run stop "$name" --json | jq -e --arg name "$name" '.vm_name == $name' >/dev/null
[ "$(status_of "$name")" = Stopped ] || fail "stop did not report Stopped"
run start "$name" --json | jq -e --arg name "$name" '.vm_name == $name' >/dev/null
[ "$(status_of "$name")" = Running ] || fail "start did not report Running"
test "$(run ssh "$name" 'cat /root/ifd-smoke-marker')" = IFD_FS_OK || fail "filesystem was lost across stop/start"

run stop "$name" --json | jq -e --arg name "$name" '.vm_name == $name' >/dev/null
run resume "$name" --json | jq -e --arg name "$name" '.vm_name == $name' >/dev/null
[ "$(status_of "$name")" = Running ] || fail "resume did not report Running"
test "$(run ssh "$name" 'cat /root/ifd-smoke-marker')" = IFD_FS_OK || fail "filesystem was lost across stop/resume"

run restart "$name" --json | jq -e --arg name "$name" '.vm_name == $name' >/dev/null
[ "$(status_of "$name")" = Running ] || fail "restart did not report Running"
test "$(run ssh "$name" 'cat /root/ifd-smoke-marker')" = IFD_FS_OK || fail "filesystem was lost across restart"

if [ "$max_instances" -eq 1 ]; then
  if run new "--name=$probe_name" --cpu=1 --memory=1GiB --json \
    >"$tmpdir/cap.json" 2>"$tmpdir/cap.err"; then
    probe_created=true
    fail "instance cap was not enforced"
  fi
  jq -e '.error | test("^project instance limit reached$")' "$tmpdir/cap.json" >/dev/null || {
    cat "$tmpdir/cap.json" "$tmpdir/cap.err" >&2
    fail "second instance was not rejected by the instance cap"
  }
fi

if run pause "$name" >/dev/null 2>"$tmpdir/pause.err"; then
  fail "pause must not pretend to implement RAM snapshotting"
fi
grep -q 'unsupported' "$tmpdir/pause.err" || fail "pause did not explain that it is unsupported"

run rm "$name" --json | jq -e --arg name "$name" '.removed == [$name]' >/dev/null
created=false
run ls --json | jq -e --arg name "$name" '.vms | all(.vm_name != $name)' >/dev/null

if [ "$generated_created" = true ]; then
  run rm "$generated_name" --json | jq -e --arg name "$generated_name" '.removed == [$name]' >/dev/null
  generated_created=false
  run ls --json | jq -e --arg name "$generated_name" '.vms | all(.vm_name != $name)' >/dev/null
fi

before_names=$(printf '%s' "$pre_list" | jq -c '.vms | map(.vm_name) | sort')
after_names=$(run ls --json | jq -c '.vms | map(.vm_name) | sort')
[ "$before_names" = "$after_names" ] || fail "project inventory changed beyond the disposable smoke containers"
printf '%s\n' 'PASS: authenticated defaults/create/list/exec/binary-stdin/exit-status/limits-rejection/lifecycle/persistence/delete smoke'
