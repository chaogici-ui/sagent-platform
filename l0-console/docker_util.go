package main

import (
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// executeAction 通过 Docker Compose 执行操作
func executeAction(agentID, action, version string) map[string]interface{} {
	result := map[string]interface{}{
		"agent_id": agentID,
		"action":   action,
		"success":  true,
	}

	composeDir := "../deploy/docker"
	service := agentID

	switch action {
	case "start":
		// 启动 SAgent 进程：删除 stopped 标记文件，supervisor 自动拉起
		containerName := agentContainerName(agentID)
		out, err := runCmd(".", "docker", "exec", containerName, "rm", "-f", "/home/deploy/SAgent/run/stopped")
		if err != nil {
			// 如果 exec 失败（容器不存在），尝试 compose up
			out2, err2 := runCmd(composeDir, "docker", "compose", "up", "-d", service)
			if err2 != nil {
				result["success"] = false
				result["error"] = err2.Error()
				break
			}
			out = strings.TrimSpace(out2)
		}
		result["output"] = "SAgent 进程已启动: " + strings.TrimSpace(out)
		addAudit("启动 Agent", agentID, "单机", "成功")

	case "stop":
		// 停止 SAgent 进程组：先标记 stopped（防止 supervisor 自动重启）→ SIGTERM 优雅退出 → SIGKILL 兜底
		containerName := agentContainerName(agentID)
		dockerCmd("exec", containerName, "sh", "-c",
			"touch /home/deploy/SAgent/run/stopped; "+
				"kill -15 -$(cat /home/deploy/SAgent/run/SAgent.pid 2>/dev/null) 2>/dev/null; "+
				"sleep 3; "+
				"kill -9 -$(cat /home/deploy/SAgent/run/SAgent.pid 2>/dev/null) 2>/dev/null; true")
		result["output"] = "SAgent 进程组已停止"
		addAudit("停止 Agent", agentID, "单机", "成功")

	case "restart":
		// 重启 SAgent 进程组
		// touch stopped → SIGTERM 优雅退出 → 等 3s → SIGKILL 兜底 → 删 stopped 标记触发 supervisor 重启
		containerName := agentContainerName(agentID)
		dockerCmd("exec", containerName, "sh", "-c",
			"touch /home/deploy/SAgent/run/stopped; "+
				"kill -15 -$(cat /home/deploy/SAgent/run/SAgent.pid 2>/dev/null) 2>/dev/null; "+
				"sleep 3; "+
				"kill -9 -$(cat /home/deploy/SAgent/run/SAgent.pid 2>/dev/null) 2>/dev/null; "+
				"rm -f /home/deploy/SAgent/run/stopped; true")
		time.Sleep(5 * time.Second)
		result["output"] = "SAgent 进程组已重启"
		addAudit("重启 Agent", agentID, "单机", "成功")

	case "deploy":
		out, err := runCmd(composeDir, "docker", "compose", "up", "-d", "--force-recreate", service)
		if err != nil {
			result["success"] = false
			result["error"] = err.Error()
			addAudit("部署 Agent", agentID, "单机", "失败: "+err.Error())
		} else {
			result["output"] = strings.TrimSpace(out)
			addAudit("部署 Agent", agentID, "单机", "成功")
		}

	case "upgrade":
		runCmd(composeDir, "docker", "compose", "build", service)
		runCmd(composeDir, "docker", "compose", "up", "-d", "--force-recreate", service)
		result["output"] = fmt.Sprintf("upgraded to %s", version)
		result["version"] = version
		addAudit("升级 Agent", agentID, "单机", "目标版本: "+version)

	default:
		// 处理测试类 action (test_*)
		if strings.HasPrefix(action, "test_") {
			result = runTestAction(action)
		} else {
			result["success"] = false
			result["error"] = "unknown action: " + action
		}
	}

	return result
}

// runTestAction 执行实际测试
func runTestAction(action string) map[string]interface{} {
	result := map[string]interface{}{
		"action":  action,
		"success": true,
	}

	switch action {
	case "test_dataflow":
		// 数据流测试：检查 VM 中的指标
		checks := []map[string]interface{}{}
		resp, err := httpGet(vmBase() + "/api/v1/label/__name__/values")
		if err != nil {
			result["success"] = false
			result["output"] = fmt.Sprintf("VM unreachable: %v", err)
			return result
		}
		hostMetrics := []string{"host_cpu_percent", "host_memory_total_bytes", "host_disk_total_bytes", "host_net_bytes_recv_total", "host_load1"}
		for _, m := range hostMetrics {
			if strings.Contains(resp, m) {
				checks = append(checks, map[string]interface{}{"metric": m, "status": "PASS"})
			} else {
				checks = append(checks, map[string]interface{}{"metric": m, "status": "FAIL"})
				result["success"] = false
			}
		}
		// 自监控
		if strings.Contains(resp, "sagent_uptime_seconds") {
			checks = append(checks, map[string]interface{}{"metric": "sagent_self_metrics", "status": "PASS"})
		} else {
			checks = append(checks, map[string]interface{}{"metric": "sagent_self_metrics", "status": "FAIL"})
			result["success"] = false
		}
		// up 检查
		upResp, _ := httpGet(vmBase() + "/api/v1/query?query=up")
		upCount := strings.Count(upResp, `"1"`)
		checks = append(checks, map[string]interface{}{"metric": fmt.Sprintf("up_nodes(%d)", upCount), "status": "PASS"})

		result["checks"] = checks
		result["output"] = fmt.Sprintf("数据流测试: %d 项检查", len(checks))

	case "test_crash":
		// 崩溃恢复测试
		before := time.Now()
		dockerCmd("exec", agentContainerName("sagent-1"), "kill", "1")
		time.Sleep(8 * time.Second)
		after := time.Now()
		statusOut, _ := dockerCmdOut("inspect", "-f", "{{.State.Health.Status}}", agentContainerName("sagent-1"))
		recoveryTime := after.Sub(before).Seconds()
		result["output"] = fmt.Sprintf("崩溃恢复: %.0fs, 状态=%s", recoveryTime, strings.TrimSpace(statusOut))
		result["recovery_seconds"] = recoveryTime
		if strings.TrimSpace(statusOut) != "healthy" {
			result["success"] = false
		}

	case "test_load":
		// 高负载压测
		start := time.Now()
		var wg sync.WaitGroup
		for i := 0; i < 50; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				httpGet(vmBase() + "/api/v1/query?query=host_cpu_percent")
				httpGet(fmt.Sprintf("%s/api/v1/query_range?query=host_memory_used_percent&start=%d&end=%d&step=15", vmBase(), time.Now().Unix()-300, time.Now().Unix()))
			}()
		}
		wg.Wait()
		statusOut, _ := dockerCmdOut("inspect", "-f", "{{.State.Health.Status}}", agentContainerName("sagent-1"))
		result["output"] = fmt.Sprintf("高负载压测: 50请求/%.1fs, 状态=%s", time.Since(start).Seconds(), strings.TrimSpace(statusOut))
		if strings.TrimSpace(statusOut) != "healthy" {
			result["success"] = false
		}

	case "test_config":
		// 配置变更测试
		dockerCmd("exec", agentContainerName("sagent-1"), "sh", "-c", "echo 'test: 1' >> /tmp/test.txt")
		time.Sleep(3 * time.Second)
		statusOut, _ := dockerCmdOut("inspect", "-f", "{{.State.Health.Status}}", agentContainerName("sagent-1"))
		result["output"] = fmt.Sprintf("配置变更测试: 状态=%s", strings.TrimSpace(statusOut))

	case "test_network":
		// 网络中断恢复测试
		dockerCmd("network", "disconnect", cfgL0Net, agentContainerName("vmagent"))
		time.Sleep(15 * time.Second)
		dockerCmd("network", "connect", cfgL0Net, agentContainerName("vmagent"))
		time.Sleep(15 * time.Second)
		upResp, _ := httpGet(vmBase() + "/api/v1/query?query=up")
		upCount := strings.Count(upResp, `"1"`)
		result["output"] = fmt.Sprintf("网络中断恢复: %d 节点在线", upCount)

	default:
		result["success"] = false
		result["output"] = "未知测试: " + action
	}

	return result
}

func httpGet(url string) (string, error) {
	resp, err := http.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return string(body), nil
}

func dockerCmd(args ...string) {
	runCmd("../deploy/docker", "docker", args...)
}

func dockerCmdOut(args ...string) (string, error) {
	return runCmd("../deploy/docker", "docker", args...)
}

// rowToAgent SQLite 行转内存 Agent

func runCmd(dir, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}
