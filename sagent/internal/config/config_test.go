package config

import (
	"os"
	"path/filepath"
	"testing"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "SAgent.yaml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("写配置夹具失败：%v", err)
	}
	return p
}

// 注册上报的插件清单必须来自 plugins 段（enabled=true 才算），不再写死 host_metrics
func TestEnabledPluginNamesFollowsConfig(t *testing.T) {
	cfg, err := Load(writeConfig(t, `
resource:
  id: "order.prod.host.sagent-1"
plugins:
  host_metrics:
    enabled: true
  log_metrics:
    enabled: true
  mysql_probe:
    enabled: false
  custom_scripts:
    enabled: true
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got := cfg.EnabledPluginNames()
	want := []string{"host_metrics", "log_metrics", "custom_scripts"}
	if len(got) != len(want) {
		t.Fatalf("应上报 %v，实际 %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("应上报 %v，实际 %v", want, got)
		}
	}
}

// 全部未启用时上报空清单（不虚构插件），且不为 nil（注册体里应是 [] 而不是 null）
func TestEnabledPluginNamesEmptyWhenAllDisabled(t *testing.T) {
	cfg, err := Load(writeConfig(t, "resource:\n  id: \"r1\"\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.EnabledPluginNames(); got == nil || len(got) != 0 {
		t.Fatalf("未启用任何插件时应为空切片，实际 %#v", got)
	}
}

// l0_console.url 配置文件未声明时用 SAGENT_L0_CONSOLE_URL 兜底（容器编排只改 env）
func TestL0ConfigStateDirectory(t *testing.T) {
	for _, tc := range []struct{ yaml, want string }{
		{"resource:\n  id: r1\n", "data"},
		{"resource:\n  id: r1\nl0_console:\n  data_dir: /var/lib/sagent-control\n", "/var/lib/sagent-control"},
	} {
		cfg, err := Load(writeConfig(t, tc.yaml))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.L0Console.DataDir != tc.want {
			t.Fatalf("state directory=%q want=%q", cfg.L0Console.DataDir, tc.want)
		}
	}
}

func TestL0ConsoleURLFromEnv(t *testing.T) {
	t.Setenv(EnvL0ConsoleURL, " http://l0-console:8080 ")
	cfg, err := Load(writeConfig(t, "resource:\n  id: \"r1\"\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.L0Console.URL != "http://l0-console:8080" {
		t.Fatalf("应从环境变量取接入点（去首尾空白），实际 %q", cfg.L0Console.URL)
	}
}

// 配置文件显式声明时优先于环境变量（配置文件是更具体的一份）
func TestL0ConsoleURLFileWinsOverEnv(t *testing.T) {
	t.Setenv(EnvL0ConsoleURL, "http://from-env:8080")
	cfg, err := Load(writeConfig(t, "resource:\n  id: \"r1\"\nl0_console:\n  url: \"http://from-file:8080\"\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.L0Console.URL != "http://from-file:8080" {
		t.Fatalf("配置文件优先，实际 %q", cfg.L0Console.URL)
	}
}
