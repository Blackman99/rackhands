#!/usr/bin/env bash
# End-to-end: the rackhands binary against
#   * the DMTF Redfish-Mockup-Server serving the DMTF public-rackmount1 mockup
#     and the Dell / HPE / Lenovo / Supermicro mockups in testdata/mockups
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
trap 'rc=$?; [ $rc -ne 0 ] && diag' ERR
expect() { local what=$1; shift; if "$@"; then ok "$what"; else fail "$what"; fi; }

# --- Redfish mockup servers (DMTF reference implementation of a BMC)
port=8400
for m in dell hpe lenovo supermicro; do
  port=$((port+1))
  python3 "$RMS_DIR/redfishMockupServer.py" -S -D "testdata/mockups/$m" -p $port >"$WORK/rms-$m.log" 2>&1 & PIDS+=($!)
done
python3 "$RMS_DIR/redfishMockupServer.py" -S -D "$RMS_DIR/public-rackmount1" -p 8405 >"$WORK/rms-dmtf.log" 2>&1 & PIDS+=($!)

# --- IPMI simulator
SIM="$WORK/ipmi"; mkdir -p "$SIM"
echo 64 >"$SIM/sensor1"; echo 117 >"$SIM/sensor2"; echo 48 >"$SIM/sensor3"
sed "s#@STATE@#$SIM#g" test/ipmi/sim.emu.in >"$SIM/sim.emu"
export RACKHANDS_SIM_STATE="$SIM/power"
(cd test/ipmi && exec ipmi_sim -c lan.conf -f "$SIM/sim.emu" -s "$SIM" -n) >"$SIM/log" 2>&1 & PIDS+=($!)

for p in 8401 8402 8403 8404 8405; do
  for _ in $(seq 50); do curl -sf "http://127.0.0.1:$p/redfish/v1/" >/dev/null && break; sleep 0.2; done
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
YAML
export BMC_PASSWORD=secret
R() { "$BIN" --config "$WORK/targets.yaml" "$@"; }

# --- reads, every vendor
R --json inventory >"$WORK/inv.json" || fail "inventory over the whole fleet"
expect "vendor detection" test "$(jq -r '[.[] | .data.vendor] | join(",")' "$WORK/inv.json")" = "Dell,HPE,Lenovo,Supermicro,Generic,Supermicro,Supermicro"
expect "DMTF mockup inventory" test "$(jq -r '.[] | select(.target=="dmtf") | .data.serial' "$WORK/inv.json")" = "437XR1138R2"
expect "IPMI FRU inventory" test "$(jq -r '.[] | select(.target=="sim") | .data.model' "$WORK/inv.json")" = "SYS-1029P-SIM"
expect "IPMI fallback when Redfish is down" test "$(jq -r '.[] | select(.target=="legacy") | .via' "$WORK/inv.json")" = "ipmi (fallback)"

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

R --allow power power sim force-off --yes sim >/dev/null
expect "IPMI chassis power off" test "$(R --json power sim | jq -r '.[0].data')" = "Off"
R --allow power power sim on --yes sim >/dev/null
expect "IPMI chassis power on" test "$(R --json power sim | jq -r '.[0].data')" = "On"
expect "audit log records plan + execution" test "$(grep -c '"phase":"executed"' "$WORK/audit.jsonl")" -ge 4

# --- MCP over stdio
python3 test/mcp_smoke.py "$BIN" "$WORK/targets.yaml"
echo "integration: $pass checks passed"
