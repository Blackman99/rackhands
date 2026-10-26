#!/usr/bin/env bash
# End-to-end: the rackhands binary against
#   * the DMTF Redfish-Mockup-Server serving the DMTF public-rackmount1 mockup
#     and the Dell / HPE (iLO 5 and iLO 4) / Lenovo / Supermicro mockups in testdata/mockups
#   * an OpenIPMI ipmi_sim BMC (IPMI 2.0 lanplus) with sensors, SEL and FRU
# Needs: ./rackhands (built), python3, jq, ipmi_sim, RMS_DIR=<Redfish-Mockup-Server checkout>.
set -euo pipefail
cd "$(dirname "$0")/.."
BIN=${BIN:-./rackhands}
RMS_DIR=${RMS_DIR:?set RMS_DIR to a Redfish-Mockup-Server checkout}
WORK=${WORK:-$(mktemp -d)}
PIDS=()
cleanup() { for p in "${PIDS[@]}"; do kill "$p" 2>/dev/null || true; done; }
trap cleanup EXIT
[ -n "${KEEP:-}" ] && trap - EXIT
pass=0
ok() { echo "ok   $*"; pass=$((pass+1)); }
diag() {
  echo "--- diagnostics"; for f in "$WORK"/*.log "$WORK"/ipmi/log; do [ -f "$f" ] && { echo "== $f"; tail -20 "$f"; }; done
  [ -f "$WORK/inv.json" ] && { echo "== inv.json"; head -c 3000 "$WORK/inv.json"; }
}
fail() { echo "FAIL $*"; diag; exit 1; }
expect() { local what=$1; shift; if "$@"; then ok "$what"; else fail "$what"; fi; }

# --- Redfish mockup servers (DMTF reference implementation of a BMC)
port=8400
for m in dell hpe lenovo supermicro; do
  port=$((port+1))
  python3 "$RMS_DIR/redfishMockupServer.py" -S -D "testdata/mockups/$m" -p $port >"$WORK/rms-$m.log" 2>&1 & PIDS+=($!)
done
python3 "$RMS_DIR/redfishMockupServer.py" -S -D "$RMS_DIR/public-rackmount1" -p 8405 >"$WORK/rms-dmtf.log" 2>&1 & PIDS+=($!)
python3 "$RMS_DIR/redfishMockupServer.py" -S -D "testdata/mockups/hpe-ilo4" -p 8406 >"$WORK/rms-hpe-ilo4.log" 2>&1 & PIDS+=($!)

# --- IPMI simulator
SIM="$WORK/ipmi"; mkdir -p "$SIM"
echo 64 >"$SIM/sensor1"; echo 117 >"$SIM/sensor2"; echo 48 >"$SIM/sensor3"
sed "s#@STATE@#$SIM#g" test/ipmi/sim.emu.in >"$SIM/sim.emu"
export RACKHANDS_SIM_STATE="$SIM/power"
(cd test/ipmi && exec ipmi_sim -c lan.conf -f "$SIM/sim.emu" -s "$SIM" -n) >"$SIM/log" 2>&1 & PIDS+=($!)

for p in 8401 8402 8403 8404 8405 8406; do
  for _ in $(seq 50); do curl -sf "http://127.0.0.1:$p/redfish/v1/" >/dev/null && break; sleep 0.2; done
  curl -sf "http://127.0.0.1:$p/redfish/v1/" >/dev/null || fail "Redfish mockup server on :$p did not come up"
done
for _ in $(seq 50); do ipmitool -I lanplus -H 127.0.0.1 -p 9623 -U admin -P secret chassis power status >/dev/null 2>&1 && break; sleep 0.2; done
ipmitool -I lanplus -H 127.0.0.1 -p 9623 -U admin -P secret chassis power status || fail "ipmi_sim did not come up"

cat >"$WORK/targets.yaml" <<YAML
defaults: {username: admin, password_env: BMC_PASSWORD}
audit_log: $WORK/audit.jsonl
targets:
  - {name: r750,   redfish: "http://127.0.0.1:8401", tags: [prod], allow: [power, boot]}
  - {name: dl380,  redfish: "http://127.0.0.1:8402", tags: [prod], allow: [virtual-media]}
  - {name: sr650,  redfish: "http://127.0.0.1:8403", tags: [prod]}
  - {name: x11,    redfish: "http://127.0.0.1:8404"}
  - {name: dmtf,   redfish: "http://127.0.0.1:8405", allow: [all]}
  - {name: sim,    ipmi: "127.0.0.1:9623", allow: [power]}
  - {name: legacy, redfish: "http://127.0.0.1:1", ipmi: "127.0.0.1:9623", timeout: 5s}
  - {name: simro,  ipmi: "127.0.0.1:9623", username: reader}
  - {name: ilo4,   redfish: "http://127.0.0.1:8406"}
YAML
export BMC_PASSWORD=secret
R() { "$BIN" --config "$WORK/targets.yaml" "$@"; }

# One IPMI round trip should take well under a second against a local simulator.
t0=$(date +%s)
RACKHANDS_IPMI_DEBUG=1 R power sim 2>&1 | while IFS= read -r l; do printf '%s %s\n' "$(date +%s.%N)" "$l"; done >"$WORK/ipmi-debug.log" || true
t1=$(date +%s)
if [ $((t1 - t0)) -gt 5 ]; then
  echo "SLOW IPMI round trip: $((t1 - t0))s; lines after a pause > 1s:"; slow=1
  awk 'NR>1 && $1-p>1 {print "---- pause " ($1-p) "s before line " NR; for(i=NR-12;i<NR;i++) print h[i%20]; print $0} {p=$1; h[NR%20]=$0}' "$WORK/ipmi-debug.log" | grep -v '^[0-9.]* *$' | head -120
fi
[ -z "${slow:-}" ] && ok "IPMI session opens quickly (no cipher-suite probe stall)" || fail "IPMI round trip took $((t1 - t0))s"

# --- reads, every vendor
R --json inventory >"$WORK/inv.json" || fail "inventory over the whole fleet"
expect "vendor detection" test "$(jq -r '[.[] | .data.vendor] | join(",")' "$WORK/inv.json")" = "Dell,HPE,Lenovo,Supermicro,Generic,Supermicro,Supermicro,Supermicro,HPE"
expect "DMTF mockup inventory" test "$(jq -r '.[] | select(.target=="dmtf") | .data.serial' "$WORK/inv.json")" = "437XR1138R2"
expect "IPMI FRU inventory" test "$(jq -r '.[] | select(.target=="sim") | .data.model' "$WORK/inv.json")" = "SYS-1029P-SIM"
expect "IPMI fallback when Redfish is down" test "$(jq -r '.[] | select(.target=="legacy") | .via' "$WORK/inv.json")" = "ipmi (fallback)"

# --- effective privilege of the configured account (issue #1), read-only
acct() { jq -r --arg t "$1" '.[] | select(.target==$t) | .data.account | "\(.role) \(.can_control) \(.warning != null)"' "$WORK/inv.json"; }
expect "Redfish admin on a box with power allowed: no warning" test "$(acct r750)" = "Administrator true false"
expect "Redfish ReadOnly account detected" test "$(acct sr650)" = "ReadOnly false false"
expect "Redfish Operator on a read-only target warns" test "$(acct x11)" = "Operator true true"
expect "HPE iLO privileges from Oem.Hpe.Privileges" test "$(acct dl380)" = "Administrator true false"
expect "IPMI User-privilege account detected (Get Session Info + Get User Access)" test "$(acct simro)" = "User false false"
expect "IPMI Administrator on a read-only target warns" test "$(acct legacy)" = "Administrator true true"
R inventory x11 sr650 2>"$WORK/inv-warn.txt" >/dev/null
expect "CLI prints the account warning on stderr" grep -q "WARN  x11: logs in as Operator" "$WORK/inv-warn.txt"
expect "no warning for a read-only account" sh -c "! grep -q 'WARN  sr650' \"$WORK/inv-warn.txt\""

set +e; R --json health >"$WORK/health.json"; code=$?; set -e
expect "health exits 1 when something is unhealthy" test $code -eq 1
expect "Dell iDRAC PSU rollup" grep -q '"subsystem power supplies"' "$WORK/health.json"
expect "HPE iLO aggregate fans" test "$(jq -r '.[] | select(.target=="dl380") | .data.overall' "$WORK/health.json")" = "Critical"
expect "Lenovo healthy" test "$(jq -r '.[] | select(.target=="sr650") | .data.overall' "$WORK/health.json")" = "OK"
expect "IPMI threshold sensor warning" test "$(jq -r '.[] | select(.target=="sim") | .data.problems[0].component' "$WORK/health.json")" = "temperature PSU2 Temp"
R health sr650 >/dev/null && ok "health exits 0 for a healthy box"

expect "Dell SEL (not Lifecycle log)" sh -c "\"$BIN\" --config $WORK/targets.yaml --json events r750 | jq -e '.data[0].source | startswith(\"Sel\")' >/dev/null"
expect "HPE IML severity mapping" sh -c "\"$BIN\" --config $WORK/targets.yaml --json events dl380 --min critical | jq -e '.data | length == 1' >/dev/null"
expect "Supermicro Log1" sh -c "\"$BIN\" --config $WORK/targets.yaml events x11 | grep -q VBAT"
expect "IPMI SEL" sh -c "\"$BIN\" --config $WORK/targets.yaml --json events sim | jq -e '[.data[] | select(.message | test(\"PSU2 Temp\"))] | length >= 1' >/dev/null"
expect "Lenovo Sensors collection" sh -c "\"$BIN\" --config $WORK/targets.yaml sensors sr650 | grep -q 'Fan 1 Front Tach'"
expect "IPMI sensors with M factor" sh -c "\"$BIN\" --config $WORK/targets.yaml sensors sim | grep -q '2400 RPM'"
expect "Dell firmware hides Previous-/Available-" test "$(R --json firmware r750 | jq '.data | length')" -eq 5
expect "boot order with display names" sh -c "\"$BIN\" --config $WORK/targets.yaml boot r750 | grep -q 'PXE Device 1'"

# --- storage (read-only: no --allow, nothing audited, nothing written)
audit_before=$( [ -f "$WORK/audit.jsonl" ] && wc -l <"$WORK/audit.jsonl" || echo 0 )
set +e; R --json storage r750 dl380 sr650 x11 dmtf ilo4 >"$WORK/storage.json"; cs=$?; set -e
expect "storage exits 1 when a drive or volume is not OK" test $cs -eq 1
sj() { jq -r --arg t "$1" ".[] | select(.target==\$t) | .data | $2" "$WORK/storage.json"; }
expect "Dell PERC failed drive (RAID.SL id, RaidStatus Failed)" test "$(sj r750 '.drives[] | select(.raid_status=="Failed") | .id')" = "Disk.Bay.2:Enclosure.Internal.0-1:RAID.SL.3-1"
expect "Dell degraded RAID5 volume" test "$(sj r750 '.volumes[] | select(.name=="pgdata") | .health + " " + .raid_type + " " + .raid_status')" = "Warning RAID5 Degraded"
expect "Dell empty AHCI subsystem skipped" test "$(sj r750 '.controllers | length')" -eq 1
expect "HPE iLO 5 worn SSD and predicted failure are warnings" test "$(sj dl380 '[.drives[] | select(.health=="Warning")] | length')" -eq 2
expect "HPE iLO 4 Smart Array from the OEM SmartStorage tree" test "$(sj ilo4 '.source + " " + .health + " " + (.volumes | map(.raid_type) | join(","))')" = "SmartStorage Critical RAID1,RAID5"
expect "Lenovo XCC storage OK" test "$(sj sr650 '.health + " " + (.drives | length | tostring)')" = "OK 2"
expect "Supermicro rebuild progress and VolumeType-only RAID level" test "$(sj x11 '.volumes[0] | .raid_type + " " + .operation')" = "RAID5/6 Rebuild 37%"
expect "DMTF mockup falls back to SimpleStorage" test "$(sj dmtf '.source + " " + (.drives[] | select(.name=="SATA Bay 2") | .health)')" = "SimpleStorage Warning"
R storage sr650 >/dev/null && ok "storage exits 0 for a healthy box"
set +e; R storage sim >"$WORK/storage-ipmi.txt" 2>&1; c_si=$?; set -e
expect "IPMI-only target: storage needs Redfish, exit 2, nothing invented" sh -c "test $c_si -eq 2 && grep -q 'needs Redfish' \"$WORK/storage-ipmi.txt\""
expect "health carries storage problems" test "$(jq -r '.[] | select(.target=="r750") | [.data.problems[] | select(.component=="drive Physical Disk 0:1:2")] | length' "$WORK/health.json")" -eq 1
expect "health on an IPMI target says storage was not checked" sh -c "jq -e '.[] | select(.target==\"sim\") | .data.notes | any(startswith(\"storage not checked\"))' \"$WORK/health.json\" >/dev/null"
R storage --problems r750 >"$WORK/storage-problems.txt" || true
expect "storage --problems hides healthy drives" sh -c "grep -q 'Physical Disk 0:1:2' \"$WORK/storage-problems.txt\" && ! grep -q 'Physical Disk 0:1:0' \"$WORK/storage-problems.txt\""
audit_after=$( [ -f "$WORK/audit.jsonl" ] && wc -l <"$WORK/audit.jsonl" || echo 0 )
expect "storage reads need no --allow and write nothing to the audit log" test "$audit_before" -eq "$audit_after"

# --- dangerous operations
set +e
R power dmtf force-off --yes dmtf >/dev/null 2>&1; c1=$?
R --allow power power sr650 on --yes sr650 >/dev/null 2>&1; c2=$?
R --allow power power dmtf force-off </dev/null >/dev/null 2>&1; c3=$?
R --allow power power dmtf force-off --yes r750 >/dev/null 2>&1; c4=$?
set -e
expect "refused without --allow" test $c1 -eq 3
expect "refused when the target's allow: lacks the class" test $c2 -eq 3
expect "refused without confirmation" test $c3 -eq 3
expect "refused when --yes names another target" test $c4 -eq 3
R --allow power --json power dmtf force-off --yes dmtf | jq -e '.status=="done"' >/dev/null && ok "power action against DMTF mockup"
R --allow boot boot r750 --override pxe --yes r750 >/dev/null 2>&1
expect "boot override PATCH persisted by the mockup server" sh -c "\"$BIN\" --config $WORK/targets.yaml --json boot r750 | jq -e '.data.override_target==\"Pxe\" and .data.override_enabled==\"Once\"' >/dev/null"
R --allow virtual-media --dry-run vmedia dl380 insert http://10.0.0.1/u.iso 2>"$WORK/vm.txt" >/dev/null
expect "iLO virtual media uses the OEM action" grep -q HpeiLOVirtualMedia.InsertVirtualMedia "$WORK/vm.txt"
R --allow all --json firmware-update dmtf http://10.0.0.1/bios.bin --yes dmtf | jq -e '.status=="done"' >/dev/null && ok "SimpleUpdate against DMTF mockup"
set +e; R --allow all firmware-update x11 http://10.0.0.1/bmc.bin --yes x11 >/dev/null 2>&1; c5=$?; set -e
expect "unsupported operation is reported, not attempted" test $c5 -eq 3 -o $c5 -eq 2

# --- tasks and jobs (read-only; the mockup server serves each task's final state)
R task r750 >"$WORK/tasks.txt"
expect "Dell job queue + TaskService, running first" sh -c "sed -n 3p \"$WORK/tasks.txt\" | grep -q '^JID_003 .*Scheduled'"
set +e
R task r750 JID_002 --wait >"$WORK/t-fail.txt" 2>&1; c6=$?
R task r750 JID_003 --wait >"$WORK/t-staged.txt" 2>&1; c7=$?
R task sr650 2 --wait >/dev/null 2>&1; c8=$?
R task x11 >/dev/null 2>&1; c9=$?
R task sim >"$WORK/t-ipmi.txt" 2>&1; c10=$?
R task r750 JID_404 >"$WORK/t-404.txt" 2>&1; c11=$?
set -e
expect "failed Dell job exits 4 and says why" sh -c "test $c6 -eq 4 && grep -q 'FAILED  r750: JID_002 failed .*signature' \"$WORK/t-fail.txt\""
expect "Dell job scheduled for the next reboot is STAGED, exit 0" sh -c "test $c7 -eq 0 && grep -q 'STAGED  r750: JID_003' \"$WORK/t-staged.txt\""
expect "Lenovo task in Exception exits 4" test $c8 -eq 4
expect "BMC without a TaskService: unsupported, exit 2" test $c9 -eq 2
expect "IPMI-only target: tasks need Redfish" sh -c "test $c10 -eq 2 && grep -q 'need Redfish' \"$WORK/t-ipmi.txt\""
expect "unknown task id is a clear error" sh -c "test $c11 -eq 2 && grep -q 'list them with' \"$WORK/t-404.txt\""
expect "Lenovo task monitor resolves to its Task" test "$(R --json task sr650 /redfish/v1/TaskService/TaskMonitors/1 --wait | jq -r '.verdict + " " + .task.uri')" = "completed /redfish/v1/TaskService/Tasks/1"
expect "DMTF task 545 completed with its warning message" test "$(R --json task dmtf 545 | jq -r '.data.phase + " " + .data.health')" = "completed Warning"
expect "watching needs no --allow and writes nothing to the audit log" test "$(grep -c '"phase":"task"' "$WORK/audit.jsonl" || true)" -eq 0
R --allow firmware --json firmware-update dmtf http://10.0.0.1/bios.bin --yes dmtf --wait >"$WORK/fw-wait.json"
expect "firmware-update --wait without a returned task: verdict no-task" test "$(jq -r '.task.verdict' "$WORK/fw-wait.json")" = "no-task"
expect "the watched outcome is audited (phase task)" grep -q '"phase":"task".*"verdict":"no-task"' "$WORK/audit.jsonl"
set +e; R firmware-update dmtf http://10.0.0.1/bios.bin --yes dmtf --wait >/dev/null 2>&1; c12=$?; set -e
expect "--wait does not open the firmware gate" test $c12 -eq 3

R --allow power power sim force-off --yes sim >/dev/null
expect "IPMI chassis power off" test "$(R --json power sim | jq -r '.[0].data')" = "Off"
R --allow power power sim on --yes sim >/dev/null
expect "IPMI chassis power on" test "$(R --json power sim | jq -r '.[0].data')" = "On"
expect "audit log records plan + execution" test "$(grep -c '"phase":"executed"' "$WORK/audit.jsonl")" -ge 4

# --- MCP over stdio
timeout 400 python3 -u test/mcp_smoke.py "$BIN" "$WORK/targets.yaml" || fail "MCP smoke test"
echo "integration: $pass checks passed"
