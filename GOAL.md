# Goal

**One safe, vendor-neutral way for a person or an AI agent to read and operate server BMCs: Redfish on Dell iDRAC, HPE iLO, Lenovo XCC, Supermicro and any DMTF-conformant BMC, with IPMI as the fallback. Anything that can take a machine down stays off until a human opts in and confirms it.**

In scope:
- Read: inventory, health rollup, sensors, event logs (SEL / IML / Lifecycle / PlatformLog), power state, firmware versions, boot order. Normalised across vendors, the same shape over IPMI.
- Dangerous: power actions, BMC reset, boot override, firmware update by URL, virtual media. Off by default. Each one needs a runtime `--allow`, a per-target `allow:`, a single named target, an explicit confirmation (typed name or a single-use MCP token bound to the exact request), and an audit record.
- Vendor OEM differences that change what a correct answer is: which log is the SEL, where the health rollup lives, which reset types the BMC accepts, OEM-only actions, ETag rules.
- A single static binary that is both a CLI and an MCP server.

Out of scope:
- BIOS/BMC settings management, RAID configuration, user/account management on the BMC, KVM or serial-over-LAN consoles, OS provisioning, fleet inventory databases, monitoring daemons or exporters. Other tools do these well.
- Fan-out of dangerous operations across many machines. That is a deliberate limit.
