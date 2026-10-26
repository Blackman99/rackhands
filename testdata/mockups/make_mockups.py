#!/usr/bin/env python3
"""Generate vendor-flavoured Redfish mockups in DMTF Redfish-Mockup-Server
layout (serve with: redfishMockupServer.py -S -D testdata/mockups/<vendor>).

They reproduce the OEM shapes rackhands handles (resource paths, log service
ids, Oem health rollups, firmware inventory noise, OEM virtual media actions,
firmware tasks / iDRAC jobs / iLO flash state).
Values are invented; structure follows each vendor's public Redfish docs.

Resources that change over time (a task going Running -> Completed) also get
<path>/_steps/NN.json snapshots. The unit-test server (internal/testbmc) serves
them one per GET, then index.json; the DMTF Redfish-Mockup-Server only ever
serves index.json, i.e. the final state.
Regenerate with: python3 testdata/mockups/make_mockups.py
"""
import json, os, shutil

HERE = os.path.dirname(os.path.abspath(__file__))
V1 = "/redfish/v1"


def ref(p):
    return {"@odata.id": V1 + p}


class Mock:
    def __init__(self, name):
        self.root = os.path.join(HERE, name)
        shutil.rmtree(self.root, ignore_errors=True)

    def put(self, path, obj, typ=None, odata_id=None, steps=()):
        """Write path/index.json. odata_id overrides @odata.id (a task monitor
        answers with the Task it watches). steps are earlier snapshots of the
        same resource, written to path/_steps/NN.json."""
        def fix(o):
            o = dict(o)
            o["@odata.id"] = odata_id or (V1 + path if path else V1 + "/")
            if typ:
                o["@odata.type"] = typ
            return o
        d = os.path.join(self.root, path.strip("/"))
        os.makedirs(d, exist_ok=True)
        with open(os.path.join(d, "index.json"), "w") as f:
            json.dump(fix(obj), f, indent=2, sort_keys=True)
            f.write("\n")
        if steps:
            os.makedirs(os.path.join(d, "_steps"), exist_ok=True)
            for i, st in enumerate(steps):
                with open(os.path.join(d, "_steps", "%02d.json" % (i + 1)), "w") as f:
                    json.dump(fix(st), f, indent=2, sort_keys=True)
                    f.write("\n")

    def coll(self, path, members, name, typ="#Collection"):
        self.put(path, {"Name": name, "Members": [ref(m) for m in members], "Members@odata.count": len(members)}, typ)


def status(h="OK", state="Enabled", rollup=None):
    s = {"Health": h, "State": state}
    if rollup:
        s["HealthRollup"] = rollup
    return s


def reset_action(path, allowed, name="ComputerSystem.Reset"):
    return {"#" + name: {"target": V1 + path + "/Actions/" + name, "ResetType@Redfish.AllowableValues": allowed}}


BOOT_TARGETS = ["None", "Pxe", "Cd", "Usb", "Hdd", "BiosSetup", "Utilities", "UefiTarget", "UefiHttp"]


def common_root(m, vendor, product, oem, extra=None):
    root = {"Name": "Root Service", "RedfishVersion": "1.17.0", "Vendor": vendor, "Product": product,
            "Systems": ref("/Systems"), "Chassis": ref("/Chassis"), "Managers": ref("/Managers"),
            "UpdateService": ref("/UpdateService"), "SessionService": ref("/SessionService"),
            "AccountService": ref("/AccountService"), "Oem": oem}
    root.update(extra or {})
    m.put("", root, "#ServiceRoot.v1_15_0.ServiceRoot")
    m.coll("/SessionService/Sessions", [], "Sessions", "#SessionCollection.SessionCollection")
    m.put("/SessionService", {"Name": "Session Service", "Sessions": ref("/SessionService/Sessions")}, "#SessionService.v1_1_8.SessionService")


STD_ROLES = {
    "Administrator": ["Login", "ConfigureManager", "ConfigureUsers", "ConfigureSelf", "ConfigureComponents"],
    "Operator": ["Login", "ConfigureSelf", "ConfigureComponents"],
    "ReadOnly": ["Login", "ConfigureSelf"],
}


def accounts(m, accts, roles=None, account_path="/AccountService/Accounts"):
    """AccountService with accounts [(id, username, role_id or None, oem or None)].
    Roles default to the three standard Redfish roles; extra/custom roles can be
    passed as {id: [privileges]}. rackhands only ever reads these."""
    rs = dict(STD_ROLES)
    rs.update(roles or {})
    members = []
    for aid, user, role, oem in accts:
        a = {"Id": aid, "Name": "User Account", "UserName": user, "Enabled": bool(user), "Locked": False}
        if role is not None:
            a["RoleId"] = role
            a["Links"] = {"Role": ref("/AccountService/Roles/" + role)}
        if oem:
            a["Oem"] = oem
        m.put(account_path + "/" + aid, a, "#ManagerAccount.v1_10_0.ManagerAccount")
        members.append(account_path + "/" + aid)
    m.coll(account_path, members, "Accounts", "#ManagerAccountCollection.ManagerAccountCollection")
    for rid, privs in rs.items():
        m.put("/AccountService/Roles/" + rid, {"Id": rid, "Name": rid, "RoleId": rid, "AssignedPrivileges": privs,
                                              "IsPredefined": rid in STD_ROLES}, "#Role.v1_3_1.Role")
    m.coll("/AccountService/Roles", ["/AccountService/Roles/" + r for r in rs], "Roles", "#RoleCollection.RoleCollection")
    m.put("/AccountService", {"Name": "Account Service", "Accounts": ref(account_path), "Roles": ref("/AccountService/Roles")},
          "#AccountService.v1_12_0.AccountService")


def log_entries(m, path, entries, inline=True):
    members = []
    for e in entries:
        e = dict(e)
        e.setdefault("EntryType", "SEL")
        p = path + "/Entries/" + e["Id"]
        e["@odata.id"] = V1 + p
        e["@odata.type"] = "#LogEntry.v1_15_0.LogEntry"
        m.put(p, e, "#LogEntry.v1_15_0.LogEntry")
        members.append(e if inline else {"@odata.id": V1 + p})
    m.put(path + "/Entries", {"Name": "Log Entries", "Members": members, "Members@odata.count": len(members)}, "#LogEntryCollection.LogEntryCollection")


def firmware(m, items, simple_update=True, oem=None, oem_steps=()):
    """oem / oem_steps: UpdateService Oem block (final state and earlier snapshots)."""
    ids = [i["Id"] for i in items]
    for i in items:
        m.put("/UpdateService/FirmwareInventory/" + i["Id"], i, "#SoftwareInventory.v1_9_0.SoftwareInventory")
    m.coll("/UpdateService/FirmwareInventory", ["/UpdateService/FirmwareInventory/" + x for x in ids], "Firmware Inventory")
    us = {"Name": "Update Service", "FirmwareInventory": ref("/UpdateService/FirmwareInventory"), "ServiceEnabled": True}
    if simple_update:
        us["Actions"] = {"#UpdateService.SimpleUpdate": {"target": V1 + "/UpdateService/Actions/UpdateService.SimpleUpdate",
                                                         "TransferProtocol@Redfish.AllowableValues": ["HTTP", "HTTPS", "NFS", "CIFS"]}}

    def with_oem(o):
        u = dict(us)
        if o:
            u["Oem"] = o
        return u
    m.put("/UpdateService", with_oem(oem), "#UpdateService.v1_11_0.UpdateService", steps=[with_oem(o) for o in oem_steps])


TASK = "#Task.v1_7_3.Task"


def msg(text, mid, sev="OK"):
    return {"Message": text, "MessageId": mid, "MessageSeverity": sev, "Severity": sev}


def task(tid, state, status="OK", pct=None, messages=(), start="2026-10-19T02:10:00Z", end=None, name=None, oem=None):
    """A DMTF Task (DSP0268 Task.v1_7). TaskState is the lifecycle, TaskStatus the health."""
    t = {"Id": tid, "Name": name or "Task " + tid, "TaskState": state, "TaskStatus": status,
         "StartTime": start, "Messages": list(messages)}
    if pct is not None:
        t["PercentComplete"] = pct
    if end:
        t["EndTime"] = end
    if oem:
        t["Oem"] = oem
    return t


