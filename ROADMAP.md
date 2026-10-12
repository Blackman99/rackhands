# Roadmap

## v0.1 (done)
- Read commands and MCP tools across Dell iDRAC 8/9, HPE iLO 4/5/6, Lenovo XCC/XCC2, Supermicro X11+ and generic DMTF Redfish
- IPMI 2.0 (lanplus) backend: FRU inventory, threshold sensors, SEL, chassis power, boot device, BMC cold reset
- Automatic fallback Redfish → IPMI on unreachable / unsupported / 404 / 501 / 503, never on bad credentials
- Safety model: classes off by default, two keys (`--allow` + per-target `allow:`), single-target, typed / token confirmation, `--dry-run`, JSONL audit
- CI against the DMTF Redfish-Mockup-Server (DMTF + four vendor mockups) and OpenIPMI `ipmi_sim`

## v0.2
- [x] TLS trust-on-first-use pinning for self-signed BMC certificates (`tls_fingerprint:`) instead of `insecure`
- Task monitoring for firmware updates (poll the returned Task / Dell job until it completes)
- Storage health (controllers, drives, volumes) in `health`
- Homebrew tap and Scoop manifest

## v0.3
- Huawei iBMC, Inspur, ASRock Rack and OpenBMC vendor profiles (mockups first)
- `events --since` and SEL clearing as a new dangerous class
- Optional per-class approval hook (run a command, e.g. a change-ticket check, before executing)
