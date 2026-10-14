// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// 作用：openArtifacts 函数用于打开输出文件和跟踪文件，如果路径相同则返回错误。
func openArtifacts(outputPath, tracePath string) (*os.File, *os.File, error) {
	if outputPath != "" && tracePath != "" {
		outputAbs, err := filepath.Abs(outputPath)
		if err != nil {
			return nil, nil, err
		}
		traceAbs, err := filepath.Abs(tracePath)
		if err != nil {
			return nil, nil, err
		}
		same := outputAbs == traceAbs
		if runtime.GOOS == "windows" {
			same = strings.EqualFold(outputAbs, traceAbs)
		}
		if same {
			return nil, nil, errors.New("report and trace must use different paths")
		}
	}
	var output, traceFile *os.File
	var err error
	if outputPath != "" {
		output, err = os.OpenFile(outputPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return nil, nil, err
		}
	}
	if tracePath != "" {
		traceFile, err = os.OpenFile(tracePath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			if output != nil {
				created, _ := output.Stat()
				output.Close()
				current, statErr := os.Lstat(outputPath)
				if statErr == nil && created != nil && os.SameFile(created, current) {
					os.Remove(outputPath)
				}
			}
			return nil, nil, err
		}
	}
	return output, traceFile, nil
}
