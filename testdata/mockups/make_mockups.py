#!/usr/bin/env python3
"""Generate vendor-flavoured Redfish mockups in DMTF Redfish-Mockup-Server
layout (serve with: redfishMockupServer.py -S -D testdata/mockups/<vendor>).

They reproduce the OEM shapes rackhands handles (resource paths, log service
ids, Oem health rollups, firmware inventory noise, OEM virtual media actions).
Values are invented; structure follows each vendor's public Redfish docs.
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

    def put(self, path, obj, typ=None):
        obj = dict(obj)
        obj["@odata.id"] = V1 + path if path else V1 + "/"
        if typ:
            obj["@odata.type"] = typ
        d = os.path.join(self.root, path.strip("/"))
        os.makedirs(d, exist_ok=True)
        with open(os.path.join(d, "index.json"), "w") as f:
            json.dump(obj, f, indent=2, sort_keys=True)
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
            "UpdateService": ref("/UpdateService"), "SessionService": ref("/SessionService"), "Oem": oem}
    root.update(extra or {})
    m.put("", root, "#ServiceRoot.v1_15_0.ServiceRoot")
    m.coll("/SessionService/Sessions", [], "Sessions", "#SessionCollection.SessionCollection")
    m.put("/SessionService", {"Name": "Session Service", "Sessions": ref("/SessionService/Sessions")}, "#SessionService.v1_1_8.SessionService")


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


def firmware(m, items, simple_update=True):
    ids = [i["Id"] for i in items]
    for i in items:
        m.put("/UpdateService/FirmwareInventory/" + i["Id"], i, "#SoftwareInventory.v1_9_0.SoftwareInventory")
    m.coll("/UpdateService/FirmwareInventory", ["/UpdateService/FirmwareInventory/" + x for x in ids], "Firmware Inventory")
    us = {"Name": "Update Service", "FirmwareInventory": ref("/UpdateService/FirmwareInventory"), "ServiceEnabled": True}
    if simple_update:
        us["Actions"] = {"#UpdateService.SimpleUpdate": {"target": V1 + "/UpdateService/Actions/UpdateService.SimpleUpdate",
                                                         "TransferProtocol@Redfish.AllowableValues": ["HTTP", "HTTPS", "NFS", "CIFS"]}}
    m.put("/UpdateService", us, "#UpdateService.v1_11_0.UpdateService")


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


# ---------------------------------------------------------------- Dell iDRAC 9
def dell():
    m = Mock("dell")
    S, C, M = "/Systems/System.Embedded.1", "/Chassis/System.Embedded.1", "/Managers/iDRAC.Embedded.1"
    common_root(m, "Dell", "Integrated Dell Remote Access Controller", {"Dell": {"ManagerMACAddress": "d0:8e:79:00:00:01", "ServiceTag": "7XQ2KF3"}})
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
              "Processors": ref(S + "/Processors"), "Memory": ref(S + "/Memory"),
              "Oem": {"Dell": {"DellSystem": {"@odata.type": "#DellSystem.v1_4_0.DellSystem", "PSRollupStatus": "Critical", "FanRollupStatus": "OK",
                                              "TempRollupStatus": "OK", "VoltRollupStatus": "OK", "CPURollupStatus": "OK", "StorageRollupStatus": "OK",
                                              "BatteryRollupStatus": "OK", "IntrusionRollupStatus": "OK", "SELRollupStatus": "Critical",
                                              "CurrentRollupStatus": "OK", "LicensingRollupStatus": "OK"}}}},
          "#ComputerSystem.v1_20_0.ComputerSystem")
    m.coll(S + "/Processors", [], "Processors")
    m.coll(S + "/Memory", [], "Memory")
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
              "LogServices": ref(S + "/LogServices"),
              "Oem": {"Hpe": {"AggregateHealthStatus": {
                  "BiosOrHardwareHealth": {"Status": {"Health": "OK"}}, "FanRedundancy": "Redundant",
                  "Fans": {"Status": {"Health": "Warning"}}, "Memory": {"Status": {"Health": "OK"}}, "Network": {"Status": {"Health": "OK"}},
                  "PowerSupplies": {"Status": {"Health": "OK"}}, "PowerSupplyRedundancy": "Redundant", "Processors": {"Status": {"Health": "OK"}},
                  "SmartStorageBattery": {"Status": {"Health": "OK"}}, "Storage": {"Status": {"Health": "OK"}}, "Temperatures": {"Status": {"Health": "OK"}}}}}},
          "#ComputerSystem.v1_17_0.ComputerSystem")
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
    ])
    return m


# ----------------------------------------------------------- Lenovo XCC 2
def lenovo():
    m = Mock("lenovo")
    S, C, M = "/Systems/1", "/Chassis/1", "/Managers/1"
    common_root(m, "Lenovo", "ThinkSystem SR650 V2", {"Lenovo": {"@odata.type": "#LenovoServiceRoot.v1_0_0.LenovoServiceRoot", "Version": "XCC2"}})
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
              "Links": {"Chassis": [ref(C)], "ManagedBy": [ref(M)]}, "LogServices": ref(S + "/LogServices")},
          "#ComputerSystem.v1_16_0.ComputerSystem")
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
              "Links": {"Chassis": [ref(C)], "ManagedBy": [ref(M)]}},
          "#ComputerSystem.v1_5_1.ComputerSystem")
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
    return m


if __name__ == "__main__":
    for f in (dell, hpe, lenovo, supermicro):
        f()
    print("mockups written to", HERE)
