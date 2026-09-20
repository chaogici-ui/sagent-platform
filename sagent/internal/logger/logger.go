package logger

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
)

// Level 日志级别
type Level int

const (
	DEBUG Level = iota
	INFO
	WARN
	ERROR
)

var levelNames = map[Level]string{DEBUG: "DEBUG", INFO: "INFO", WARN: "WARN", ERROR: "ERROR"}

// Logger 分级日志
type Logger struct {
	level   Level
	stdLog  *log.Logger
	fileLog *log.Logger
	file    *os.File
}

// New 创建日志器
func New(logDir, logFile string, level Level) (*Logger, error) {
	l := &Logger{level: level}

	// stderr writer（始终输出）
	l.stdLog = log.New(os.Stderr, "", log.LstdFlags)

	// 文件 writer
	if logDir != "" && logFile != "" {
		if err := os.MkdirAll(logDir, 0755); err != nil {
			return nil, fmt.Errorf("create log dir: %w", err)
		}
		fullPath := filepath.Join(logDir, logFile)
		f, err := os.OpenFile(fullPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			return nil, fmt.Errorf("open log file: %w", err)
		}
		l.file = f
		l.fileLog = log.New(io.MultiWriter(os.Stderr, f), "", log.LstdFlags)
	} else {
		l.fileLog = log.New(os.Stderr, "", log.LstdFlags)
	}

	l.Info("logger initialized, level=%s", levelNames[level])
	return l, nil
}

// Close 关闭日志文件
func (l *Logger) Close() {
	if l.file != nil {
		l.file.Close()
	}
}

func (l *Logger) logf(level Level, format string, args ...any) {
	if level < l.level {
		return
	}
	msg := fmt.Sprintf("[%s] %s", levelNames[level], fmt.Sprintf(format, args...))
	l.fileLog.Println(msg)
}

func (l *Logger) Debug(format string, args ...any) { l.logf(DEBUG, format, args...) }
func (l *Logger) Info(format string, args ...any)  { l.logf(INFO, format, args...) }
func (l *Logger) Warn(format string, args ...any)  { l.logf(WARN, format, args...) }
func (l *Logger) Error(format string, args ...any) { l.logf(ERROR, format, args...) }

// StdLogger 返回标准库 *log.Logger（兼容旧代码）
func (l *Logger) StdLogger() *log.Logger {
	return log.New(io.MultiWriter(os.Stderr, l.file), "", log.LstdFlags)
}
