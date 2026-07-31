package windowsssh

import (
	"strings"
	"testing"
	"time"

	"github.com/LJAYi/komari-bridge/internal/config"
)

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
