package control

// action.go —— 本机 agent-action 执行（G2 通道退役后通道 A 的落地端）。
//
// 动作由 SAgent 心跳拉取（tasks 字段）后在目标机本地执行，不再经 L0 docker exec（通道 B）。
// 复用 run.sh 监督进程语义：run/stopped 标记 + run/SAgent.pid 进程组信号完成 start/stop/restart。
// 破坏性动作（stop/restart）在回执完成后才由监督进程按标记收敛，避免自杀阻断回执。

import (
	"os"
	"strconv"
	"strings"
	"syscall"

	"github.com/sagent/core/internal/constants"
)

// localAction 在监督进程（run.sh）语义下执行本机 agent-action。
// 返回 (status, note)；status ∈ done|failed。
func localAction(action string) (string, string) {
	return localActionAt(action, constants.DefaultStopFile, constants.DefaultPIDFile)
}

// localActionAt 可注入路径的 localAction（单测传临时目录，避免污染真实 run/）。
func localActionAt(action, stopFile, pidFile string) (string, string) {
	switch action {
	case "start":
		if err := os.Remove(stopFile); err != nil && !os.IsNotExist(err) {
			return "failed", "start 清除 stopped 标记失败: " + err.Error()
		}
		return "done", "SAgent start：已清除 stopped 标记，监督进程恢复"
	case "stop":
		if err := os.WriteFile(stopFile, []byte("1"), 0o644); err != nil {
			return "failed", "stop 添加 stopped 标记失败: " + err.Error()
		}
		_ = signalGroup(pidFile, syscall.SIGTERM)
		return "done", "SAgent stop：已置 stopped 标记并优雅终止进程组"
	case "restart":
		if err := os.WriteFile(stopFile, []byte("1"), 0o644); err != nil {
			return "failed", "restart 添加 stopped 标记失败: " + err.Error()
		}
		_ = signalGroup(pidFile, syscall.SIGTERM)
		if err := os.Remove(stopFile); err != nil && !os.IsNotExist(err) {
			return "failed", "restart 清除 stopped 标记失败: " + err.Error()
		}
		return "done", "SAgent restart：已触发监督进程重启（stopped 标记翻转）"
	default:
		return "failed", "unsupported action: " + action
	}
}

// signalGroup 向 pid 文件记录的进程组发信号（best-effort；SAGent 进程组由 run.sh 监督）。
func signalGroup(pidFile string, sig syscall.Signal) error {
	b, err := os.ReadFile(pidFile)
	if err != nil {
		return err
	}
	p, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || p <= 1 {
		return nil
	}
	return syscall.Kill(-p, sig)
}