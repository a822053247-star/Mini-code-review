// SPDX-License-Identifier: Apache-2.0

// Package trace writes metadata-only JSONL execution events.
package trace

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"time"

	"example.com/mini-code-review/internal/llm"
)

type Event struct {
	RunID      string          `json:"run_id"`
	Time       string          `json:"time"`
	Event      string          `json:"event"`
	File       string          `json:"file,omitempty"`
	Round      int             `json:"round,omitempty"`
	Tool       string          `json:"tool,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
	DurationMS *int64          `json:"duration_ms,omitempty"`
	Status     string          `json:"status,omitempty"`
	Code       string          `json:"code,omitempty"`
	HTTPStatus int             `json:"http_status,omitempty"`
	StopReason string          `json:"stop_reason,omitempty"`
	Usage      *llm.TokenUsage `json:"usage,omitempty"`
}

type Sink func(Event) error

type Error struct{ cause error }

func (err *Error) Error() string { return "trace recording failed" }
func (err *Error) Unwrap() error { return err.cause }
func Failed(err error) error     { return &Error{cause: err} }
func IsError(err error) bool     { var failure *Error; return errors.As(err, &failure) }

type Logger struct {
	writer io.Writer
	runID  string
	err    error
}

func New(writer io.Writer) (*Logger, error) {
	if writer == nil {
		return nil, errors.New("trace writer must be configured")
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return nil, Failed(err)
	}
	return &Logger{writer: writer, runID: hex.EncodeToString(id[:])}, nil
}

// 作用：Record 方法用于记录代码审查任务的事件，包括工具调用、完成任务等。
func (logger *Logger) Record(event Event) error {
	if logger.err != nil {
		return logger.err
	}
	event.RunID, event.Time = logger.runID, time.Now().UTC().Format(time.RFC3339Nano)
	encoded, err := json.Marshal(event)
	if err == nil {
		encoded = append(encoded, '\n')
		var written int
		written, err = logger.writer.Write(encoded)
		if err == nil && written != len(encoded) {
			err = io.ErrShortWrite
		}
	}
	if err != nil {
		logger.err = Failed(err)
	}
	return logger.err
}

// 作用：Duration 函数用于计算两个时间点之间的毫秒数。
func Duration(start time.Time) *int64 { elapsed := time.Since(start).Milliseconds(); return &elapsed }
