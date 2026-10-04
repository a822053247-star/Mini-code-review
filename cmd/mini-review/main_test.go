// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRunExitCodes(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		code int
		text string
	}{
		{"help", []string{"--help"}, 0, "Usage of mini-review"},
		{"invalid arguments", []string{"--format=text"}, 2, "--format"},
		{"valid arguments", []string{"--mock", "--format=json"}, 1, "Review execution is not implemented yet."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var diagnostics bytes.Buffer
			if code := run(tc.args, &diagnostics); code != tc.code {
				t.Fatalf("exit code = %d, want %d", code, tc.code)
			}
			if !strings.Contains(diagnostics.String(), tc.text) {
				t.Fatalf("diagnostics = %q", diagnostics.String())
			}
		})
	}
}
