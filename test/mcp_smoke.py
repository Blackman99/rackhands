#!/usr/bin/env python3
"""Drive `rackhands mcp` over stdio like an agent would (JSON-RPC, one
message per line) and check the two-step confirmation end to end."""
import json, queue, subprocess, sys, threading, time

binary, config = sys.argv[1], sys.argv[2]


class Session:
    def __init__(self, *extra):
        self.p = subprocess.Popen([binary, "--config", config, *extra, "mcp"], stdin=subprocess.PIPE, stdout=subprocess.PIPE, text=True)
        self.q = queue.Queue()
        threading.Thread(target=lambda: [self.q.put(l) for l in self.p.stdout], daemon=True).start()
        self.n = 0
        self.rpc("initialize", {"protocolVersion": "2025-06-18", "capabilities": {}, "clientInfo": {"name": "ci", "version": "0"}})
        self.send({"jsonrpc": "2.0", "method": "notifications/initialized"})

    def send(self, msg):
        self.p.stdin.write(json.dumps(msg) + "\n")
        self.p.stdin.flush()

    def rpc(self, method, params):
        self.n += 1
        self.send({"jsonrpc": "2.0", "id": self.n, "method": method, "params": params})
        deadline = time.time() + 120
        while True:
            try:
                line = self.q.get(timeout=max(0.1, deadline - time.time()))
            except queue.Empty:
                self.p.kill()
                raise SystemExit(f"FAIL {method} {json.dumps(params)[:200]}: no answer from the MCP server within 120s")
            msg = json.loads(line)
            if msg.get("id") == self.n:
                if "error" in msg:
                    raise SystemExit(f"{method}: {msg['error']}")
                return msg["result"]

    def tool(self, name, args):
        r = self.rpc("tools/call", {"name": name, "arguments": args})
        return r.get("isError", False), r.get("structuredContent"), "".join(c.get("text", "") for c in r.get("content", []))

    def close(self):
        self.p.stdin.close()
        self.p.wait(timeout=10)


def check(cond, what):
    print(("ok   " if cond else "FAIL ") + what, flush=True)
    if not cond:
        sys.exit(1)


s = Session()
names = {t["name"] for t in s.rpc("tools/list", {})["tools"]}
check("get_health" in names and "power_action" not in names, "read-only by default: no dangerous tools listed")
check({"list_tasks", "get_task"} <= names and "update_firmware" not in names, "task tools are read-only and listed without --allow")
err, out, _ = s.tool("list_tasks", {"target": "r750"})
check(not err and [t["id"] for t in out["data"]["tasks"]][:1] == ["JID_003"], "list_tasks: Dell job queue, unfinished first")
err, out, _ = s.tool("get_task", {"target": "r750", "task_id": "JID_002", "wait_seconds": 30})
check(not err and out["verdict"] == "failed" and "signature" in out["task"]["message"], f"get_task waits and reports the failure {json.dumps(out)[:160]}")
err, out, _ = s.tool("get_health", {"targets": ["all"]})
overall = {r["target"]: (r.get("data") or {}).get("overall") for r in out["results"]}
check(overall.get("r750") == "Critical" and overall.get("sr650") == "OK", f"fleet health over MCP {overall}")
err, out, _ = s.tool("get_event_log", {"target": "dl380", "min_severity": "Critical"})
check(not err and "Fan Failure" in json.dumps(out), "iLO IML via MCP")
err, out, _ = s.tool("get_inventory", {"targets": ["x11", "sr650"]})
acct = {r["target"]: (r.get("data") or {}).get("account") or {} for r in out["results"]}
check(acct["x11"].get("role") == "Operator" and acct["x11"].get("warning") and acct["sr650"].get("role") == "ReadOnly"
      and not acct["sr650"].get("warning"), f"get_inventory reports account privilege and warns {acct}")
s.close()

s = Session("--allow", "firmware")
args = {"target": "dmtf", "image_uri": "http://10.0.0.1/bios.bin", "wait_seconds": 30}
err, plan, _ = s.tool("update_firmware", args)
check(not err and plan["status"] == "confirmation_required", "update_firmware with wait_seconds still plans first")
err, done, text = s.tool("update_firmware", {**args, "confirm_token": plan["confirm_token"]})
check(not err and done["status"] == "done" and done["task"]["verdict"] == "no-task", f"confirmed update_firmware + wait returns the task verdict: {text[:160]}")
s.close()

s = Session("--allow", "power")
names = {t["name"] for t in s.rpc("tools/list", {})["tools"]}
check("power_action" in names and "update_firmware" not in names, "--allow power registers only power_action")
err, plan, _ = s.tool("power_action", {"target": "dmtf", "action": "force-restart"})
check(not err and plan["status"] == "confirmation_required" and plan["confirm_token"], "first call returns a plan, not an action")
err, _, text = s.tool("power_action", {"target": "dmtf", "action": "force-off", "confirm_token": plan["confirm_token"]})
check(err and "does not match" in text, "token is bound to the exact request")
err, done, text = s.tool("power_action", {"target": "dmtf", "action": "force-restart", "confirm_token": plan["confirm_token"]})
check(not err and done["status"] == "done" and "204" in done["result"], f"confirmed call executes against the mockup server: {text[:120]}")
err, _, text = s.tool("power_action", {"target": "dmtf", "action": "force-restart", "confirm_token": plan["confirm_token"]})
check(err and "already used" in text, "token cannot be replayed")
err, a, _ = s.tool("power_action", {"target": "dmtf", "action": "force-restart"})
err2, b, _ = s.tool("power_action", {"target": "dmtf", "action": "force-restart"})
check(not err and not err2 and a["confirm_token"] != b["confirm_token"], "identical plans in the same second get distinct tokens")
ok = []
for tok in (b["confirm_token"], a["confirm_token"]):
    err, done, _ = s.tool("power_action", {"target": "dmtf", "action": "force-restart", "confirm_token": tok})
    ok.append(not err and done["status"] == "done")
check(all(ok), "both identical same-second plans can be confirmed once each")
err, _, text = s.tool("power_action", {"target": "dmtf", "action": "force-restart", "confirm_token": a["confirm_token"]})
check(err and "already used" in text, "each of them stays single-use")
err, _, text = s.tool("power_action", {"target": "sr650", "action": "on"})
check(err and "allow:" in text, "target without allow: power is refused")
s.close()
print("mcp smoke: all checks passed")
