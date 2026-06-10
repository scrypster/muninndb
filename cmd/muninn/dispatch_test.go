package main

import (
	"testing"
)

func TestParseSubcommand(t *testing.T) {
	cases := []struct {
		args []string
		want string
	}{
		{[]string{}, ""}, // no args → printQuickStart
		{[]string{"shell"}, "shell"},
		{[]string{"start"}, "start"},
		{[]string{"stop"}, "stop"},
		{[]string{"status"}, "status"},
		{[]string{"restart"}, "restart"},
		{[]string{"start", "web"}, "start:web"},
		{[]string{"stop", "web"}, "stop:web"},
		{[]string{"show", "vaults"}, "show:vaults"},
		{[]string{"help"}, "help"},
		{[]string{"start", "--dev"}, "start"}, // flags are not subcommands
		{[]string{"init", "--yes"}, "init"},   // flags are not subcommands
	}
	for _, tc := range cases {
		got := parseSubcommand(tc.args)
		if got != tc.want {
			t.Errorf("parseSubcommand(%v) = %q, want %q", tc.args, got, tc.want)
		}
	}
}

func TestParseSubcommandLogs(t *testing.T) {
	if parseSubcommand([]string{"logs"}) != "logs" {
		t.Error("logs should parse as logs")
	}
	if parseSubcommand([]string{"completion", "bash"}) != "completion:bash" {
		t.Error("completion bash should parse as completion:bash")
	}
	if parseSubcommand([]string{"completion", "zsh"}) != "completion:zsh" {
		t.Error("completion zsh should parse as completion:zsh")
	}
}
