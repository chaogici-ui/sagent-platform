package main

// 反向隧道自动化（2026-09-22 用户拍板：只给 IP + 类型 + 凭据，接入点平台全自动）。
//
// 背景：跨网段真实主机到平台方向单向不通（实测目标机 → 平台宿主机 8080 全 000），
// 唯一可靠双向通道是平台 → 目标机的 SSH。ssh -R 在这条已有连接上"倒挂"端口：
// 目标机 127.0.0.1:18080 ──(ssh -R)──> 平台容器 8080。
//
// 原方案的三个死穴（隧道死了没人拉）：没人跑过 / 容器重建即丢 / 网络抖动自杀。
// 本模块让平台自己持有并守护隧道：
//   - 探路遇接入点不可达 → 自动建隧道并重测（onboard_ansible.go autoResolveAccessPoint）
//   - 常驻 keeper 每 30s 巡检：host 类资源登记了 127.0.0.1 接入点 → 隧道进程不在就重建
//     （覆盖容器重启后全量恢复、运行中掉线自愈，无需人工跑脚本）

import (
	"fmt"
	"log"
	"os/exec"
	"strings"
	"sync"
	"time"

	storepkg "github.com/sagent/l0-console/store"
)

// tunnelRemotePort 反向隧道在目标机侧监听的端口（Agent 接入点 = http://127.0.0.1:<该端口>）。
// 默认 18080，可用 TUNNEL_REMOTE_PORT 覆盖
func tunnelRemotePort() int {
	if cfgTunnelRemotePort > 0 {
		return cfgTunnelRemotePort
	}
	return 18080
}

// tunnelCheckInterval keeper 巡检间隔
const tunnelCheckInterval = 30 * time.Second

// liveTunnels 资源ID → 在跑的隧道进程。keeper 判活依据：cmd.ProcessState 非空即已退出
var liveTunnels sync.Map

// reverseTunnelCmd 构造反向隧道命令（纯函数，回归有断言）：
// sshpass -p PASS ssh -N -R <目标机端口>:127.0.0.1:<平台端口> user@host -p port
// -N 只转发不执行命令；ExitOnForwardFailure 让端口被占时进程立刻退出（keeper 重试可感知）
func reverseTunnelCmd(cred *sshCred) *exec.Cmd {
	remote := fmt.Sprintf("%d:127.0.0.1:%d", tunnelRemotePort(), platformPort())
	return exec.Command("sshpass", "-p", cred.Pass, "ssh",
		"-N",
		"-R", remote,
		"-p", fmt.Sprint(cred.Port),
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "ServerAliveInterval=30",
		"-o", "ServerAliveCountMax=3",
		"-o", "ExitOnForwardFailure=yes",
		fmt.Sprintf("%s@%s", cred.User, cred.Host))
}

// isTunnelAccessPoint 接入点是否形如 http://127.0.0.1:<port>（隧道语义）。
// 精确匹配主机段——HasPrefix 会把 127.0.0.1:18080.evil.com 这类地址误放行
func isTunnelAccessPoint(url string) bool {
	base := fmt.Sprintf("http://127.0.0.1:%d", tunnelRemotePort())
	return url == base || strings.HasPrefix(url, base+"/")
}

// tunnelAlive 该资源名下隧道进程是否还活着。
// 判活依据是收尸协程（watchTunnelExit）落定的 ProcessState：Wait 返回即视为退出。
// 不能用 Process.Signal(nil) 探活——Go 对 nil 信号做 syscall.Signal 断言必失败
// （恒定返回 "os: unsupported signal type"），那会让 keeper 每轮都判死并重建
func tunnelAlive(resourceID string) bool {
	v, ok := liveTunnels.Load(resourceID)
	if !ok {
		return false
	}
	cmd, ok := v.(*exec.Cmd)
	if !ok || cmd == nil || cmd.Process == nil {
		return false
	}
	return cmd.ProcessState == nil
}

// watchTunnelExit 收尸并摘除台账：Start 之后必须有人 Wait，否则进程退出即僵尸；
// 摘除用 CompareAndDelete——只摘自己那条，避免误删重建后的新进程
func watchTunnelExit(resourceID string, cmd *exec.Cmd) {
	err := cmd.Wait()
	liveTunnels.CompareAndDelete(resourceID, cmd)
	if err != nil {
		log.Printf("[tunnel] %s 隧道进程退出：%v", resourceID, err)
	}
}

// startReverseTunnel 拉起一条反向隧道（不阻塞）。返回是否新拉起
func startReverseTunnel(resourceID string, cred *sshCred) (bool, error) {
	if tunnelAlive(resourceID) {
		return false, nil
	}
	cmd := reverseTunnelCmd(cred)
	if err := cmd.Start(); err != nil {
		return false, err
	}
	liveTunnels.Store(resourceID, cmd)
	go watchTunnelExit(resourceID, cmd)
	log.Printf("[tunnel] 反向隧道已建立 %s@%s（目标机 127.0.0.1:%d → 平台 %d）",
		cred.User, cred.Host, tunnelRemotePort(), platformPort())
	return true, nil
}

// ensureTunnelForResource 按资源台账确保隧道在跑。
// 只对已登记隧道语义接入点的资源生效（明确登记为 docker 的跳过——容器目标不走隧道；
// 空 kind 视为 host，兼容存量台账）。返回 (是否新拉起, 是否适用, 错误)
func ensureTunnelForResource(res *storepkg.Resource) (bool, bool, error) {
	if res == nil || strings.TrimSpace(res.TargetKind) == "docker" {
		return false, false, nil
	}
	if !isTunnelAccessPoint(strings.TrimSpace(res.AgentConsoleURL)) {
		return false, false, nil
	}
	cred := sshCredOf(res)
	if cred == nil || cred.Pass == "" {
		return false, true, fmt.Errorf("资源 %s 缺 SSH 凭据，无法自动建隧道", res.ID)
	}
	started, err := startReverseTunnel(res.ID, cred)
	return started, true, err
}

// startTunnelKeeper 常驻守护：每 30s 巡检全部 host 类资源，隧道不在就重建。
// 容器重启后 liveTunnels 清空 → 首轮巡检全量恢复，不再依赖人工跑脚本
func startTunnelKeeper(catDB *storepkg.DB) {
	go func() {
		ticker := time.NewTicker(tunnelCheckInterval)
		defer ticker.Stop()
		for range ticker.C {
			resList, err := catDB.ListResources("", "")
			if err != nil {
				continue
			}
			for _, res := range resList {
				started, applies, err := ensureTunnelForResource(res)
				if !applies {
					continue
				}
				if err != nil {
					log.Printf("[tunnel] %s 隧道维持失败：%v", res.ID, err)
					continue
				}
				if started {
					log.Printf("[tunnel] %s 隧道曾中断，已自动重建", res.ID)
				}
			}
		}
	}()
	log.Printf("[tunnel] 反向隧道守护已启动（每 %s 巡检）", tunnelCheckInterval)
}
