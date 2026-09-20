package bus

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"sync"
)

// Metric 从 Unix Socket 接收的指标
type Metric struct {
	Name   string            `json:"name"`
	Value  float64           `json:"value"`
	Help   string            `json:"help,omitempty"`
	Type   string            `json:"type,omitempty"` // gauge / counter
	Labels map[string]string `json:"labels,omitempty"`
}

// Server Unix Socket 总线服务端
// 插件（如 Vector）通过 bus.sock 以 JSON lines 上报指标
type Server struct {
	socketPath string
	listener   net.Listener
	mu         sync.RWMutex
	handler    func(metrics []Metric)
	stopCh     chan struct{}
	wg         sync.WaitGroup
}

// NewServer 创建总线服务端
func NewServer(socketPath string) *Server {
	return &Server{
		socketPath: socketPath,
		stopCh:     make(chan struct{}),
	}
}

// Start 启动监听
func (s *Server) Start() error {
	// 清理旧 socket 文件
	os.Remove(s.socketPath)

	listener, err := net.Listen("unix", s.socketPath)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", s.socketPath, err)
	}
	s.listener = listener

	// 设置权限：仅 owner 可读写
	if err := os.Chmod(s.socketPath, 0600); err != nil {
		listener.Close()
		return fmt.Errorf("chmod %s: %w", s.socketPath, err)
	}

	fmt.Printf("Bus server listening on %s\n", s.socketPath)

	s.wg.Add(1)
	go s.acceptLoop()

	return nil
}

// SetHandler 设置指标处理回调
func (s *Server) SetHandler(handler func(metrics []Metric)) {
	s.handler = handler
}

// acceptLoop 接受连接并处理
func (s *Server) acceptLoop() {
	defer s.wg.Done()

	for {
		conn, err := s.listener.Accept()
		if err != nil {
			select {
			case <-s.stopCh:
				return
			default:
				continue
			}
		}
		s.wg.Add(1)
		go s.handleConn(conn)
	}
}

// handleConn 处理单个连接，读取 JSON lines
func (s *Server) handleConn(conn net.Conn) {
	defer s.wg.Done()
	defer conn.Close()

	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var metric Metric
		if err := json.Unmarshal(line, &metric); err != nil {
			fmt.Fprintf(os.Stderr, "bus: parse error: %v (line: %s)\n", err, string(line))
			continue
		}

		if s.handler != nil {
			s.handler([]Metric{metric})
		}
	}
}

// Stop 停止总线服务
func (s *Server) Stop() {
	close(s.stopCh)
	if s.listener != nil {
		s.listener.Close()
	}
	s.wg.Wait()
	os.Remove(s.socketPath)
}
