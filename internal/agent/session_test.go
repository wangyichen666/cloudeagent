package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVersionMajorMinor(t *testing.T) {
	cases := []struct {
		in    string
		major int
		minor int
	}{
		{"QwenPaw, version 1.1.7", 1, 1},
		{"QwenPaw, version 2.0.4", 2, 0},
		{"qwenpaw 2.3.0", 2, 3},
		{"", 0, 0},
	}
	for _, c := range cases {
		major, minor := versionMajorMinor(c.in)
		if major != c.major || minor != c.minor {
			t.Errorf("versionMajorMinor(%q) = %d.%d, want %d.%d",
				c.in, major, minor, c.major, c.minor)
		}
	}
}

func fakeQwenPawBin(t *testing.T, version string) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "qwenpaw")
	script := "#!/bin/sh\nprintf 'QwenPaw, version %s\\n' " + version + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

func TestAcpReadyVersionGate(t *testing.T) {
	ws := t.TempDir()

	// 旧版 qwenpaw：应提示升级，即使模型配置真实。
	old := fakeQwenPawBin(t, "1.1.7")
	cfgOld := NewConfigManager("", nil)
	cfgOld.Apply(&RuntimeConfig{BaseURL: "https://api.example.com/v1", APIKey: "sk", Model: "m"})
	sOld, err := NewSession(ws, cfgOld, old)
	if err != nil {
		t.Fatal(err)
	}
	ready, reason := sOld.AcpReady()
	if ready {
		t.Fatal("旧版 qwenpaw 不应就绪")
	}
	if !strings.Contains(reason, "升级") {
		t.Fatalf("旧版提示应包含升级指引: %s", reason)
	}

	// 新版 + 真实模型：就绪。
	newBin := fakeQwenPawBin(t, "2.0.4")
	cfgNew := NewConfigManager("", nil)
	cfgNew.Apply(&RuntimeConfig{BaseURL: "https://api.example.com/v1", APIKey: "sk", Model: "m"})
	sNew, err := NewSession(ws, cfgNew, newBin)
	if err != nil {
		t.Fatal(err)
	}
	ready, reason = sNew.AcpReady()
	if !ready {
		t.Fatalf("新版 qwenpaw + 真实模型应就绪: %s", reason)
	}

	// 新版 + mock 配置：不可用但原因明确。
	sMock, err := NewSession(ws, NewConfigManager("", nil), newBin)
	if err != nil {
		t.Fatal(err)
	}
	ready, reason = sMock.AcpReady()
	if ready || !strings.Contains(reason, "mock") {
		t.Fatalf("mock 配置应不可用且提示 mock: ready=%v reason=%s", ready, reason)
	}
}
