package linuxssh

const collectorScript = `
import json
import os
import platform
import re
import subprocess

def command(args, timeout=8):
    try:
        return subprocess.check_output(args, text=True, stderr=subprocess.DEVNULL, timeout=timeout).strip()
    except Exception:
        return ""

def os_release():
    values = {}
    try:
        for line in open("/etc/os-release", encoding="utf-8"):
            if "=" in line:
                key, value = line.rstrip().split("=", 1)
                values[key] = value.strip('"')
    except OSError:
        pass
    return values.get("PRETTY_NAME") or values.get("NAME") or platform.system()

def cpu_info():
    name = ""
    physical = set()
    physical_id = None
    core_id = None
    try:
        for line in open("/proc/cpuinfo", encoding="utf-8"):
            if line.startswith("model name") and not name:
                name = line.split(":", 1)[1].strip()
            elif line.startswith("physical id"):
                physical_id = line.split(":", 1)[1].strip()
            elif line.startswith("core id"):
                core_id = line.split(":", 1)[1].strip()
            elif not line.strip() and physical_id is not None and core_id is not None:
                physical.add((physical_id, core_id))
                physical_id = core_id = None
    except OSError:
        pass
    parts = open("/proc/stat", encoding="utf-8").readline().split()[1:]
    values = [int(value) for value in parts]
    total = sum(values[:8])
    idle = values[3] + (values[4] if len(values) > 4 else 0)
    return name, os.cpu_count() or 0, len(physical), {"Total": total, "Idle": idle}

def memory_info():
    result = {}
    try:
        for line in open("/proc/meminfo", encoding="utf-8"):
            key, value = line.split(":", 1)
            if key in ("MemTotal", "MemAvailable", "SwapTotal", "SwapFree"):
                result[key] = int(value.strip().split()[0])
    except OSError:
        pass
    return result

def disk_info():
    output = command(["df", "-B1", "-P", "-x", "tmpfs", "-x", "devtmpfs", "-x", "squashfs", "-x", "overlay"])
    total = used = 0
    seen = set()
    for line in output.splitlines()[1:]:
        fields = line.split()
        if len(fields) < 6 or fields[0] in seen:
            continue
        seen.add(fields[0])
        try:
            total += int(fields[1])
            used += int(fields[2])
        except ValueError:
            pass
    return total, used

def network_info():
    up = down = 0
    try:
        for line in open("/proc/net/dev", encoding="utf-8"):
            if ":" not in line:
                continue
            interface, values = line.split(":", 1)
            if interface.strip() == "lo":
                continue
            fields = values.split()
            down += int(fields[0])
            up += int(fields[8])
    except OSError:
        pass
    return {"Up": up, "Down": down}

def connection_count(protocol):
    count = 0
    for suffix in (protocol, protocol + "6"):
        try:
            with open("/proc/net/" + suffix, encoding="utf-8") as handle:
                count += max(0, sum(1 for _ in handle) - 1)
        except OSError:
            pass
    return count

def gpu_info():
    query = "index,name,utilization.gpu,memory.used,memory.total,temperature.gpu"
    output = command(["nvidia-smi", "--query-gpu=" + query, "--format=csv,noheader,nounits"])
    result = []
    for line in output.splitlines():
        fields = [field.strip() for field in line.split(",")]
        if len(fields) != 6:
            continue
        try:
            result.append({
                "name": fields[1],
                "utilization": float(fields[2]),
                "memory_used": int(float(fields[3]) * 1024 * 1024),
                "memory_total": int(float(fields[4]) * 1024 * 1024),
                "temperature": int(float(fields[5])),
            })
        except ValueError:
            pass
    return result

def service_active(name):
    return command(["systemctl", "is-active", name]) == "active"

def slurm_info():
    output = command(["sinfo", "--noheader", "-o", "%P|%a|%D|%T|%C|%G"])
    if not output:
        return {"available": False}
    partitions = []
    for line in output.splitlines():
        fields = line.split("|", 5)
        if len(fields) == 6:
            try:
                nodes = int(fields[2])
            except ValueError:
                nodes = 0
            partitions.append({
                "name": fields[0].rstrip("*"), "availability": fields[1],
                "nodes": nodes, "state": fields[3], "cpus": fields[4], "gres": fields[5],
            })
    jobs = []
    queue = command(["squeue", "--noheader", "-o", "%i|%P|%u|%T|%M|%D|%R|%b"])
    for line in queue.splitlines():
        fields = line.split("|", 7)
        if len(fields) == 8:
            try:
                nodes = int(fields[5])
            except ValueError:
                nodes = 0
            jobs.append({
                "id": fields[0], "partition": fields[1], "user": fields[2],
                "state": fields[3], "elapsed": fields[4], "nodes": nodes,
                "reason": fields[6], "gres": fields[7],
            })
    configured = allocated = 0
    nodes = command(["scontrol", "show", "node", "-o"])
    for line in nodes.splitlines():
        configured_match = re.search(r"CfgTRES=.*?gres/gpu=(\d+)", line)
        allocated_match = re.search(r"AllocTRES=.*?gres/gpu=(\d+)", line)
        if configured_match:
            configured += int(configured_match.group(1))
        if allocated_match:
            allocated += int(allocated_match.group(1))
    return {
        "available": True,
        "controller_up": service_active("slurmctld"),
        "node_daemon_up": service_active("slurmd"),
        "partitions": partitions, "jobs": jobs,
        "gpus_configured": configured, "gpus_allocated": allocated,
    }

cpu_name, cpu_cores, physical_cores, cpu = cpu_info()
disk_total, disk_used = disk_info()
try:
    uptime = int(float(open("/proc/uptime", encoding="utf-8").read().split()[0]))
except Exception:
    uptime = 0
try:
    processes = sum(name.isdigit() for name in os.listdir("/proc"))
except OSError:
    processes = 0

print(json.dumps({
    "cpu_name": cpu_name,
    "cpu_cores": cpu_cores,
    "cpu_physical_cores": physical_cores,
    "arch": platform.machine(),
    "os": os_release(),
    "kernel": platform.release(),
    "cpu": cpu,
    "memory": memory_info(),
    "load": list(os.getloadavg()),
    "disk_total": disk_total,
    "disk_used": disk_used,
    "network": network_info(),
    "tcp": connection_count("tcp"),
    "udp": connection_count("udp"),
    "uptime": uptime,
    "processes": processes,
    "gpus": gpu_info(),
    "slurm": slurm_info(),
}, separators=(",", ":")))
`
