package windowsssh

import (
	"encoding/base64"
	"strings"
	"unicode/utf16"
)

func collectorScript() string {
	wslPayload := base64.StdEncoding.EncodeToString([]byte(wslCollectorPython))
	return strings.ReplaceAll(windowsCollectorPowerShell, "__WSL_PAYLOAD__", wslPayload)
}

func collectorBootstrapCommand() string {
	const bootstrap = `$script = [Console]::In.ReadToEnd(); Invoke-Expression $script`
	runes := utf16.Encode([]rune(bootstrap))
	bytes := make([]byte, len(runes)*2)
	for i, value := range runes {
		bytes[i*2] = byte(value)
		bytes[i*2+1] = byte(value >> 8)
	}
	return "powershell.exe -NoLogo -NoProfile -NonInteractive -EncodedCommand " + base64.StdEncoding.EncodeToString(bytes)
}

const windowsCollectorPowerShell = `$ErrorActionPreference = "SilentlyContinue"
$ProgressPreference = "SilentlyContinue"
[Console]::OutputEncoding = New-Object System.Text.UTF8Encoding($false)
$OutputEncoding = [Console]::OutputEncoding

function Get-NvidiaGpu {
    $result = @()
    try {
        $exe = (Get-Command nvidia-smi.exe -ErrorAction Stop).Source
        $psi = New-Object System.Diagnostics.ProcessStartInfo
        $psi.FileName = $exe
        $psi.Arguments = '--query-gpu=index,name,utilization.gpu,memory.used,memory.total,temperature.gpu --format=csv,noheader,nounits'
        $psi.UseShellExecute = $false
        $psi.CreateNoWindow = $true
        $psi.RedirectStandardOutput = $true
        $psi.RedirectStandardError = $true
        $process = New-Object System.Diagnostics.Process
        $process.StartInfo = $psi
        [void]$process.Start()
        if (-not $process.WaitForExit(8000)) {
            $process.Kill()
            return @()
        }
        $output = $process.StandardOutput.ReadToEnd()
        foreach ($line in ($output -split '[\r\n]+')) {
            $fields = @($line -split ',' | ForEach-Object { $_.Trim() })
            if ($fields.Count -ne 6) { continue }
            $result += [pscustomobject]@{
                name = $fields[1]
                utilization = [double]$fields[2]
                memory_used = [int64]([double]$fields[3] * 1MB)
                memory_total = [int64]([double]$fields[4] * 1MB)
                temperature = [int]([double]$fields[5])
            }
        }
    } catch {}
    return @($result)
}

$processors = @(Get-CimInstance Win32_Processor)
$operatingSystem = Get-CimInstance Win32_OperatingSystem
$logicalDisks = @(Get-CimInstance Win32_LogicalDisk -Filter "DriveType=3")
$diskTotal = [int64](($logicalDisks | Measure-Object -Property Size -Sum).Sum)
$diskFree = [int64](($logicalDisks | Measure-Object -Property FreeSpace -Sum).Sum)
$netUp = [int64]0
$netDown = [int64]0
try {
    $statistics = @(Get-NetAdapterStatistics -ErrorAction Stop)
    $netUp = [int64](($statistics | Measure-Object -Property SentBytes -Sum).Sum)
    $netDown = [int64](($statistics | Measure-Object -Property ReceivedBytes -Sum).Sum)
} catch {}
$cpuUsage = [double](($processors | Measure-Object -Property LoadPercentage -Average).Average)
$cpuName = if ($processors.Count -gt 0) { [string]$processors[0].Name } else { "" }
$cpuLogical = [int](($processors | Measure-Object -Property NumberOfLogicalProcessors -Sum).Sum)
$cpuPhysical = [int](($processors | Measure-Object -Property NumberOfCores -Sum).Sum)
$memoryTotal = [int64]$operatingSystem.TotalVisibleMemorySize * 1KB
$memoryFree = [int64]$operatingSystem.FreePhysicalMemory * 1KB
$uptime = [int64]((Get-Date) - $operatingSystem.LastBootUpTime).TotalSeconds
$processes = @(Get-Process).Count
$windowsGpus = @(Get-NvidiaGpu)

$running = @()
try {
    $running = @(& wsl.exe --list --running --quiet 2>$null | ForEach-Object { (([string]$_) -replace [char]0, '').Trim() } | Where-Object { $_ })
} catch {}
$wsl = @()
try {
    $registrations = @(Get-ChildItem 'HKCU:\Software\Microsoft\Windows\CurrentVersion\Lxss')
    foreach ($registration in $registrations) {
        $properties = Get-ItemProperty $registration.PSPath
        $distribution = [string]$properties.DistributionName
        if (-not $distribution) { continue }
        $online = ($running -contains $distribution) -or ([int]$properties.State -eq 1)
        $data = $null
        if ($online) {
            try {
                $json = & wsl.exe -d $distribution -- sh -lc "printf '%s' '__WSL_PAYLOAD__' | base64 -d | python3 -" 2>$null
                if ($LASTEXITCODE -eq 0 -and $json) {
                    $data = (($json -join [Environment]::NewLine) | ConvertFrom-Json)
                } else {
                    $online = $false
                }
            } catch { $online = $false }
        }
        $wsl += [pscustomobject]@{
            guid = [string]$registration.PSChildName
            name = $distribution
            version = [int]$properties.Version
            base_path = [string]$properties.BasePath
            online = [bool]$online
            data = $data
        }
    }
} catch {}

[pscustomobject]@{
    windows = [pscustomobject]@{
        cpu_name = $cpuName
        cpu_cores = $cpuLogical
        cpu_physical_cores = $cpuPhysical
        arch = $env:PROCESSOR_ARCHITECTURE
        os = [string]$operatingSystem.Caption
        kernel = [string]$operatingSystem.Version
        cpu_usage = $cpuUsage
        memory_total = $memoryTotal
        memory_free = $memoryFree
        disk_total = $diskTotal
        disk_free = $diskFree
        network_up = $netUp
        network_down = $netDown
        uptime = $uptime
        processes = $processes
        gpus = @($windowsGpus)
    }
    wsl = @($wsl)
} | ConvertTo-Json -Depth 10 -Compress
`

