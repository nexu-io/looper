package bootstrap

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/powerformer/looper/internal/config"
)

const logFileName = "looperd.log"

var logPriorities = map[config.LogLevel]int{
	config.LogLevelDebug: 10,
	config.LogLevelInfo:  20,
	config.LogLevelWarn:  30,
	config.LogLevelError: 40,
}

type Logger interface {
	Debug(message string, context map[string]any)
	Info(message string, context map[string]any)
	Warn(message string, context map[string]any)
	Error(message string, context map[string]any)
}

type LoggerOptions struct {
	Stdout io.Writer
	Stderr io.Writer
	Now    func() time.Time
}

type logger struct {
	config  config.LoggingConfig
	logPath string
	stdout  io.Writer
	stderr  io.Writer
	now     func() time.Time
	mu      sync.Mutex
}

type logEntry struct {
	Timestamp string          `json:"ts"`
	Level     config.LogLevel `json:"level"`
	Message   string          `json:"message"`
	Context   map[string]any  `json:"context,omitempty"`
}

func CreateLogger(cfg config.LoggingConfig, logDir string, options LoggerOptions) (Logger, error) {
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return nil, fmt.Errorf("create log directory: %w", err)
	}

	return &logger{
		config:  cfg,
		logPath: LogFilePath(logDir),
		stdout:  writerOrDefault(options.Stdout, os.Stdout),
		stderr:  writerOrDefault(options.Stderr, os.Stderr),
		now:     timeSourceOrDefault(options.Now),
	}, nil
}

func LogFilePath(logDir string) string {
	return filepath.Join(logDir, logFileName)
}

func (l *logger) Debug(message string, context map[string]any) {
	l.write(config.LogLevelDebug, message, context)
}

func (l *logger) Info(message string, context map[string]any) {
	l.write(config.LogLevelInfo, message, context)
}

func (l *logger) Warn(message string, context map[string]any) {
	l.write(config.LogLevelWarn, message, context)
}

func (l *logger) Error(message string, context map[string]any) {
	l.write(config.LogLevelError, message, context)
}

func (l *logger) write(level config.LogLevel, message string, context map[string]any) {
	if logPriorities[level] < logPriorities[l.config.Level] {
		return
	}

	entry := logEntry{
		Timestamp: FormatLocalTimestamp(l.now()),
		Level:     level,
		Message:   message,
		Context:   context,
	}

	encoded, err := json.Marshal(entry)
	if err != nil {
		_, _ = fmt.Fprintf(l.stderr, "failed to encode looperd log entry: %v\n", err)
		return
	}

	line := string(encoded) + "\n"

	l.mu.Lock()
	defer l.mu.Unlock()

	if err := appendLogLine(l.logPath, line, l.config); err != nil {
		_, _ = fmt.Fprintf(l.stderr, "failed to write looperd log: %v\n", err)
	}

	target := l.stdout
	if level == config.LogLevelWarn || level == config.LogLevelError {
		target = l.stderr
	}

	_, _ = io.WriteString(target, string(encoded))
	_, _ = io.WriteString(target, "\n")
}

func appendLogLine(path string, line string, cfg config.LoggingConfig) error {
	if err := rotateLogIfNeeded(path, int64(len(line)), cfg); err != nil {
		return err
	}

	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer file.Close()

	_, err = io.WriteString(file, line)
	return err
}

func rotateLogIfNeeded(path string, incomingBytes int64, cfg config.LoggingConfig) error {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}

		return err
	}

	maxBytes := int64(cfg.MaxSizeMB) * 1024 * 1024
	if info.Size()+incomingBytes <= maxBytes {
		return nil
	}

	// Rotation happens before the next line is appended, so the active file may
	// exceed the configured size by at most one encoded log entry.
	return rotateLogFiles(path, cfg.MaxFiles)
}

func rotateLogFiles(path string, maxFiles int) error {
	if err := removeStaleRotatedLogFiles(path, maxFiles); err != nil {
		return err
	}

	if maxFiles <= 1 {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}

		return nil
	}

	oldestArchivePath := rotatedLogFilePath(path, maxFiles-1)
	if err := os.Remove(oldestArchivePath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}

	for index := maxFiles - 2; index >= 1; index-- {
		source := rotatedLogFilePath(path, index)
		target := rotatedLogFilePath(path, index+1)
		if err := renameIfExists(source, target); err != nil {
			return err
		}
	}

	return renameIfExists(path, rotatedLogFilePath(path, 1))
}

func removeStaleRotatedLogFiles(path string, maxFiles int) error {
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}

		return err
	}

	prefix := filepath.Base(path) + "."
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), prefix) {
			continue
		}

		index, err := strconv.Atoi(strings.TrimPrefix(entry.Name(), prefix))
		if err != nil || index < maxFiles {
			continue
		}

		if err := os.Remove(filepath.Join(filepath.Dir(path), entry.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}

	return nil
}

func renameIfExists(source string, target string) error {
	if err := os.Rename(source, target); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}

		return err
	}

	return nil
}

func rotatedLogFilePath(path string, index int) string {
	return path + "." + strconv.Itoa(index)
}

func FormatLocalTimestamp(value time.Time) string {
	return value.Format("2006-01-02T15:04:05.000-07:00")
}

func writerOrDefault(writer io.Writer, fallback io.Writer) io.Writer {
	if writer != nil {
		return writer
	}

	return fallback
}

func timeSourceOrDefault(now func() time.Time) func() time.Time {
	if now != nil {
		return now
	}

	return time.Now
}
