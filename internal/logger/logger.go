package logger

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// LLMCallRecord represents a single LLM API call log entry.
type LLMCallRecord struct {
	Timestamp    string `json:"timestamp"`
	RouteID      string `json:"route_id"`
	ModelID      string `json:"model_id"`
	Provider     string `json:"provider"`
	TargetModel  string `json:"target_model"`
	DurationMs   int64  `json:"duration_ms"`
	StatusCode   int    `json:"status_code"`
	Error        string `json:"error,omitempty"`
	InputTokens  int    `json:"input_tokens,omitempty"`
	OutputTokens int    `json:"output_tokens,omitempty"`
}

// Logger manages application and LLM call logs.
type Logger struct {
	llmDir     string
	llmMu      sync.Mutex
	appLogPath string
	appFile    *os.File
}

// New creates a new logger.
func New(logDir string) *Logger {
	return &Logger{
		llmDir:     logDir,
		appLogPath: filepath.Join(logDir, "app.log"),
	}
}

// InitSlog sets up slog to write to the app log file.
func (l *Logger) InitSlog(level string) error {
	lv := slog.LevelInfo
	switch strings.ToLower(level) {
	case "debug":
		lv = slog.LevelDebug
	case "warn", "warning":
		lv = slog.LevelWarn
	case "error":
		lv = slog.LevelError
	}

	f, err := os.OpenFile(l.appLogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return err
	}
	l.appFile = f

	mw := io.MultiWriter(os.Stdout, f)
	handler := slog.NewTextHandler(mw, &slog.HandlerOptions{Level: lv})
	slog.SetDefault(slog.New(handler))
	return nil
}

// Close closes the app log file.
func (l *Logger) Close() error {
	if l.appFile != nil {
		return l.appFile.Close()
	}
	return nil
}

// LogLLMCall appends a record to the daily JSONL file.
func (l *Logger) LogLLMCall(record LLMCallRecord) error {
	l.llmMu.Lock()
	defer l.llmMu.Unlock()

	date := time.Now().Format("2006-01-02")
	path := filepath.Join(l.llmDir, fmt.Sprintf("llm-%s.jsonl", date))

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return err
	}
	defer f.Close()

	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(f, "%s\n", data)
	return err
}

// ReadLLMLogs reads LLM logs for a specific date with pagination.
func (l *Logger) ReadLLMLogs(date string, offset, limit int) ([]LLMCallRecord, int, error) {
	path := filepath.Join(l.llmDir, fmt.Sprintf("llm-%s.jsonl", date))
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, 0, nil
		}
		return nil, 0, err
	}
	defer f.Close()

	var all []LLMCallRecord
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var rec LLMCallRecord
		if err := json.Unmarshal(scanner.Bytes(), &rec); err == nil {
			all = append(all, rec)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, 0, err
	}

	total := len(all)
	if offset >= total {
		return nil, total, nil
	}
	end := offset + limit
	if end > total {
		end = total
	}
	return all[offset:end], total, nil
}

// TailAppLog returns the last N lines of the app log.
func (l *Logger) TailAppLog(lines int) ([]string, error) {
	f, err := os.Open(l.appLogPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	// Simple implementation: read all, keep last N lines.
	var all []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		all = append(all, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}

	if len(all) <= lines {
		return all, nil
	}
	return all[len(all)-lines:], nil
}

// ReadAppLog reads the entire app log.
func (l *Logger) ReadAppLog() (string, error) {
	f, err := os.Open(l.appLogPath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	defer f.Close()

	data, err := io.ReadAll(f)
	if err != nil {
		return "", err
	}
	return string(data), nil
}
