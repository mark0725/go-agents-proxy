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
	Timestamp      string `json:"timestamp"`
	RouteID        string `json:"route_id"`
	ModelID        string `json:"model_id"`
	Provider       string `json:"provider"`
	TargetModel    string `json:"target_model"`
	DurationMs     int64  `json:"duration_ms"`
	StatusCode     int    `json:"status_code"`
	Error          string `json:"error,omitempty"`
	InputTokens    int    `json:"input_tokens,omitempty"`
	OutputTokens   int    `json:"output_tokens,omitempty"`
	StopReason     string `json:"stop_reason,omitempty"`
	RequestBody    string `json:"request_body,omitempty"`
	ResponseBody   string `json:"response_body,omitempty"`
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
	lines, err := readLastLines(path, 5000)
	if err != nil {
		return nil, 0, err
	}

	var all []LLMCallRecord
	for _, line := range lines {
		var rec LLMCallRecord
		if err := json.Unmarshal([]byte(line), &rec); err == nil {
			all = append(all, rec)
		}
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
	return readLastLines(l.appLogPath, lines)
}

func readLastLines(path string, limit int) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	const maxCapacity = 1024 * 1024
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, maxCapacity)

	if limit <= 0 {
		limit = 100
	}
	queue := make([]string, 0, limit)
	for scanner.Scan() {
		if len(queue) == limit {
			copy(queue, queue[1:])
			queue[len(queue)-1] = scanner.Text()
			continue
		}
		queue = append(queue, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return queue, nil
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