def task_service(m, tasks):
    """tasks: [(id, final, steps)]"""
    for tid, final, steps in tasks:
        m.put("/TaskService/Tasks/" + tid, final, TASK, steps=steps)
    m.coll("/TaskService/Tasks", ["/TaskService/Tasks/" + t[0] for t in tasks], "Task Collection", "#TaskCollection.TaskCollection")
    m.put("/TaskService", {"Id": "TaskService", "Name": "Task Service", "ServiceEnabled": True, "CompletedTaskOverWritePolicy": "Oldest",
                           "Status": status(), "Tasks": ref("/TaskService/Tasks")}, "#TaskService.v1_2_0.TaskService")


def dell_job(jid, state, pct, message, message_id, name, job_type="FirmwareUpdate", start="TIME_NOW", end="TIME_NA"):
    """DellJob as iDRAC 9 serves it: JobState is the truth, StartTime/EndTime
    are often the literals TIME_NOW / TIME_NA rather than timestamps."""
    return {"Id": jid, "Name": name, "JobState": state, "JobType": job_type, "PercentComplete": pct,
            "Message": message, "MessageId": message_id, "MessageArgs": [], "StartTime": start, "EndTime": end,
            "CompletionTime": None if state not in ("Completed", "Failed") else "2026-10-19T02:13:02"}


def thermal(m, path, temps, fans):
    m.put(path, {"Name": "Thermal", "Temperatures": temps, "Fans": fans}, "#Thermal.v1_7_0.Thermal")


def power(m, path, psus, volts, watts):
    m.put(path, {"Name": "Power", "PowerSupplies": psus, "Voltages": volts,
                 "PowerControl": [{"MemberId": "0", "Name": "System Power Control", "PowerConsumedWatts": watts}]}, "#Power.v1_7_1.Power")


def temp(name, c, crit, h="OK", warn=None, ctx="CPU"):
    t = {"Name": name, "ReadingCelsius": c, "UpperThresholdCritical": crit, "PhysicalContext": ctx, "Status": status(h)}
    if warn:
        t["UpperThresholdNonCritical"] = warn
    return t


def fan(name, rpm, h="OK", unit="RPM", key="Name"):
    return {key: name, "Reading": rpm, "ReadingUnits": unit, "PhysicalContext": "SystemBoard", "Status": status(h)}


def vm(m, path, mid, actions):
    obj = {"Id": mid, "Name": "Virtual " + mid, "MediaTypes": ["CD", "DVD"], "Image": None, "Inserted": False, "ConnectedVia": "NotConnected"}
    obj.update(actions)
    m.put(path + "/" + mid, obj, "#VirtualMedia.v1_6_0.VirtualMedia")


def std_vm_actions(p):
    return {"Actions": {"#VirtualMedia.InsertMedia": {"target": V1 + p + "/Actions/VirtualMedia.InsertMedia"},
                        "#VirtualMedia.EjectMedia": {"target": V1 + p + "/Actions/VirtualMedia.EjectMedia"}}}


TB, GB = 1000 ** 4, 1000 ** 3


def storage(m, sys_path, subsystems):
    """DMTF Storage under sys_path/Storage. Each subsystem is a dict:
    id, name, status, controllers [dict], inline (True: StorageControllers array,
    the shape iDRAC and Supermicro serve; False: a Controllers collection, like
    XCC2 and iLO 5), drives [(absolute path, body)] (some BMCs keep drives under
    /Chassis), volumes [(id, body)] (key absent: no Volumes link at all)."""
    members = []
    for sub in subsystems:
        sp = sys_path + "/Storage/" + sub["id"]
        drives = sub.get("drives", [])
        o = {"Id": sub["id"], "Name": sub["name"], "Status": sub.get("status", status()),
             "Drives": [ref(d) for d, _ in drives], "Drives@odata.count": len(drives)}
        if sub.get("inline", True):
            o["StorageControllers"] = [dict(c, **{"@odata.id": V1 + sp + "#/StorageControllers/%d" % i, "MemberId": str(i)})
                                       for i, c in enumerate(sub.get("controllers", []))]
        else:
            cids = []
            for i, c in enumerate(sub.get("controllers", [])):
                cid = c.get("Id", str(i))
                m.put(sp + "/Controllers/" + cid, dict(c, Id=cid), "#StorageController.v1_7_0.StorageController")
                cids.append(sp + "/Controllers/" + cid)
            m.coll(sp + "/Controllers", cids, "Storage Controllers", "#StorageControllerCollection.StorageControllerCollection")
            o["Controllers"] = ref(sp + "/Controllers")
        for dp, body in drives:
            m.put(dp, body, "#Drive.v1_17_0.Drive")
        if "volumes" in sub:
            vols = []
            for vid, body in sub["volumes"]:
                m.put(sp + "/Volumes/" + vid, dict(body, Id=body.get("Id", vid)), "#Volume.v1_9_0.Volume")
                vols.append(sp + "/Volumes/" + vid)
            m.coll(sp + "/Volumes", vols, "Volume Collection", "#VolumeCollection.VolumeCollection")
            o["Volumes"] = ref(sp + "/Volumes")
        o.update(sub.get("extra", {}))
        m.put(sp, o, "#Storage.v1_13_0.Storage")
        members.append(sp)
    m.coll(sys_path + "/Storage", members, "Storage Collection", "#StorageCollection.StorageCollection")


def drive(did, name, model, media, cap, serial, h="OK", life=None, predicted=False, proto="SAS", state="Enabled", extra=None):
    d = {"Id": did, "Name": name, "Model": model, "MediaType": media, "CapacityBytes": cap, "Protocol": proto,
         "SerialNumber": serial, "FailurePredicted": predicted, "Status": status(h, state)}
    if life is not None:
        d["PredictedMediaLifeLeftPercent"] = life
    d.update(extra or {})
    return d


