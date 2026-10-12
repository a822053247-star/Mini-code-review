// SPDX-License-Identifier: Apache-2.0

package tool

import (
	"strings"

	"example.com/mini-code-review/internal/model"
)

type findingKey struct {
	path       string
	start, end int
	title      string
}

// Collector 按首次接受顺序保存合法问题，仅用于一次运行中的串行任务。
// 零值可直接使用；写入口留在包内，调用方通过 Commenter 完成校验后提交。
type Collector struct {
	findings []model.Finding
	seen     map[findingKey]struct{}
}

func NewCollector() *Collector { return &Collector{} }

func (collector *Collector) add(finding model.Finding) bool {
	key := findingKey{
		path: finding.Path, start: finding.StartLine, end: finding.EndLine,
		title: strings.ToLower(strings.Join(strings.Fields(finding.Title), " ")),
	}
	if _, exists := collector.seen[key]; exists {
		return true
	}
	if collector.seen == nil {
		collector.seen = make(map[findingKey]struct{})
	}
	collector.seen[key] = struct{}{}
	collector.findings = append(collector.findings, finding)
	return false
}

// Findings 返回独立副本，空集合序列化为 []，不会泄露内部切片。
func (collector *Collector) Findings() []model.Finding {
	findings := make([]model.Finding, len(collector.findings))
	copy(findings, collector.findings)
	return findings
}
