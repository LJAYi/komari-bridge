package windowsssh

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/LJAYi/komari-bridge/internal/config"
	"github.com/LJAYi/komari-bridge/internal/model"
)

func TestRunWithReconnectRetriesOnceAfterTransportFailure(t *testing.T) {
	t.Parallel()
	attempts, reconnects := 0, 0
	output, err := runWithReconnect(context.Background(), func() ([]byte, error) {
		attempts++
		if attempts == 1 {
			return nil, errors.New("remote command exited without exit status")
		}
		return []byte("recovered"), nil
	}, func() { reconnects++ })
	if err != nil {
		t.Fatal(err)
	}
	if string(output) != "recovered" || attempts != 2 || reconnects != 1 {
		t.Fatalf("output=%q attempts=%d reconnects=%d", output, attempts, reconnects)
	}
}

func TestRunWithReconnectDoesNotRetryCancellation(t *testing.T) {
	t.Parallel()
	attempts, reconnects := 0, 0
	_, err := runWithReconnect(context.Background(), func() ([]byte, error) {
		attempts++
		return nil, context.Canceled
	}, func() { reconnects++ })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v", err)
	}
	if attempts != 1 || reconnects != 0 {
		t.Fatalf("attempts=%d reconnects=%d", attempts, reconnects)
	}
}

func TestWSLConstructorDoesNotEmitWindowsHost(t *testing.T) {
	t.Parallel()
	cfg := config.WindowsSSHConfig{ID: "workstation-a", User: "monitor", Password: "test-only", InsecureIgnoreHostKey: true}
	provider, err := NewWSL(cfg, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if provider.SourceType() != "windows_wsl" || provider.emitHost || !provider.cfg.DiscoverWSL || provider.cfg.EnableNVIDIA {
		t.Fatalf("unexpected WSL mode: %#v", provider)
	}
}

func TestDecodeCollectorOutputIgnoresPowerShellCLIXML(t *testing.T) {
	t.Parallel()
	output := []byte("#< CLIXML\r\n" +
		`{"windows":{"cpu_name":"Example CPU","cpu_cores":32,"gpus":[{"name":"Example GPU","memory_total":25769803776}]},"wsl":[{"guid":"{abc}","name":"Ubuntu","version":2,"online":true}]}` +
		"\r\n<Objs><Obj S=\"progress\" /></Objs>")
	raw, err := decodeCollectorOutput(output)
	if err != nil {
		t.Fatal(err)
	}
	if raw.Windows.CPUName != "Example CPU" || raw.Windows.CPUCores != 32 || len(raw.Windows.GPUs) != 1 {
		t.Fatalf("unexpected Windows report: %#v", raw.Windows)
	}
	if len(raw.WSL) != 1 || raw.WSL[0].GUID != "{abc}" || !raw.WSL[0].Online {
		t.Fatalf("unexpected WSL report: %#v", raw.WSL)
	}
}

func TestCollectorUsesShortEncodedBootstrap(t *testing.T) {
	t.Parallel()
	command := collectorBootstrapCommand()
	if !strings.Contains(command, "-EncodedCommand") {
		t.Fatalf("collector bootstrap is not encoded: %q", command)
	}
	if len(command) > 2048 {
		t.Fatalf("collector bootstrap is unexpectedly large: %d bytes", len(command))
	}
	if !strings.Contains(collectorScript(true), "python3 -") {
		t.Fatal("collector script does not contain the WSL inner collector")
	}
	if !strings.Contains(collectorScript(false), "if ($false)") {
		t.Fatal("WSL-only collector does not disable Windows host collection")
	}
}

func TestValidateWindowsReportRejectsDegradedHostData(t *testing.T) {
	t.Parallel()
	valid := windowsReport{CPUName: "CPU", CPUCores: 16, Arch: "AMD64", OS: "Windows", Kernel: "10.0", MemoryTotal: 1024, MemoryFree: 512, DiskTotal: 4096, DiskFree: 2048, Uptime: 100}
	if err := validateWindowsReport(valid); err != nil {
		t.Fatalf("valid report rejected: %v", err)
	}
	tests := []struct {
		name   string
		mutate func(*windowsReport)
	}{
		{"missing CPU", func(r *windowsReport) { r.CPUName = "" }},
		{"zero memory", func(r *windowsReport) { r.MemoryTotal = 0 }},
		{"free exceeds memory", func(r *windowsReport) { r.MemoryFree = r.MemoryTotal + 1 }},
		{"zero disk", func(r *windowsReport) { r.DiskTotal = 0 }},
		{"free exceeds disk", func(r *windowsReport) { r.DiskFree = r.DiskTotal + 1 }},
		{"zero uptime", func(r *windowsReport) { r.Uptime = 0 }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			copy := valid
			test.mutate(&copy)
			if err := validateWindowsReport(copy); err == nil {
				t.Fatal("degraded report was accepted")
			}
		})
	}
}

func TestValidateWSLDataFailureIsScopedToDistro(t *testing.T) {
	t.Parallel()
	valid := wslData{
		CPUName: "CPU", CPUCores: 4, Arch: "x86_64", OS: "Ubuntu", Kernel: "6.6",
		CPU: cpuCounters{Total: 100}, Memory: map[string]uint64{"MemTotal": 1024, "MemAvailable": 512},
		DiskTotal: 4096, DiskUsed: 1024, Uptime: 100, GPUOK: true,
	}
	if err := validateWSLData(valid, true); err != nil {
		t.Fatalf("valid WSL data rejected: %v", err)
	}
	broken := valid
	broken.Memory = nil
	if err := validateWSLData(broken, true); err == nil {
		t.Fatal("broken WSL data accepted")
	}
	host := windowsReport{CPUName: "CPU", CPUCores: 16, Arch: "AMD64", OS: "Windows", Kernel: "10.0", MemoryTotal: 1024, MemoryFree: 512, DiskTotal: 4096, DiskFree: 2048, Uptime: 100}
	if err := validateWindowsReport(host); err != nil {
		t.Fatalf("WSL failure contaminated host validation: %v", err)
	}
}

func TestWSLGPUCachePreservesDevicesDuringTransientFailure(t *testing.T) {
	t.Parallel()
	provider := &Provider{previousWGPU: make(map[string]cachedGPUs)}
	now := time.Now()
	devices := []model.GPUDevice{{Name: "GPU", MemoryTotal: 1024}}
	success := wslReport{GUID: "guid-a", Data: &wslData{GPUOK: true, GPUs: devices}}
	if !provider.stabilizeWSLGPU(&success, now) {
		t.Fatal("successful GPU result was rejected")
	}
	failure := wslReport{GUID: "guid-a", Data: &wslData{GPUOK: false}}
	if !provider.stabilizeWSLGPU(&failure, now.Add(20*time.Second)) || !failure.Data.GPUOK || len(failure.Data.GPUs) != 1 {
		t.Fatalf("fresh GPU cache was not reused: %#v", failure.Data)
	}
	failure.Data.GPUs[0].Name = "changed"
	if provider.previousWGPU["guid-a"].devices[0].Name != "GPU" {
		t.Fatal("GPU cache aliases a caller-owned slice")
	}
	expired := wslReport{GUID: "guid-a", Data: &wslData{GPUOK: false}}
	if provider.stabilizeWSLGPU(&expired, now.Add(wslGPUGracePeriod+time.Second)) {
		t.Fatal("expired GPU cache was reused")
	}
	restarted := &Provider{previousWGPU: make(map[string]cachedGPUs)}
	if restarted.stabilizeWSLGPU(&expired, now) {
		t.Fatal("failed first GPU probe was accepted")
	}
}