# ---------------------------------------------------------------- Dell iDRAC 9
def dell():
    m = Mock("dell")
    S, C, M = "/Systems/System.Embedded.1", "/Chassis/System.Embedded.1", "/Managers/iDRAC.Embedded.1"
    common_root(m, "Dell", "Integrated Dell Remote Access Controller", {"Dell": {"ManagerMACAddress": "d0:8e:79:00:00:01", "ServiceTag": "7XQ2KF3"}},
                {"TaskService": ref("/TaskService")})
    m.coll("/Systems", [S], "Computer System Collection")
    m.coll("/Chassis", [C], "Chassis Collection")
    m.coll("/Managers", [M], "Manager Collection")
    m.put(S, {"Id": "System.Embedded.1", "Name": "System", "Manufacturer": "Dell Inc.", "Model": "PowerEdge R750", "SKU": "7XQ2KF3",
              "SerialNumber": "CNWS3001A0012", "BiosVersion": "1.13.2", "HostName": "db-01", "PowerState": "On",
              "Status": status("Critical", rollup="Critical"),
              "ProcessorSummary": {"Count": 2, "Model": "Intel(R) Xeon(R) Gold 6338 CPU @ 2.00GHz", "Status": status(rollup="OK")},
              "MemorySummary": {"TotalSystemMemoryGiB": 512, "Status": status(rollup="OK")},
              "Boot": {"BootOrder": ["Boot0003", "Boot0004", "Boot0001"], "BootOptions": ref(S + "/BootOptions"),
                       "BootSourceOverrideEnabled": "Disabled", "BootSourceOverrideTarget": "None", "BootSourceOverrideMode": "UEFI",
                       "BootSourceOverrideTarget@Redfish.AllowableValues": BOOT_TARGETS + ["SDCard", "UefiShell"]},
              "Actions": reset_action(S, ["On", "ForceOff", "ForceRestart", "GracefulRestart", "GracefulShutdown", "PushPowerButton", "Nmi", "PowerCycle"]),
              "Links": {"Chassis": [ref(C)], "ManagedBy": [ref(M)]},
              "Processors": ref(S + "/Processors"), "Memory": ref(S + "/Memory"), "Storage": ref(S + "/Storage"),
              "Oem": {"Dell": {"DellSystem": {"@odata.type": "#DellSystem.v1_4_0.DellSystem", "PSRollupStatus": "Critical", "FanRollupStatus": "OK",
                                              "TempRollupStatus": "OK", "VoltRollupStatus": "OK", "CPURollupStatus": "OK", "StorageRollupStatus": "Critical",
                                              "BatteryRollupStatus": "OK", "IntrusionRollupStatus": "OK", "SELRollupStatus": "Critical",
                                              "CurrentRollupStatus": "OK", "LicensingRollupStatus": "OK"}}}},
          "#ComputerSystem.v1_20_0.ComputerSystem")
    m.coll(S + "/Processors", [], "Processors")
    m.coll(S + "/Memory", [], "Memory")
    # iDRAC 9: Storage members are FQDDs (RAID.SL.3-1 = PERC in slot 3, AHCI.Embedded.2-1 = chipset SATA with nothing
    # behind it). Controllers come inline (StorageControllers). Drive Ids are Disk.Bay.N:Enclosure.Internal.0-1:RAID.SL.3-1;
    # the colons are dropped from the mockup *paths* only, so the repository still checks out on Windows.
    # Oem.Dell.DellPhysicalDisk.RaidStatus / DellVolume.RaidStatus carry the controller's own word (Failed, Degraded).
    R = "RAID.SL.3-1"
    def dd(bay, media, cap, model, serial, h="OK", life=None, raid="Online", **kw):
        did = "Disk.Bay.%d:Enclosure.Internal.0-1:%s" % (bay, R)
        return (S + "/Storage/%s/Drives/Disk.Bay.%d" % (R, bay),
                drive(did, "Physical Disk 0:1:%d" % bay, model, media, cap, serial, h, life,
                      extra={"Oem": {"Dell": {"DellPhysicalDisk": {"RaidStatus": raid}}},
                             "PhysicalLocation": {"PartLocation": {"LocationOrdinalValue": bay, "LocationType": "Bay"}}}, **kw))
    drives = [dd(0, "SSD", 960 * GB, "MZ7L3960HCJR0D3", "S6KMNA0T401177", life=97),
              dd(1, "SSD", 960 * GB, "MZ7L3960HCJR0D3", "S6KMNA0T401192", life=96),
              dd(2, "HDD", 2400 * GB, "ST2400MM0129", "WBN0K2L7", h="Critical", raid="Failed"),
              dd(3, "HDD", 2400 * GB, "ST2400MM0129", "WBN0K3A1"),
              dd(4, "HDD", 2400 * GB, "ST2400MM0129", "WBN0K3C9")]
    def dv(n, name, raid, vtype, cap, bays, h="OK", rs="Online"):
        return ("Disk.Virtual.%d" % n, {"Id": "Disk.Virtual.%d:%s" % (n, R), "Name": name, "RAIDType": raid, "VolumeType": vtype,
                "CapacityBytes": cap, "Status": status(h), "Encrypted": False,
                "Links": {"Drives": [ref(drives[b][0]) for b in bays]},
                "Oem": {"Dell": {"DellVolume": {"RaidStatus": rs}}}})
    storage(m, S, [
        {"id": R, "name": "PERC H755 Front", "status": status("OK", rollup="Critical"),
         "controllers": [{"Name": "PERC H755 Front", "Model": "PERC H755 Front", "FirmwareVersion": "52.21.0-4606",
                          "Manufacturer": "DELL", "Status": status("OK", rollup="Critical"), "SpeedGbps": 12}],
         "drives": drives,
         "volumes": [dv(0, "os", "RAID1", "Mirrored", 960 * GB, [0, 1]),
                     dv(1, "pgdata", "RAID5", "StripedWithParity", 4800 * GB, [2, 3, 4], "Warning", "Degraded")]},
        {"id": "AHCI.Embedded.2-1", "name": "C620 Series Chipset SATA Controller", "status": status(),
         "controllers": [{"Name": "C620 Series Chipset SATA Controller", "FirmwareVersion": "", "Status": status()}],
         "volumes": []},
    ])
    opts = {"Boot0003": "Integrated RAID Controller 1: ubuntu", "Boot0004": "PXE Device 1: Embedded NIC 1 Port 1 Partition 1", "Boot0001": "Embedded SATA Port Optical Drive"}
    for k, v in opts.items():
        m.put(S + "/BootOptions/" + k, {"Id": k, "BootOptionReference": k, "DisplayName": v, "Name": "Boot Option"}, "#BootOption.v1_0_4.BootOption")
    m.coll(S + "/BootOptions", [S + "/BootOptions/" + k for k in opts], "Boot Options")
    m.put(C, {"Id": "System.Embedded.1", "Name": "Computer System Chassis", "ChassisType": "RackMount", "SerialNumber": "CNWS3001A0012",
              "Status": status("Critical", rollup="Critical"), "Thermal": ref(C + "/Thermal"), "Power": ref(C + "/Power"),
              "Links": {"ComputerSystems": [ref(S)], "ManagedBy": [ref(M)]}}, "#Chassis.v1_21_0.Chassis")
    thermal(m, C + "/Thermal",
            [temp("CPU1 Temp", 52, 95, warn=90), temp("CPU2 Temp", 55, 95, warn=90), temp("System Board Inlet Temp", 24, 47, warn=42, ctx="Intake"),
             temp("System Board Exhaust Temp", 41, 75, warn=70, ctx="Exhaust")],
            [fan("System Board Fan%d%s" % (i, ab), 8400 + 120 * i) for i in range(1, 4) for ab in "AB"])
    power(m, C + "/Power",
          [{"MemberId": "PSU.Slot.1", "Name": "PS1 Status", "PowerInputWatts": 412, "LastPowerOutputWatts": 398, "Status": status()},
           {"MemberId": "PSU.Slot.2", "Name": "PS2 Status", "PowerInputWatts": 0, "LastPowerOutputWatts": 0, "Status": status("Critical")}],
          [{"Name": "PS1 Voltage 1", "ReadingVolts": 230, "Status": status()}, {"Name": "PS2 Voltage 2", "ReadingVolts": 0, "Status": status("Critical")}], 398)
    m.put(M, {"Id": "iDRAC.Embedded.1", "Name": "Manager", "Manufacturer": "Dell Inc.", "Model": "15G Monolithic", "FirmwareVersion": "7.00.00.172",
              "Status": status(), "LogServices": ref(M + "/LogServices"), "VirtualMedia": ref(M + "/VirtualMedia"),
              "Actions": reset_action(M, ["GracefulRestart"], "Manager.Reset")}, "#Manager.v1_17_0.Manager")
    m.coll(M + "/LogServices", [M + "/LogServices/Sel", M + "/LogServices/Lclog"], "Log Service Collection")
    m.put(M + "/LogServices/Sel", {"Id": "Sel", "Name": "SEL Log Service", "LogEntryType": "SEL", "Entries": ref(M + "/LogServices/Sel/Entries")}, "#LogService.v1_3_0.LogService")
    m.put(M + "/LogServices/Lclog", {"Id": "Lclog", "Name": "LifeCycle Controller Log Service", "LogEntryType": "Event", "Entries": ref(M + "/LogServices/Lclog/Entries")}, "#LogService.v1_3_0.LogService")
    log_entries(m, M + "/LogServices/Sel", [
        {"Id": "7", "Created": "2026-10-09T03:12:44-05:00", "Severity": "Critical", "MessageId": "PSU0003", "Message": "The power input for power supply 2 is lost.", "SensorType": "Power Supply"},
        {"Id": "6", "Created": "2026-10-09T03:12:41-05:00", "Severity": "Critical", "MessageId": "RDU0012", "Message": "Power supply redundancy is lost.", "SensorType": "Power Supply"},
        {"Id": "5", "Created": "2026-10-02T11:30:02-05:00", "Severity": "OK", "MessageId": "CPU0000", "Message": "Internal error has occurred check for additional logs.", "SensorType": "Processor"},
        {"Id": "4", "Created": "2026-09-28T08:00:00-05:00", "Severity": "OK", "MessageId": "SEL9901", "Message": "OEM software event.", "SensorType": "OEM"},
    ])
    log_entries(m, M + "/LogServices/Lclog", [
        {"Id": "1201", "Created": "2026-10-09T03:15:00-05:00", "Severity": "OK", "MessageId": "USR0030", "Message": "Successfully logged in using monitor, from 10.0.0.9 and REDFISH.", "EntryType": "Event"},
    ])
    vm(m, M + "/VirtualMedia", "CD", std_vm_actions(M + "/VirtualMedia/CD"))
    m.coll(M + "/VirtualMedia", [M + "/VirtualMedia/CD"], "Virtual Media Services")
    firmware(m, [
        {"Id": "Installed-25227-7.00.00.172__iDRAC.Embedded.1-1", "Name": "Integrated Dell Remote Access Controller", "Version": "7.00.00.172", "Updateable": True},
        {"Id": "Previous-25227-6.10.80.00__iDRAC.Embedded.1-1", "Name": "Integrated Dell Remote Access Controller", "Version": "6.10.80.00", "Updateable": True},
        {"Id": "Installed-159-1.13.2__BIOS.Setup.1-1", "Name": "BIOS", "Version": "1.13.2", "Updateable": True},
        {"Id": "Available-159-1.14.1__BIOS.Setup.1-1", "Name": "BIOS", "Version": "1.14.1", "Updateable": True},
        {"Id": "Installed-0-52.21.0-4606__RAID.SL.3-1", "Name": "PERC H755 Front", "Version": "52.21.0-4606", "Updateable": True},
        {"Id": "Installed-108255-22.31.6__NIC.Embedded.1-1-1", "Name": "Broadcom Gigabit Ethernet BCM5720", "Version": "22.31.6", "Updateable": True},
        {"Id": "Installed-0-00.3D.69__PSU.Slot.1", "Name": "PS1 Power Supply", "Version": "00.3D.69", "Updateable": True},
    ])
    # Firmware jobs. SimpleUpdate answers 202 + Location: /redfish/v1/TaskService/Tasks/JID_…; that Task
    # carries the DellJob under Oem.Dell, and JobState is the truth (TaskState stays "Running" while a BIOS
    # job sits "Scheduled" waiting for the host reboot). The iDRAC job queue is Managers/iDRAC.Embedded.1/Jobs.
    def dj(jid, state, pct, message, mid, name, **kw):
        return {"Dell": dict(dell_job(jid, state, pct, message, mid, name, **kw), **{"@odata.type": "#DellJob.v1_6_0.DellJob"})}
    idrac = "Firmware Update: iDRAC"
    task_service(m, [
        ("JID_001", task("JID_001", "Completed", pct=100, end="2026-10-19T02:13:02Z", name="Task",
                         messages=[msg("Job completed successfully.", "IDRAC.2.9.PR19")],
                         oem=dj("JID_001", "Completed", 100, "Job completed successfully.", "PR19", idrac)),
         [task("JID_001", "Running", pct=0, name="Task", messages=[msg("Downloading the iDRAC firmware package.", "IDRAC.2.9.RED052")],
               oem=dj("JID_001", "Downloading", 0, "Downloading the iDRAC firmware package.", "RED052", idrac)),
          task("JID_001", "Running", pct=40, name="Task", messages=[msg("Installing the iDRAC firmware package.", "IDRAC.2.9.RED053")],
               oem=dj("JID_001", "Running", 40, "Installing the iDRAC firmware package.", "RED053", idrac)),
          task("JID_001", "Running", pct=80, name="Task", messages=[msg("Installing the iDRAC firmware package.", "IDRAC.2.9.RED053")],
               oem=dj("JID_001", "Running", 80, "Installing the iDRAC firmware package.", "RED053", idrac))]),
        ("JID_002", task("JID_002", "Exception", "Critical", pct=0, end="2026-10-19T01:41:12Z", name="Task",
                         messages=[msg("Unable to verify Update Package signature.", "IDRAC.2.9.RED007", "Critical")],
                         oem=dj("JID_002", "Failed", 0, "Unable to verify Update Package signature.", "RED007", "Firmware Update: PERC H755 Front")), []),
        ("JID_003", task("JID_003", "Running", pct=0, name="Task", messages=[msg("Task successfully scheduled.", "IDRAC.2.9.JCP001")],
                         oem=dj("JID_003", "Scheduled", 0, "Task successfully scheduled.", "JCP001", "Firmware Update: BIOS")), []),
    ])
    # the queue lists oldest first, like iDRAC
    jobs = [dell_job("JID_001", "Completed", 100, "Job completed successfully.", "PR19", idrac),
            dell_job("JID_002", "Failed", 0, "Unable to verify Update Package signature.", "RED007", "Firmware Update: PERC H755 Front"),
            dell_job("JID_003", "Scheduled", 0, "Task successfully scheduled.", "JCP001", "Firmware Update: BIOS")]
    for j in jobs:
        m.put(M + "/Jobs/" + j["Id"], j, "#DellJob.v1_6_0.DellJob")
    m.coll(M + "/Jobs", [M + "/Jobs/" + j["Id"] for j in jobs], "JobQueue", "#JobCollection.JobCollection")
    # iDRAC: 16 fixed slots, slot 1 is disabled and nameless; integration logs in as "admin"
    accounts(m, [("1", "", "None", None), ("2", "root", "Administrator", None), ("3", "admin", "Administrator", None),
                 ("4", "agent", "ReadOnly", None)], roles={"None": []})
    return m