const wslCollectorPython = `import json
import os
import platform
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
    package = core = None
    try:
        for line in open("/proc/cpuinfo", encoding="utf-8"):
            if line.startswith("model name") and not name:
                name = line.split(":", 1)[1].strip()
            elif line.startswith("physical id"):
                package = line.split(":", 1)[1].strip()
            elif line.startswith("core id"):
                core = line.split(":", 1)[1].strip()
            elif not line.strip() and package is not None and core is not None:
                physical.add((package, core))
                package = core = None
    except OSError:
        pass
    values = [int(value) for value in open("/proc/stat", encoding="utf-8").readline().split()[1:]]
    return name, os.cpu_count() or 0, len(physical), {"total": sum(values[:8]), "idle": values[3] + values[4]}

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
            total += int(fields[1]); used += int(fields[2])
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
            fields = values.split(); down += int(fields[0]); up += int(fields[8])
    except OSError:
        pass
    return {"up": up, "down": down}

def gpu_info():
    output = command(["nvidia-smi", "--query-gpu=index,name,utilization.gpu,memory.used,memory.total,temperature.gpu", "--format=csv,noheader,nounits"])
    result = []
    for line in output.splitlines():
        fields = [field.strip() for field in line.split(",")]
        if len(fields) != 6:
            continue
        try:
            result.append({"name": fields[1], "utilization": float(fields[2]), "memory_used": int(float(fields[3]) * 1048576), "memory_total": int(float(fields[4]) * 1048576), "temperature": int(float(fields[5]))})
        except ValueError:
            pass
    return result

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

print(json.dumps({"cpu_name": cpu_name, "cpu_cores": cpu_cores, "cpu_physical_cores": physical_cores, "arch": platform.machine(), "os": os_release(), "kernel": platform.release(), "cpu": cpu, "memory": memory_info(), "load": list(os.getloadavg()), "disk_total": disk_total, "disk_used": disk_used, "network": network_info(), "uptime": uptime, "processes": processes, "gpus": gpu_info()}, separators=(",", ":")))
`
