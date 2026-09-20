package subprocess

import (
	"fmt"
	"os"
	"os/exec"
	"sync"
	"time"
)

// Plugin 子进程插件
type Plugin struct {
	Name       string
	Command    string        // 可执行文件路径
	Args       []string      // 命令行参数
	WorkDir    string        // 工作目录
	Env        []string      // 环境变量
	crashLimit int           // 窗口内最大崩溃次数
	crashWin   time.Duration // 崩溃计数窗口

	cmd        *exec.Cmd
	mu         sync.Mutex
	stopCh     chan struct{}
	doneCh     chan struct{}
	crashCount int
	crashStart time.Time
	running    bool
}

// New 创建子进程插件
func New(name, command string, args []string) *Plugin {
	return &Plugin{
		Name:       name,
		Command:    command,
		Args:       args,
		crashLimit: 10,
		crashWin:   60 * time.Second,
		stopCh:     make(chan struct{}, 1),
	}
}

// Start 启动子进程，崩溃自动拉起
func (p *Plugin) Start() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.running {
		return fmt.Errorf("plugin %s already running", p.Name)
	}

	p.running = true
	p.doneCh = make(chan struct{})

	go p.monitorLoop()
	return nil
}

// monitorLoop 监控子进程，崩溃后自动拉起
func (p *Plugin) monitorLoop() {
	defer close(p.doneCh)

	for {
		select {
		case <-p.stopCh:
			return
		default:
		}

		if err := p.startProcess(); err != nil {
			fmt.Fprintf(os.Stderr, "plugin %s: start failed: %v\n", p.Name, err)
		}

		// 等待进程退出
		if p.cmd != nil {
			p.cmd.Wait()
		}

		// 检查是否被主动停止
		select {
		case <-p.stopCh:
			return
		default:
		}

		// 崩溃限流：窗口内 crash 次数超过限制则不再拉起
		if p.tooManyCrashes() {
			fmt.Fprintf(os.Stderr, "plugin %s: too many crashes, giving up\n", p.Name)
			return
		}

		fmt.Fprintf(os.Stderr, "plugin %s: exited, restarting in 2s...\n", p.Name)
		time.Sleep(2 * time.Second)
	}
}

// startProcess 启动子进程
func (p *Plugin) startProcess() error {
	p.cmd = exec.Command(p.Command, p.Args...)
	if p.WorkDir != "" {
		p.cmd.Dir = p.WorkDir
	}
	if len(p.Env) > 0 {
		p.cmd.Env = append(os.Environ(), p.Env...)
	}
	p.cmd.Stdout = os.Stdout
	p.cmd.Stderr = os.Stderr

	// 设置进程组，便于终止整个进程树

	fmt.Printf("plugin %s: starting %s %v\n", p.Name, p.Command, p.Args)
	return p.cmd.Start()
}

// Stop 停止子进程
func (p *Plugin) Stop() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if !p.running {
		return nil
	}

	// 发送停止信号
	select {
	case p.stopCh <- struct{}{}:
	default:
	}

	// 终止进程
	if p.cmd != nil && p.cmd.Process != nil {
		if err := p.cmd.Process.Kill(); err != nil {
			fmt.Fprintf(os.Stderr, "plugin %s: kill error: %v\n", p.Name, err)
		} else {
			// Wait for OS to reap
			done := make(chan struct{})
			go func() { p.cmd.Wait(); close(done) }()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
			}
		}
	}

	// 等待 monitorLoop 退出（最多等待 3s）
	if p.doneCh != nil {
		select {
		case <-p.doneCh:
		case <-time.After(3 * time.Second):
			fmt.Fprintf(os.Stderr, "plugin %s: stop timeout waiting for monitorLoop\n", p.Name)
		}
	}

	p.running = false
	fmt.Printf("plugin %s: stopped\n", p.Name)
	return nil
}

// Running 返回是否在运行
func (p *Plugin) Running() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.running
}

// PluginName 返回插件名（实现 server.PluginHandle 接口）
func (p *Plugin) PluginName() string {
	return p.Name
}

// Signal 发送信号给插件进程（实现 server.PluginHandle 接口）
func (p *Plugin) Signal(sig os.Signal) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cmd == nil || p.cmd.Process == nil {
		return fmt.Errorf("plugin %s: no process", p.Name)
	}
	return p.cmd.Process.Signal(sig)
}

// tooManyCrashes 检查是否在时间窗口内崩溃次数超限
func (p *Plugin) tooManyCrashes() bool {
	now := time.Now()
	if p.crashStart.IsZero() || now.Sub(p.crashStart) > p.crashWin {
		p.crashCount = 0
		p.crashStart = now
	}
	p.crashCount++
	return p.crashCount > p.crashLimit
}