# -------------------------------------------------------------- HPE iLO 5
def hpe():
    m = Mock("hpe")
    S, C, M = "/Systems/1", "/Chassis/1", "/Managers/1"
    common_root(m, "HPE", "ProLiant DL380 Gen10 Plus",
                {"Hpe": {"@odata.type": "#HpeiLOServiceExt.v2_3_0.HpeiLOServiceExt",
                         "Manager": [{"ManagerType": "iLO 5", "ManagerFirmwareVersion": "3.08", "HostName": "ilo-web-07"}]}})
    m.coll("/Systems", [S], "Computer Systems")
    m.coll("/Chassis", [C], "Chassis")
    m.coll("/Managers", [M], "Managers")
    m.put(S, {"Id": "1", "Name": "Computer System", "Manufacturer": "HPE", "Model": "ProLiant DL380 Gen10 Plus", "SKU": "P05172-B21",
              "SerialNumber": "MXQ12345AB", "BiosVersion": "U46 v2.80 (01/19/2026)", "HostName": "web-07", "PowerState": "On",
              "Status": status("Warning", rollup="Warning"),
              "ProcessorSummary": {"Count": 2, "Model": "Intel(R) Xeon(R) Silver 4314 CPU @ 2.40GHz", "Status": status(rollup="OK")},
              "MemorySummary": {"TotalSystemMemoryGiB": 256, "Status": status(rollup="OK")},
              "Boot": {"BootOrder": ["Boot000A", "Boot0009", "Boot0012"], "BootOptions": ref(S + "/BootOptions"),
                       "BootSourceOverrideEnabled": "Disabled", "BootSourceOverrideTarget": "None", "BootSourceOverrideMode": "UEFI",
                       "BootSourceOverrideTarget@Redfish.AllowableValues": BOOT_TARGETS + ["UefiShell"]},
              "Actions": reset_action(S, ["On", "ForceOff", "GracefulShutdown", "ForceRestart", "Nmi", "PushPowerButton", "GracefulRestart"]),
              "Links": {"Chassis": [ref(C)], "ManagedBy": [ref(M)]},
              "LogServices": ref(S + "/LogServices"), "Storage": ref(S + "/Storage"),
              "Oem": {"Hpe": {"Links": {"SmartStorage": ref(S + "/SmartStorage")}, "AggregateHealthStatus": {
                  "BiosOrHardwareHealth": {"Status": {"Health": "OK"}}, "FanRedundancy": "Redundant",
                  "Fans": {"Status": {"Health": "Warning"}}, "Memory": {"Status": {"Health": "OK"}}, "Network": {"Status": {"Health": "OK"}},
                  "PowerSupplies": {"Status": {"Health": "OK"}}, "PowerSupplyRedundancy": "Redundant", "Processors": {"Status": {"Health": "OK"}},
                  "SmartStorageBattery": {"Status": {"Health": "OK"}}, "Storage": {"Status": {"Health": "Warning"}}, "Temperatures": {"Status": {"Health": "OK"}}}}}},
          "#ComputerSystem.v1_17_0.ComputerSystem")
    # iLO 5 (2.x+) with an MR controller: DMTF Storage, controllers as a collection, numeric drive ids.
    # SmartStorage (the iLO OEM tree for Smart Array) is still linked but empty on MR controllers.
    D = "DE00A000"
    hd = [(S + "/Storage/%s/Drives/%d" % (D, i), drive(str(i), "Drive %d" % i, model, media, cap, sn, life=life, predicted=pred,
           extra={"PhysicalLocation": {"PartLocation": {"ServiceLabel": "Slot=12:Port=1I:Box=1:Bay=%d" % (i + 1)}}}))
          for i, (model, media, cap, sn, life, pred) in enumerate([
              ("MO001600JWTBT", "SSD", 1600 * GB, "S5H0NA0R301442", 6, False),
              ("MO001600JWTBT", "SSD", 1600 * GB, "S5H0NA0R301459", 71, False),
              ("MB004000JWJTT", "HDD", 4 * TB, "ZC1A7K2Q", None, True),
              ("MB004000JWJTT", "HDD", 4 * TB, "ZC1A7K9M", None, False)])]
    storage(m, S, [{"id": D, "name": "HPE MR416i-a Gen10+", "inline": False, "status": status("OK", rollup="Warning"),
                    "controllers": [{"Id": "0", "Name": "HPE MR416i-a Gen10+", "Model": "HPE MR416i-a Gen10+",
                                     "FirmwareVersion": "52.24.3-4948", "Status": status(), "SerialNumber": "SKC4012345"}],
                    "drives": hd,
                    "volumes": [("239", {"Name": "Logical Drive 1", "RAIDType": "RAID1", "CapacityBytes": 1600 * GB, "Status": status(),
                                         "Links": {"Drives": [ref(hd[0][0]), ref(hd[1][0])]}}),
                                ("240", {"Name": "Logical Drive 2", "RAIDType": "RAID10", "CapacityBytes": 4 * TB, "Status": status(),
                                         "Links": {"Drives": [ref(hd[2][0]), ref(hd[3][0])]}})]}])
    m.put(S + "/SmartStorage", {"Id": "SmartStorage", "Name": "HpeSmartStorage", "Status": status(),
                                "Links": {"ArrayControllers": ref(S + "/SmartStorage/ArrayControllers")}}, "#HpeSmartStorage.v2_0_0.HpeSmartStorage")
    m.coll(S + "/SmartStorage/ArrayControllers", [], "HpeSmartStorageArrayControllers", "#HpeSmartStorageArrayControllerCollection.HpeSmartStorageArrayControllerCollection")
    opts = {"Boot000A": "HPE MR416i-a Gen10+ - Ubuntu", "Boot0009": "Embedded LOM 1 Port 1 : HPE Ethernet 1Gb 4-port 369i Adapter - NIC (PXE IPv4)", "Boot0012": "Generic USB Boot"}
    for k, v in opts.items():
        m.put(S + "/BootOptions/" + k, {"Id": k, "BootOptionReference": k, "DisplayName": v, "Name": "Boot Option"}, "#BootOption.v1_0_1.BootOption")
    m.coll(S + "/BootOptions", [S + "/BootOptions/" + k for k in opts], "Boot Options")
    m.coll(S + "/LogServices", [S + "/LogServices/IML"], "Log Service Collection")
    m.put(S + "/LogServices/IML", {"Id": "IML", "Name": "Integrated Management Log", "Entries": ref(S + "/LogServices/IML/Entries")}, "#LogService.v1_0_0.LogService")
    log_entries(m, S + "/LogServices/IML", [
        {"Id": "41", "Created": "2026-10-10T22:04:13Z", "Severity": "Warning", "Message": "System Fans Not Redundant", "EntryType": "Oem",
         "Oem": {"Hpe": {"Severity": "Caution", "Class": 3, "Code": 4}}},
        {"Id": "40", "Created": "2026-10-10T22:04:11Z", "Severity": "Critical", "Message": "Fan Failure (Fan 3, Location System)", "EntryType": "Oem",
         "Oem": {"Hpe": {"Severity": "Critical", "Class": 3, "Code": 3}}},
        {"Id": "39", "Created": "2026-10-01T09:00:00Z", "Severity": "OK", "Message": "Firmware flashed (iLO 5 3.08)", "EntryType": "Oem",
         "Oem": {"Hpe": {"Severity": "Informational", "Class": 32, "Code": 1}}},
        {"Id": "38", "Created": "2026-09-30T23:11:47Z", "Severity": "OK", "Message": "Server power restored.", "EntryType": "Oem",
         "Oem": {"Hpe": {"Severity": "Repaired", "Class": 1, "Code": 2}}},
    ], inline=False)
    m.put(C, {"Id": "1", "Name": "Computer System Chassis", "ChassisType": "RackMount", "SerialNumber": "MXQ12345AB", "Status": status("Warning"),
              "Thermal": ref(C + "/Thermal"), "Power": ref(C + "/Power"), "Links": {"ComputerSystems": [ref(S)], "ManagedBy": [ref(M)]}}, "#Chassis.v1_10_0.Chassis")
    thermal(m, C + "/Thermal",
            [temp("01-Inlet Ambient", 22, 42, warn=42, ctx="Intake"), temp("02-CPU 1", 40, 0), temp("03-CPU 2", 40, 0),
             {"Name": "04-P1 DIMM 1-6", "PhysicalContext": "SystemMemory", "Status": {"State": "Absent"}}],
            [fan("Fan %d" % i, 0 if i == 3 else 34, "Critical" if i == 3 else "OK", "Percent") for i in range(1, 7)])
    power(m, C + "/Power",
          [{"MemberId": "0", "Name": "HpeServerPowerSupply", "LastPowerOutputWatts": 188, "Status": status()},
           {"MemberId": "1", "Name": "HpeServerPowerSupply", "LastPowerOutputWatts": 176, "Status": status()}], [], 364)
    p = M + "/VirtualMedia/2"
    m.put(M, {"Id": "1", "Name": "Manager", "Manufacturer": "HPE", "Model": "iLO 5", "FirmwareVersion": "iLO 5 v3.08", "Status": status(),
              "LogServices": ref(M + "/LogServices"), "VirtualMedia": ref(M + "/VirtualMedia"),
              "Actions": {"#Manager.Reset": {"target": V1 + M + "/Actions/Manager.Reset"}}}, "#Manager.v1_5_1.Manager")
    m.coll(M + "/LogServices", [M + "/LogServices/IEL"], "Log Service Collection")
    m.put(M + "/LogServices/IEL", {"Id": "IEL", "Name": "iLO Event Log", "Entries": ref(M + "/LogServices/IEL/Entries")}, "#LogService.v1_0_0.LogService")
    log_entries(m, M + "/LogServices/IEL", [
        {"Id": "1", "Created": "2026-10-10T22:05:00Z", "Severity": "OK", "Message": "Browser login: admin - 10.0.0.9", "EntryType": "Oem"}])
    vm(m, M + "/VirtualMedia", "2", {"Oem": {"Hpe": {"Actions": {
        "#HpeiLOVirtualMedia.InsertVirtualMedia": {"target": V1 + p + "/Actions/Oem/Hpe/HpeiLOVirtualMedia.InsertVirtualMedia"},
        "#HpeiLOVirtualMedia.EjectVirtualMedia": {"target": V1 + p + "/Actions/Oem/Hpe/HpeiLOVirtualMedia.EjectVirtualMedia"}}}}})
    m.coll(M + "/VirtualMedia", [p], "Virtual Media Services")
    firmware(m, [
        {"Id": "1", "Name": "iLO 5", "Version": "3.08 Sep 03 2026", "Updateable": True},
        {"Id": "2", "Name": "System ROM", "Version": "U46 v2.80 (01/19/2026)", "Updateable": True},
        {"Id": "3", "Name": "Intelligent Platform Abstraction Data", "Version": "21.4.0 Build 37", "Updateable": True},
        {"Id": "4", "Name": "Server Platform Services (SPS) Firmware", "Version": "6.1.4.47", "Updateable": False},
        {"Id": "5", "Name": "HPE MR416i-a Gen10+", "Version": "52.24.3-4948", "Updateable": True},
    ],
        # iLO 5: SimpleUpdate returns no Task; the flash is followed through UpdateService Oem.Hpe.State and
        # FlashProgressPercent (Idle, Uploading, Verifying, Writing, Updating, Complete, Error).
        oem={"Hpe": {"@odata.type": "#HpeiLOUpdateServiceExt.v2_3_0.HpeiLOUpdateServiceExt", "State": "Complete", "FlashProgressPercent": 100,
                     "Result": {"MessageId": "Update.1.1.UpdateSuccessful", "MessageArgs": ["System ROM", "U46_2.90_02_20_2026.fwpkg"]}}},
        oem_steps=[{"Hpe": {"State": s_, "FlashProgressPercent": p_}} for s_, p_ in
                   (("Idle", 0), ("Uploading", 0), ("Verifying", 0), ("Writing", 35), ("Updating", 85))])
    # iLO 5: no RoleId on older firmware; privileges are Oem.Hpe.Privileges booleans
    def priv(power, cfg, users):
        return {"Hpe": {"LoginName": "", "Privileges": {"LoginPriv": True, "RemoteConsolePriv": cfg, "VirtualMediaPriv": cfg,
                "VirtualPowerAndResetPriv": power, "iLOConfigPriv": cfg, "UserConfigPriv": users, "HostBIOSConfigPriv": cfg}}}
    accounts(m, [("1", "Administrator", None, priv(True, True, True)), ("2", "admin", None, priv(True, True, True)),
                 ("3", "monitor", None, priv(False, False, False)), ("4", "ops", None, priv(True, False, False))])
    return m


