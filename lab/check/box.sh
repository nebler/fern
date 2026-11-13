# shellcheck shell=bash
# Sourced by lab/check.sh, whose variables and helpers shellcheck can't see
# from here (SC2154, SC2034); the rest as there.
# shellcheck disable=SC2329,SC2086,SC2154,SC2034
# Section box of lab/check.sh, which sources this file: it has no use by itself.
box() {
  echo "== a box keeps no state of its own"
  fresh $ENV
  sql $ENV "insert into users (name, email) values ('Edsger', 'edsger@example.com')" >/dev/null
  # setsid: an exec's own process group ends with it. The pause lets the
  # child leave the group before the exec ends and the group is killed.
  fern exec $ENV -- sh -c 'echo x > /tmp/outside && echo y > inside && setsid -f sleep 1000 >/dev/null 2>&1 && sleep 0.5'
  check "the box runs under gVisor" "Starting gVisor..." "$(fern exec $ENV -- dmesg | head -1 | cut -c16-)"
  check "the daemon runs the box with runsc" yes "$([ -n "$(pod_uid $ENV)" ] && echo yes || echo no)"
  check "the root filesystem is read-only" no "$(succeeds fern exec $ENV -- touch /etc/x)"
  # The C library's lookup, as Node's fetch makes it: curl knows localhost itself.
  check "localhost is the loopback address" 127.0.0.1 "$(fern exec $ENV -- getent ahostsv4 localhost | awk 'NR == 1 { print $1 }')"
  check "the box writes to the pool" y "$(in_daemon "cat /var/lib/fern/envs/$ENV/live/inside")"
  # What the node is doing now, for a person at the server: the box, how
  # long it has been up, its memory and CPU time, and what holds it awake.
  check "fern top lists the box, with what holds it" 1 "$(fern top | grep -cE "^$ENV +services +[0-9.smhd]+ +[0-9.]+ MiB +[0-9.smhd]+ +[^?]" || true)"
  check "a stray process runs" 1 "$(procs $ENV | grep -cx sleep || true)"
  check "fern why says the box is awake, and when it sleeps" "awake in" "$(fern why $ENV | awk -v e=$ENV '$1 == e { print $2, $4 }')"
  local mounted=0
  metrics >"$TMP/metrics"
  for _ in $(seq "$WAKES"); do
    fern sleep $ENV | took >>"$TMP/sleep"
    mounted=$((mounted + $(mounts $ENV)))
    fern wake $ENV | took >>"$TMP/wake"
  done
  metric box.sleep_ms "sleep (until the box is gone), ms" "$(stats <"$TMP/sleep")"
  metric box.wake_ms "wake (until Postgres is ready), ms" "$(stats <"$TMP/wake")"
  note "wake steps: $(wake_steps "$TMP/metrics")"
  # Memory without the page cache, which comes and goes: the box's (Postgres
  # idle, gVisor included) and the daemon's. gVisor keeps what the box's
  # programs allocate in shared memory, so the box's counts shmem too.
  metric box.box_memory_mib "an awake box's memory, MiB" \
    "$(pod_cgroup $ENV memory.stat | awk '$1 == "anon" || $1 == "shmem" { b += $2 } END { printf "%.1f", b / 1048576 }')"
  metric box.daemon_memory_mib "the daemon's memory, MiB" \
    "$(in_daemon 'cat /proc/1/status' | awk '$1 == "RssAnon:" { printf "%.1f", $2 / 1024 }')"
  check "the workspace is unmounted at every sleep" 0 "$mounted"
  check "and mounted once while awake" 1 "$(mounts $ENV)"
  check "a write outside the workspace is gone" no "$(succeeds fern exec $ENV -- test -e /tmp/outside)"
  check "a write in it stays" y "$(fern exec $ENV -- cat inside)"
  check "and so does the database (4 users)" 4 "$(sql $ENV 'select count(*) from users')"
  check "only the declared services come back" 0 "$(procs $ENV | grep -cx sleep || true)"
  # T3 asks for a pre-wake when a thread is opened or reverted. It answers
  # at once; a command after the user's think time (3 s here) finds the box
  # up. Interleaved with commands to an asleep box, which wait for the wake.
  metrics >"$TMP/metrics"
  for _ in $(seq "$PREWAKES"); do
    fern sleep $ENV >/dev/null
    fern prewake $ENV | took >>"$TMP/prewake"
    sleep 3
    # With agent boxes a pre-wake wakes Claude's: the next turn's command.
    if [ "${AGENTS:-0}" = 1 ]; then
      node_ms "$FERN_CLI" exec --agent $ENV -- true >>"$TMP/prewoken"
    else
      node_ms "$FERN_CLI" exec $ENV -- true >>"$TMP/prewoken"
    fi
    fern sleep $ENV >/dev/null
    node_ms "$FERN_CLI" exec $ENV -- true >>"$TMP/asleep"
  done
  metric box.prewake_ms "a pre-wake's answer, ms" "$(stats <"$TMP/prewake")"
  metric box.prewoken_exec_ms "a command 3 s after a pre-wake, ms" "$(stats <"$TMP/prewoken")"
  metric box.asleep_exec_ms "a command to an asleep box, ms" "$(stats <"$TMP/asleep")"
  check "every pre-wake was used by its command" "$PREWAKES" "$(metrics | counter_since "$TMP/metrics" 'fern_prewakes_total{result="used"}')"
  sql $ENV "insert into users (name, email) values ('Barbara', 'barbara@example.com')" >/dev/null
  local users=""
  start=$(now_ms)
  kill_box $ENV
  # The box comes back with the next command, which finds the old one gone.
  for _ in $(seq 80); do
    users=$(sql $ENV 'select count(*) from users' 2>/dev/null) && break
    sleep 0.25
  done
  check "Postgres recovers after its box is killed (5 users)" 5 "$users"
  metric box.force_delete_recovery_ms "kill until the new box's Postgres answers, ms" $(($(now_ms) - start))
  check "and the seed ran once" 1 "$(sql $ENV "select count(*) from users where name = 'Ada'")"
  # That was its first crash; at the third in five minutes it is given up on.
  kill_box $ENV
  fern exec $ENV -- true >/dev/null 2>&1
  kill_box $ENV
  check "a box that crashed 3 times in 5 minutes isn't woken again" yes \
    "$(succeeds grep -q 'keeps crashing' <<<"$(fern exec $ENV -- true 2>&1)")"
  check "until a person wakes it" 0 "$(fern wake $ENV >/dev/null 2>&1; fern exec $ENV -- true >/dev/null 2>&1; echo $?)"
}
