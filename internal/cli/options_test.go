// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"errors"
	"flag"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParseDefaults(t *testing.T) {
	repo, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	var diagnostics bytes.Buffer
	got, err := Parse(nil, &diagnostics)
	if err != nil {
		t.Fatal(err)
	}
	want := Options{Repo: repo, Base: "HEAD~1", Head: "HEAD", Format: "markdown", MaxRounds: 8}
	if !reflect.DeepEqual(got, want) || diagnostics.Len() != 0 {
		t.Fatalf("got %+v, want %+v; diagnostics: %s", got, want, diagnostics.String())
	}
}

func TestParseExplicitOptions(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo with spaces")
	background := "  Add timeout handling\nPreserve this text.  "
	args := []string{"--repo", repo, "--base", "main", "--head", "feature", "--background", background,
		"--format", "json", "--output", "review.json", "--trace", "trace.jsonl", "--max-rounds", "20", "--mock"}
	var diagnostics bytes.Buffer
	got, err := Parse(args, &diagnostics)
	if err != nil {
		t.Fatal(err)
	}
	want := Options{Repo: repo, Base: "main", Head: "feature", Background: background,
		Format: "json", Output: "review.json", Trace: "trace.jsonl", MaxRounds: 20, Mock: true}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestParseInvalidOptions(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		text string
	}{
		{"unknown flag", []string{"--unknown"}, "flag provided but not defined"},
		{"positional argument", []string{"extra"}, "positional arguments"},
		{"empty repo", []string{"--repo="}, "--repo"},
		{"blank repo", []string{"--repo=   "}, "--repo"},
		{"empty base", []string{"--base="}, "--base"},
		{"blank base", []string{"--base=   "}, "--base"},
		{"empty head", []string{"--head="}, "--head"},
		{"blank head", []string{"--head=   "}, "--head"},
		{"option-like base", []string{"--base=-main"}, "--base"},
		{"option-like head", []string{"--head=-HEAD"}, "--head"},
		{"invalid format", []string{"--format=text"}, "--format"},
		{"zero rounds", []string{"--max-rounds=0"}, "--max-rounds"},
		{"negative rounds", []string{"--max-rounds=-1"}, "--max-rounds"},
		{"too many rounds", []string{"--max-rounds=21"}, "--max-rounds"},
		{"non-integer rounds", []string{"--max-rounds=abc"}, "invalid value"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var diagnostics bytes.Buffer
			_, err := Parse(tc.args, &diagnostics)
			if err == nil || !strings.Contains(diagnostics.String(), tc.text) {
				t.Fatalf("error = %v, diagnostics = %q", err, diagnostics.String())
			}
		})
	}
}

func TestParseRoundBoundaries(t *testing.T) {
	for _, tc := range []struct {
		arg  string
		want int
	}{{"1", 1}, {"20", 20}} {
		var diagnostics bytes.Buffer
		opts, err := Parse([]string{"--max-rounds", tc.arg}, &diagnostics)
		if err != nil || opts.MaxRounds != tc.want {
			t.Fatalf("rounds %s: got %d, error %v", tc.arg, opts.MaxRounds, err)
		}
	}
}

func TestParseRelativeRepoAndIndependentCalls(t *testing.T) {
	relative := filepath.Join("fixtures", "repo with spaces")
	want, err := filepath.Abs(relative)
	if err != nil {
		t.Fatal(err)
	}
	var diagnostics bytes.Buffer
	opts, err := Parse([]string{"--repo", relative, "--mock", "--format=json"}, &diagnostics)
	if err != nil || opts.Repo != want {
		t.Fatalf("repo = %q, want %q; error = %v", opts.Repo, want, err)
	}
	defaults, err := Parse(nil, &diagnostics)
	if err != nil || defaults.Mock || defaults.Format != "markdown" {
		t.Fatalf("a previous parse affected defaults: %+v, error %v", defaults, err)
	}
}

func TestParseHelp(t *testing.T) {
	var diagnostics bytes.Buffer
	_, err := Parse([]string{"--help"}, &diagnostics)
	if !errors.Is(err, flag.ErrHelp) || !strings.Contains(diagnostics.String(), "Usage of mini-review") {
		t.Fatalf("error = %v, diagnostics = %q", err, diagnostics.String())
	}
}