# ----------------------------------------------------------- Lenovo XCC 2
def lenovo():
    m = Mock("lenovo")
    S, C, M = "/Systems/1", "/Chassis/1", "/Managers/1"
    common_root(m, "Lenovo", "ThinkSystem SR650 V2", {"Lenovo": {"@odata.type": "#LenovoServiceRoot.v1_0_0.LenovoServiceRoot", "Version": "XCC2"}},
                {"TaskService": ref("/TaskService")})
    m.coll("/Systems", [S], "Computer System Collection")
    m.coll("/Chassis", [C], "Chassis Collection")
    m.coll("/Managers", [M], "Manager Collection")
    m.put(S, {"Id": "1", "Name": "ComputerSystem", "Manufacturer": "Lenovo", "Model": "ThinkSystem SR650 V2 -[7Z73CTO1WW]-", "SKU": "7Z73CTO1WW",
              "SerialNumber": "J30AB123", "BiosVersion": "AFE128K-1.80", "HostName": "k8s-node-12", "PowerState": "On",
              "@odata.etag": "W/\"6a1b2c3d\"",
              "Status": status(rollup="OK"),
              "ProcessorSummary": {"Count": 2, "Model": "Intel(R) Xeon(R) Gold 5318Y CPU @ 2.10GHz", "Status": status(rollup="OK")},
              "MemorySummary": {"TotalSystemMemoryGiB": 384, "Status": status(rollup="OK")},
              "Boot": {"BootOrder": ["Boot0001", "Boot0002"], "BootOptions": ref(S + "/BootOptions"),
                       "BootSourceOverrideEnabled": "Disabled", "BootSourceOverrideTarget": "None", "BootSourceOverrideMode": "UEFI",
                       "BootSourceOverrideTarget@Redfish.AllowableValues": ["None", "Pxe", "Cd", "Usb", "Hdd", "BiosSetup", "Diags", "UefiTarget"]},
              "Actions": reset_action(S, ["On", "Nmi", "GracefulShutdown", "GracefulRestart", "ForceOn", "ForceOff", "ForceRestart"]),
              "Links": {"Chassis": [ref(C)], "ManagedBy": [ref(M)]}, "LogServices": ref(S + "/LogServices"),
              "Storage": ref(S + "/Storage")},
          "#ComputerSystem.v1_16_0.ComputerSystem")
    # XCC2: Storage per adapter slot (RAID_Slot3), Controllers collection, Disk.N drive ids, healthy and worn-in SSDs.
    L = "RAID_Slot3"
    ld = [(S + "/Storage/%s/Drives/Disk.%d" % (L, i), drive("Disk.%d" % i, "Drive %d" % i, "SSDSC2KG480G8L", "SSD", 480 * GB,
           sn, life=life, proto="SATA", extra={"PhysicalLocation": {"PartLocation": {"LocationOrdinalValue": i, "ServiceLabel": "Drive %d" % i}}}))
          for i, (sn, life) in enumerate([("PHYG1234016P480BGN", 88), ("PHYG1234022Q480BGN", 91)])]
    storage(m, S, [{"id": L, "name": "ThinkSystem RAID 940-8i 4GB Flash PCIe Gen4 12Gb Adapter", "inline": False, "status": status(rollup="OK"),
                    "controllers": [{"Id": "0", "Name": "RAID 940-8i", "Model": "ThinkSystem RAID 940-8i 4GB Flash PCIe Gen4 12Gb Adapter",
                                     "FirmwareVersion": "52.20.0-4523", "Status": status(rollup="OK")}],
                    "drives": ld,
                    "volumes": [("Volume0", {"Name": "boot", "RAIDType": "RAID1", "VolumeType": "Mirrored", "CapacityBytes": 479 * GB,
                                             "Status": status(), "Links": {"Drives": [ref(d[0]) for d in ld]}})]}])
    opts = {"Boot0001": "ubuntu (M.2 Bay 0)", "Boot0002": "Network 1: PXE IPv4"}
    for k, v in opts.items():
        m.put(S + "/BootOptions/" + k, {"Id": k, "BootOptionReference": k, "DisplayName": v, "Name": "Boot Option"}, "#BootOption.v1_0_3.BootOption")
    m.coll(S + "/BootOptions", [S + "/BootOptions/" + k for k in opts], "Boot Options")
    m.coll(S + "/LogServices", [S + "/LogServices/PlatformLog"], "Log Service Collection")
    m.put(S + "/LogServices/PlatformLog", {"Id": "PlatformLog", "Name": "Platform Event Log", "Entries": ref(S + "/LogServices/PlatformLog/Entries")}, "#LogService.v1_2_0.LogService")
    log_entries(m, S + "/LogServices/PlatformLog", [
        {"Id": "plat_12", "Created": "2026-10-08T14:20:00+00:00", "Severity": "OK", "MessageId": "FQXSPEM4041I", "Message": "The SMTP Server is not configured.", "EntryType": "Event"},
        {"Id": "plat_11", "Created": "2026-10-07T06:01:12+00:00", "Severity": "Warning", "MessageId": "FQXSPMA0025I",
         "MessageArgs": ["DIMM 14"], "EntryType": "Event"},
    ])
    m.put(C, {"Id": "1", "Name": "Chassis", "ChassisType": "RackMount", "SerialNumber": "J30AB123", "Status": status(rollup="OK"),
              "Sensors": ref(C + "/Sensors"), "Links": {"ComputerSystems": [ref(S)], "ManagedBy": [ref(M)]}}, "#Chassis.v1_15_0.Chassis")
    sensors = {
        "CPU1_Temp": {"Name": "CPU 1 Temp", "ReadingType": "Temperature", "Reading": 47, "ReadingUnits": "Cel", "Thresholds": {"UpperCritical": {"Reading": 98}}},
        "Ambient_Temp": {"Name": "Ambient Temp", "ReadingType": "Temperature", "Reading": 23, "ReadingUnits": "Cel", "Thresholds": {"UpperCritical": {"Reading": 47}}},
        "Fan1_Tach": {"Name": "Fan 1 Front Tach", "ReadingType": "Rotational", "Reading": 6720, "ReadingUnits": "RPM", "Thresholds": {"LowerCritical": {"Reading": 500}}},
        "PSU1_Power": {"Name": "PSU1 Input Power", "ReadingType": "Power", "Reading": 286, "ReadingUnits": "W"},
        "SysBrd_3V3": {"Name": "SysBrd 3.3V", "ReadingType": "Voltage", "Reading": 3.31, "ReadingUnits": "V", "Thresholds": {"UpperCritical": {"Reading": 3.6}, "LowerCritical": {"Reading": 2.97}}},
    }
    for k, v in sensors.items():
        v = dict(v, Id=k, Status=status())
        m.put(C + "/Sensors/" + k, v, "#Sensor.v1_6_0.Sensor")
    m.coll(C + "/Sensors", [C + "/Sensors/" + k for k in sensors], "Sensors")
    m.put(M, {"Id": "1", "Name": "Manager", "Manufacturer": "Lenovo", "Model": "Lenovo XClarity Controller 2", "FirmwareVersion": "UHC830D 2.10",
              "Status": status(), "LogServices": ref(M + "/LogServices"), "VirtualMedia": ref(M + "/VirtualMedia"),
              "Actions": reset_action(M, ["GracefulRestart", "ForceRestart"], "Manager.Reset")}, "#Manager.v1_14_0.Manager")
    m.coll(M + "/LogServices", [M + "/LogServices/AuditLog"], "Log Service Collection")
    m.put(M + "/LogServices/AuditLog", {"Id": "AuditLog", "Name": "Audit Log", "Entries": ref(M + "/LogServices/AuditLog/Entries")}, "#LogService.v1_2_0.LogService")
    log_entries(m, M + "/LogServices/AuditLog", [
        {"Id": "audit_88", "Created": "2026-10-08T14:22:00+00:00", "Severity": "OK", "MessageId": "FQXSPUN0018I", "Message": "Remote Login Successful. Login ID: monitor using Redfish", "EntryType": "Event"}])
    p = M + "/VirtualMedia/EXT1"
    vm(m, M + "/VirtualMedia", "EXT1", std_vm_actions(p))
    m.coll(M + "/VirtualMedia", [p], "Virtual Media")
    firmware(m, [
        {"Id": "BMC-Primary", "Name": "XCC Primary Firmware", "Version": "UHC830D 2.10", "Updateable": True},
        {"Id": "BMC-Backup", "Name": "XCC Backup Firmware", "Version": "UHC828E 1.92", "Updateable": True},
        {"Id": "UEFI", "Name": "UEFI", "Version": "AFE128K-1.80", "Updateable": True},
        {"Id": "LXPM", "Name": "LXPM Diagnostic Software", "Version": "PDL146M-3.30", "Updateable": True},
    ])
    # XCC follows DSP0266 to the letter: SimpleUpdate answers 202 + Location: a task monitor, which answers
    # 202 + the Task while it runs. Task 1 runs to Completed; task 2 ends in Exception with the reason.
    running = [task("1", "Running", pct=p_, messages=[msg("The task with Id '1' has started.", "TaskEvent.1.0.TaskStarted")]) for p_ in (5, 35, 80)]
    task_service(m, [
        ("1", task("1", "Completed", pct=100, end="2026-10-19T02:16:41Z",
                   messages=[msg("The task with Id '1' has started.", "TaskEvent.1.0.TaskStarted"),
                             msg("The target device 'UEFI' has been successfully updated with image 'lnvgy_fw_uefi_afe130k-1.90.uxz'.", "Update.1.1.UpdateSuccessful"),
                             msg("The task with Id '1' has completed.", "TaskEvent.1.0.TaskCompletedOK")]), running),
        ("2", task("2", "Exception", "Critical", pct=12, start="2026-10-18T22:02:10Z", end="2026-10-18T22:02:51Z",
                   messages=[msg("The task with Id '2' has started.", "TaskEvent.1.0.TaskStarted"),
                             msg("Verification of image 'lnvgy_fw_xcc_uhc832d-2.20.uxz' at 'BMC-Primary' failed.", "Update.1.1.VerificationFailed", "Critical"),
                             msg("The task with Id '2' has been aborted.", "TaskEvent.1.0.TaskAborted", "Critical")]), []),
    ])
    m.put("/TaskService/TaskMonitors/1", running[0], TASK, odata_id=V1 + "/TaskService/Tasks/1")
    # XCC: custom roles (CustomRoleN) carry their own privilege list; integration's "admin" is read-only here
    accounts(m, [("1", "USERID", "Administrator", None), ("2", "admin", "ReadOnly", None), ("3", "fanwatch", "CustomRole1", None)],
             roles={"CustomRole1": ["Login", "ConfigureComponents"]})
    return m


