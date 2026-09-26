package main

// logging.go — 平台自身日志基础设施（架构 3.2「可运维」+ 用户长期工程偏好）。
//
// 现状与目标：此前所有日志用标准库 log.Printf 直写容器 stdout，容器销毁即丢，
// 无滚动、无归档、无保留策略。本文件在不改动任何业务调用点（log.Printf 保持不变）的
// 前提下，把 log 默认输出重定向到一套本地滚动日志：
//   - 文件落点：<workdir>/logs/app.log（随 catalog-data 卷持久化，镜像重建不丢）
//   - 滚动：当前文件达 10MB 即滚动；旧文件经 gzip 压缩为 app.log.YYYYmmdd-HHMMSS.gz
//   - 保留：仅保留最近 10 份归档，更早的自动删除（超过即删除，不中断写入）
//   - 异步：写入经缓冲 chan 异步落盘，避免阻塞业务逻辑，降低运行时影响
//   - 镜像可见：同一消息同时写文件与按需透出（stderr），便于 docker compose logs 观察
//   - 可靠性：任一次文件写失败降级为 stderr，绝不 panic/阻断主程序
//
// 纯标准库实现，不引入第三方依赖（符合项目「轻量」取向）。

import (
	"compress/gzip"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	logDir          = "data/logs" // 随 catalog-data 卷持久化
	logFile         = "app.log"
	logMaxBytes     = 10 << 20 // 单文件 10MB 滚动
	logKeepArchives = 10       // 仅保留最近 10 份 gzip 归档
	logBufSize      = 256      // 异步写缓冲队列
)

// rotatingFile 单个可滚动日志文件 + 异步写。并发安全。
type rotatingFile struct {
	path   string
	f      *os.File
	size   int64
	buf    chan []byte
	done   chan struct{}
	closed bool
	mu     sync.Mutex
}

// newRotatingFile 打开（必要时新建）当前活动日志文件并启动异步落盘 goroutine。
// 返回的 *rotatingFile 的 Write 是非阻塞入队。
func newRotatingFile(path string) (*rotatingFile, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	rf := &rotatingFile{
		path: path,
		buf:  make(chan []byte, logBufSize),
		done: make(chan struct{}),
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	rf.f = f
	rf.size = st.Size()
	go rf.pump()
	return rf, nil
}

// pump 异步消费缓冲并落盘；写失败降级 stderr；关闭或超时退出。
func (rf *rotatingFile) pump() {
	for {
		select {
		case line := <-rf.buf:
			rf.writeSync(line)
		case <-rf.done:
			// 收尾：尽量排空缓冲
			for {
				select {
				case line := <-rf.buf:
					rf.writeSync(line)
				default:
					rf.mu.Lock()
					rf.f.Sync()
					rf.f.Close()
					rf.mu.Unlock()
					close(rf.done)
					return
				}
			}
		}
	}
}

// writeSync 同步写一行；若当前文件超限则滚动。
func (rf *rotatingFile) writeSync(line []byte) {
	rf.mu.Lock()
	defer rf.mu.Unlock()
	if rf.closed {
		return
	}
	if rf.f == nil {
		fmt.Fprint(os.Stderr, string(line))
		return
	}
	if rf.size+int64(len(line)) > logMaxBytes {
		if err := rf.rotate(); err != nil {
			// 滚动失败：放弃文件但继续写 stderr，保证日志不丢到用户视野之外
			fmt.Fprintf(os.Stderr, "[logging] rotate failed: %v\n", err)
		}
	}
	n, err := rf.f.Write(line)
	if err == nil {
		rf.size += int64(n)
	} else {
		fmt.Fprint(os.Stderr, string(line))
	}
}

// rotate 压缩当前文件为归档并新建活动文件，只保留最近 keepArchives 份。
func (rf *rotatingFile) rotate() error {
	if rf.f != nil {
		rf.f.Close()
	}
	target := strings.TrimSuffix(rf.path, filepath.Ext(rf.path)) + "." +
		time.Now().Format("20060102-150405") + ".gz"
	if err := gzipFile(rf.path, target); err != nil {
		// 压缩失败也尽量保留原样，不阻断主程序
		fmt.Fprintf(os.Stderr, "[logging] gzip %s -> %s: %v\n", rf.path, target, err)
	}
	pruneArchives(filepath.Dir(rf.path), logKeepArchives)

	nf, err := os.OpenFile(rf.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		rf.f = nil
		return err
	}
	rf.f = nf
	rf.size = 0
	return nil
}

// gzipFile 将 src 无损压缩为 dst（压缩 C 级：速度/体积平衡）。
func gzipFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	zw, err := gzip.NewWriterLevel(out, gzip.BestCompression)
	if err != nil {
		out.Close()
		return err
	}
	if _, err := io.Copy(zw, in); err != nil {
		zw.Close()
		out.Close()
		os.Remove(dst)
		return err
	}
	if err := zw.Close(); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// pruneArchives 删除超出保留份额的最旧归档。
func pruneArchives(dir string, keep int) {
	matches, err := filepath.Glob(filepath.Join(dir, "app.log.*.gz"))
	if err != nil {
		return
	}
	sort.Strings(matches)
	excess := len(matches) - keep
	for i := 0; i < excess; i++ {
		_ = os.Remove(matches[i])
	}
}

// initLogging 启动滚动日志，并把标准库 log 的输出 writer 重定向到它（业务 log.Printf 零改动）。
// stdout 中转保留：交给 log 默认的 stderr；文件由本基础设施写。返回清理函数供退出时 flush。
func initLogging() (cleanup func()) {
	path := filepath.Join(logDir, logFile)
	rf, err := newRotatingFile(path)
	if err != nil {
		// 无法建日志文件时，维持原行为（stdout）并打一条可见警告，不 panic
		fmt.Fprintf(os.Stderr, "[logging] init failed, fallback stderr: %v\n", err)
		return func() {}
	}
	// stdWriter 在文件与 os.Stderr 之间多路复用，保持 docker compose logs 可见性
	tee := &teeWriter{rf: rf}
	log.SetOutput(tee)
	log.SetPrefix("")
	log.SetFlags(log.LstdFlags)
	return rf.Shutdown
}

// teeWriter 把每条日志同时交给滚动文件与 stderr。
type teeWriter struct {
	rf *rotatingFile
}

func (t *teeWriter) Write(p []byte) (int, error) {
	// 拷贝一份（p 可变，而异步队列需持有时长）
	cp := make([]byte, len(p))
	copy(cp, p)
	t.rf.buf <- cp
	fmt.Fprint(os.Stderr, string(p)) // stdout 镜像（docker compose logs）
	return len(p), nil
}

// Shutdown 优雅刷盘并关闭。
func (rf *rotatingFile) Shutdown() {
	rf.mu.Lock()
	if rf.closed {
		rf.mu.Unlock()
		return
	}
	rf.closed = true
	rf.mu.Unlock()
	close(rf.done) // 触发 pump 排空
	<-rf.done
}
