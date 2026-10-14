// SPDX-License-Identifier: Apache-2.0

package trace

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

type failingWriter struct {
	calls int
	short bool
}

func (w *failingWriter) Write(body []byte) (int, error) {
	w.calls++
	if w.short {
		return len(body) - 1, nil
	}
	return 0, errors.New("private writer detail")
}

func TestLoggerJSONLIdentityTimeAndMetadata(t *testing.T) {
	var buffer bytes.Buffer
	logger, err := New(&buffer)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"run_start", "file_start", "llm_start", "llm_end", "tool_start", "tool_end", "file_end", "run_end"} {
		if err := logger.Record(Event{Event: name, File: "a.go", Round: 1, Tool: "file_read", ToolCallID: "read_1", Status: "ok"}); err != nil {
			t.Fatal(err)
		}
	}
	lines := strings.Split(strings.TrimSpace(buffer.String()), "\n")
	if len(lines) != 8 {
		t.Fatal("events are not one JSON value per line")
	}
	for _, line := range lines {
		var event Event
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatal(err)
		}
		if event.RunID != logger.runID || event.ToolCallID != "read_1" {
			t.Fatal("event identity was lost")
		}
		if _, err := time.Parse(time.RFC3339Nano, event.Time); err != nil {
			t.Fatal(err)
		}
		for _, forbidden := range []string{"arguments", "messages", "headers", "reasoning", "content", "api_key"} {
			if strings.Contains(line, `"`+forbidden+`"`) {
				t.Fatal("trace exposed payload fields")
			}
		}
	}
}

func TestLoggerWriteFailureIsStickyAndSanitized(t *testing.T) {
	for _, short := range []bool{false, true} {
		writer := &failingWriter{short: short}
		logger, _ := New(writer)
		first := logger.Record(Event{Event: "run_start"})
		second := logger.Record(Event{Event: "run_end"})
		if !IsError(first) || first != second || writer.calls != 1 || strings.Contains(first.Error(), "private writer detail") {
			t.Fatal("trace failure was hidden, leaked, or retried")
		}
		if short && !errors.Is(first, io.ErrShortWrite) {
			t.Fatal("short-write cause was lost")
		}
	}
	if _, err := New(nil); err == nil {
		t.Fatal("nil trace writer accepted")
	}
}