# ------------------------------------------------------- Supermicro X11
def supermicro():
    m = Mock("supermicro")
    S, C, M = "/Systems/1", "/Chassis/1", "/Managers/1"
    common_root(m, "Supermicro", "SYS-6019U-TR4", {"Supermicro": {"@odata.type": "#SmcServiceRoot.v1_0_0.SmcServiceRoot"}})
    m.coll("/Systems", [S], "Computer System Collection")
    m.coll("/Chassis", [C], "Chassis Collection")
    m.coll("/Managers", [M], "Manager Collection")
    m.put(S, {"Id": "1", "Name": "System", "Manufacturer": "Supermicro", "Model": "SYS-6019U-TR4", "SerialNumber": "S41234567890",
              "BiosVersion": "4.0", "PowerState": "On", "Status": status("Warning", rollup="Warning"),
              "ProcessorSummary": {"Count": 2, "Model": "Intel(R) Xeon(R) Gold 6230 CPU @ 2.10GHz", "Status": status(rollup="OK")},
              "MemorySummary": {"TotalSystemMemoryGiB": 192, "Status": status(rollup="OK")},
              "Boot": {"BootSourceOverrideEnabled": "Disabled", "BootSourceOverrideTarget": "None", "BootSourceOverrideMode": "Legacy",
                       "BootSourceOverrideTarget@Redfish.AllowableValues": ["None", "Pxe", "Hdd", "Cd", "BiosSetup", "Usb"]},
              "Actions": reset_action(S, ["On", "ForceOff", "GracefulShutdown", "GracefulRestart", "ForceRestart", "Nmi", "ForceOn"]),
              "Links": {"Chassis": [ref(C)], "ManagedBy": [ref(M)]}, "Storage": ref(S + "/Storage")},
          "#ComputerSystem.v1_5_1.ComputerSystem")
    # X11 (storage needs the SFT-DCMS-SINGLE licence on real boards): HA-RAID with inline controllers, drives under
    # /Chassis/1/Drives, volumes with only the old VolumeType (no RAIDType) and a rebuild in progress.
    sd = [("/Chassis/1/Drives/HA-RAID.0.Disk.%d" % i, drive("HA-RAID.0.Disk.%d" % i, "Disk.%d" % i, "HUS726T4TALA6L4", "HDD", 4 * TB, sn,
           proto="SATA", extra=extra))
          for i, (sn, extra) in enumerate([("V6G1A2BK", {}), ("V6G1A3CK", {}), ("V6G1A4DK", {}),
                                           ("V6G1B9XK", {"Operations": [{"OperationName": "Rebuild", "PercentageComplete": 37}]})])]
    storage(m, S, [{"id": "HA-RAID", "name": "HA-RAID", "status": status("Warning"),
                    "controllers": [{"Name": "AVAGO 3108 MegaRAID", "Model": "AVAGO 3108 MegaRAID", "FirmwareVersion": "4.680.00-8527",
                                     "Status": status()}],
                    "drives": sd,
                    "volumes": [("0", {"Name": "VD0", "VolumeType": "StripedWithParity", "CapacityBytes": 12 * TB, "Status": status("Warning"),
                                       "Operations": [{"OperationName": "Rebuild", "PercentageComplete": 37}],
                                       "Links": {"Drives": [ref(d[0]) for d in sd]}})]}])
    m.put(C, {"Id": "1", "Name": "Computer System Chassis", "ChassisType": "RackMount", "Status": status("Warning"),
              "Thermal": ref(C + "/Thermal"), "Power": ref(C + "/Power"), "Links": {"ComputerSystems": [ref(S)], "ManagedBy": [ref(M)]}}, "#Chassis.v1_5_1.Chassis")
    thermal(m, C + "/Thermal",
            [temp("CPU1 Temp", 92, 100, "Warning", warn=90), temp("CPU2 Temp", 61, 100, warn=90), temp("System Temp", 31, 85, warn=80, ctx="SystemBoard"),
             {"Name": "PCH Temp", "ReadingCelsius": 49, "UpperThresholdCritical": 95, "Status": {"State": "Enabled"}}],
            [fan("FAN%d" % i, 4200 + 100 * i) for i in range(1, 5)] + [{"Name": "FAN5", "Status": {"State": "Absent"}}])
    power(m, C + "/Power",
          [{"MemberId": "0", "Name": "Power Supply 1", "PowerInputWatts": 251, "Status": status()},
           {"MemberId": "1", "Name": "Power Supply 2", "PowerInputWatts": 247, "Status": status()}],
          [{"Name": "12V", "ReadingVolts": 12.06, "UpperThresholdCritical": 13.2, "LowerThresholdCritical": 10.8, "Status": status()},
           {"Name": "VBAT", "ReadingVolts": 2.71, "LowerThresholdCritical": 2.6, "Status": status("Warning")}], 498)
    m.put(M, {"Id": "1", "Name": "Manager", "Model": "ASPEED", "FirmwareVersion": "01.73.12", "Status": status(),
              "LogServices": ref(M + "/LogServices"), "VirtualMedia": ref(M + "/VirtualMedia"),
              "Actions": reset_action(M, ["GracefulRestart"], "Manager.Reset")}, "#Manager.v1_3_0.Manager")
    m.coll(M + "/LogServices", [M + "/LogServices/Log1"], "Log Service Collection")
    m.put(M + "/LogServices/Log1", {"Id": "Log1", "Name": "Log Service", "LogEntryType": "SEL", "Entries": ref(M + "/LogServices/Log1/Entries")}, "#LogService.v1_1_0.LogService")
    log_entries(m, M + "/LogServices/Log1", [
        {"Id": "1", "Created": "2026-10-05T10:00:00+00:00", "Severity": "OK", "Message": "[System Event] AC Power On - Assertion", "SensorType": "System Event"},
        {"Id": "2", "Created": "2026-10-11T02:44:19+00:00", "Severity": "Warning", "Message": "[Temperature] CPU1 Temp - Upper Non-critical - going high - Assertion", "SensorType": "Temperature"},
        {"Id": "3", "Created": "2026-10-11T03:02:51+00:00", "Severity": "Warning", "Message": "[Voltage] VBAT - Lower Non-critical - going low - Assertion", "SensorType": "Voltage"},
    ])
    p = M + "/VirtualMedia/CD1"
    vm(m, M + "/VirtualMedia", "CD1", {"Oem": {"Supermicro": {"VirtualMediaConfig": ref(M + "/VM1/CfgCD")}}})
    m.coll(M + "/VirtualMedia", [p], "Virtual Media")
    firmware(m, [
        {"Id": "BMC", "Name": "BMC Firmware", "Version": "01.73.12", "Updateable": True},
        {"Id": "BIOS", "Name": "BIOS", "Version": "4.0", "Updateable": True},
    ], simple_update=False)
    # Supermicro: integration's "admin" is an Operator, which can power the box off
    accounts(m, [("2", "ADMIN", "Administrator", None), ("3", "admin", "Operator", None)])
    return m


