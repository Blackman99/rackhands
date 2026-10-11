#!/bin/sh
# chassis_control helper for ipmi_sim: keeps the simulated power state in a file.
state="${RACKHANDS_SIM_STATE:-/tmp/rackhands-sim-power}"
shift # MC address
op="$1"; shift
case "$op" in
  get)
    for p in "$@"; do
      case "$p" in
        power) echo "power:$(cat "$state" 2>/dev/null || echo 1)" ;;
        boot) echo "boot:default" ;;
      esac
    done ;;
  set)
    while [ $# -ge 2 ]; do
      case "$1" in power) echo "$2" > "$state" ;; esac
      shift 2
    done ;;
  check) ;;
esac