# --------------------------------------------------- HPE iLO 4 (Gen9), minimal
def hpe_ilo4():
    """Only what storage needs: iLO 4 has no DMTF Storage; Smart Array lives in the
    OEM SmartStorage tree (Oem.Hp, HpSmartStorage* types, sizes in MiB, RAID level as
    a bare string, SSD wear as *used* endurance)."""
    m = Mock("hpe-ilo4")
    S, C, M = "/Systems/1", "/Chassis/1", "/Managers/1"
    common_root(m, "HPE", "ProLiant DL360 Gen9",
                {"Hp": {"@odata.type": "#HpiLOServiceExt.1.0.0.HpiLOServiceExt",
                        "Manager": [{"ManagerType": "iLO 4", "ManagerFirmwareVersion": "2.82", "HostName": "ilo-cache-03"}]}})
    m.coll("/Systems", [S], "Computer Systems")
    m.coll("/Chassis", [C], "Computer System Chassis")
    m.coll("/Managers", [M], "Managers")
    m.put(S, {"Id": "1", "Name": "Computer System", "Manufacturer": "HP", "Model": "ProLiant DL360 Gen9", "SKU": "755258-B21",
              "SerialNumber": "CZJ5430ABC", "BiosVersion": "P89 v2.80 (10/16/2020)", "HostName": "cache-03", "PowerState": "On",
              "Status": status("Warning"), "ProcessorSummary": {"Count": 2, "Model": "Intel(R) Xeon(R) CPU E5-2650 v4 @ 2.20GHz", "Status": status()},
              "MemorySummary": {"TotalSystemMemoryGiB": 128, "Status": status()},
              "Links": {"Chassis": [ref(C)], "ManagedBy": [ref(M)]},
              "Oem": {"Hp": {"Links": {"SmartStorage": ref(S + "/SmartStorage")}}}}, "#ComputerSystem.1.0.1.ComputerSystem")
    m.put(C, {"Id": "1", "Name": "Computer System Chassis", "ChassisType": "RackMount", "SerialNumber": "CZJ5430ABC", "Status": status("Warning"),
              "Links": {"ComputerSystems": [ref(S)], "ManagedBy": [ref(M)]}}, "#Chassis.1.0.0.Chassis")
    m.put(M, {"Id": "1", "Name": "Manager", "Model": "iLO 4", "FirmwareVersion": "iLO 4 v2.82", "Status": status()}, "#Manager.1.0.0.Manager")
    A = S + "/SmartStorage/ArrayControllers/0"
    m.put(S + "/SmartStorage", {"Id": "SmartStorage", "Name": "HpSmartStorage", "Status": status("Warning"),
                                "Links": {"ArrayControllers": ref(S + "/SmartStorage/ArrayControllers")}}, "#HpSmartStorage.1.0.0.HpSmartStorage")
    m.coll(S + "/SmartStorage/ArrayControllers", [A], "HpSmartStorageArrayControllers", "#HpSmartStorageArrayControllerCollection.1.0.0.HpSmartStorageArrayControllerCollection")
    m.put(A, {"Id": "0", "Name": "HpSmartStorageArrayController", "Model": "Smart Array P440ar Controller", "SerialNumber": "PDNLH0BRH8A1PA",
              "Location": "Slot 0", "LocationFormat": "PCISlot", "FirmwareVersion": {"Current": {"VersionString": "7.00"}},
              "Status": status("Warning"), "Links": {"LogicalDrives": ref(A + "/LogicalDrives"), "PhysicalDrives": ref(A + "/DiskDrives")}},
          "#HpSmartStorageArrayController.1.1.0.HpSmartStorageArrayController")
    disks = [("0", "1I:1:1", "MK000480GWCEV", "SSD", 457862, "OK", ["OK"], 3),
             ("1", "1I:1:2", "MK000480GWCEV", "SSD", 457862, "OK", ["OK"], 4),
             ("2", "1I:1:3", "EG001200JWJNQ", "HDD", 1144641, "OK", ["OK"], None),
             ("3", "1I:1:4", "EG001200JWJNQ", "HDD", 1144641, "Critical", ["Failed"], None),
             ("4", "2I:1:5", "EG001200JWJNQ", "HDD", 1144641, "OK", ["OK"], None)]
    for did, loc, model, media, mib, h, reasons, used in disks:
        d = {"Id": did, "Name": "HpSmartStorageDiskDrive", "Location": loc, "LocationFormat": "ControllerPort:Box:Bay", "Model": model,
             "MediaType": media, "InterfaceType": "SAS", "CapacityMiB": mib, "SerialNumber": "SN%s%s" % (model[-4:], did),
             "FirmwareVersion": {"Current": {"VersionString": "HPD3"}}, "Status": status(h), "DiskDriveStatusReasons": reasons}
        if used is not None:
            d["SSDEnduranceUtilizationPercentage"] = used
        m.put(A + "/DiskDrives/" + did, d, "#HpSmartStorageDiskDrive.1.1.0.HpSmartStorageDiskDrive")
    m.coll(A + "/DiskDrives", [A + "/DiskDrives/" + d[0] for d in disks], "HpSmartStorageDiskDrives", "#HpSmartStorageDiskDriveCollection.1.0.0.HpSmartStorageDiskDriveCollection")
    lds = [("1", "os", "1", 457830, "OK", ["OK"], ["0", "1"]), ("2", "cache", "5", 2289280, "Warning", ["InterimRecoveryMode"], ["2", "3", "4"])]
    for lid, name, raid, mib, h, reasons, members in lds:
        m.put(A + "/LogicalDrives/" + lid, {"Id": lid, "Name": "HpSmartStorageLogicalDrive", "LogicalDriveName": name, "LogicalDriveNumber": int(lid),
                                            "Raid": raid, "CapacityMiB": mib, "Status": status(h), "LogicalDriveStatusReasons": reasons,
                                            "Links": {"DataDrives": ref(A + "/LogicalDrives/" + lid + "/DataDrives")}},
              "#HpSmartStorageLogicalDrive.1.1.0.HpSmartStorageLogicalDrive")
        m.coll(A + "/LogicalDrives/" + lid + "/DataDrives", [A + "/DiskDrives/" + x for x in members], "HpSmartStorageDiskDrives")
    m.coll(A + "/LogicalDrives", [A + "/LogicalDrives/" + l[0] for l in lds], "HpSmartStorageLogicalDrives", "#HpSmartStorageLogicalDriveCollection.1.0.0.HpSmartStorageLogicalDriveCollection")
    accounts(m, [("1", "Administrator", None, {"Hp": {"LoginName": "", "Privileges": {"LoginPriv": True, "RemoteConsolePriv": True,
             "VirtualMediaPriv": True, "VirtualPowerAndResetPriv": True, "iLOConfigPriv": True, "UserConfigPriv": True}}}),
                 ("2", "admin", None, {"Hp": {"LoginName": "", "Privileges": {"LoginPriv": True, "RemoteConsolePriv": False,
                  "VirtualMediaPriv": False, "VirtualPowerAndResetPriv": False, "iLOConfigPriv": False, "UserConfigPriv": False}}})])
    return m


if __name__ == "__main__":
    for f in (dell, hpe, hpe_ilo4, lenovo, supermicro):
        f()
    print("mockups written to", HERE)
