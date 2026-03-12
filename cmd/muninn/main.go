package main

import (
	"fmt"
	"os"
	"strings"
)

func main() {
	if len(os.Args) < 2 {
		runDefault()
		return
	}

	// --daemon flag: run server inline (forked by runStart)
	for i, arg := range os.Args[1:] {
		if arg == "--daemon" {
			os.Args = append(os.Args[:i+1], os.Args[i+2:]...)
			runServer()
			return
		}
	}

	sub := parseSubcommand(os.Args[1:])
	rest := os.Args[2:]

	// For commands without FlagSets, check for -h/--help and show subcommand help
	if sub != "" && sub != "help" && sub != "--help" && sub != "-h" {
		helpKey := sub
		if strings.Contains(helpKey, ":") {
			helpKey = strings.Replace(helpKey, ":", " ", 1)
		}
		if wantsHelp(rest) {
			if fn, ok := subcommandHelp[helpKey]; ok {
				fn()
				return
			}
		}
	}

	switch sub {
	case "":
		runDefault()
	case "init":
		runInit()
	case "shell":
		runShell()
	case "start":
		if err := runStart(true); err != nil {
			os.Exit(1)
		}
	case "start:web":
		runStartService("web")
	case "stop":
		runStop()
	case "stop:web":
		runStopService("web")
	case "status":
		runStatus()
	case "backup":
		runBackup(rest)
	case "upgrade":
		runUpgrade(rest)
	case "restart":
		runStop()
		if err := runStart(true); err != nil {
			os.Exit(1)
		}
	case "show:vaults":
		runShowVaults()
	case "mcp":
		runMCPStdio()
	case "logs":
		runLogs(rest)
	case "cluster":
		runCluster(rest)
	case "completion:bash":
		printCompletion("bash")
	case "completion:zsh":
		printCompletion("zsh")
	case "completion:fish":
		printCompletion("fish")
	case "completion":
		fmt.Fprintln(os.Stderr, "Usage: muninn completion <bash|zsh|fish>")
		os.Exit(1)
	case "help", "--help", "-h":
		printHelp()
	case "version", "--version":
		fmt.Println(muninnVersion())
		return
	default:
		// Container commands: "vault" or "vault:delete" both route to runVault.
		// parseSubcommand joins two-word commands with ":", so we match by prefix.
		// rest (os.Args[2:]) includes the subcommand word for dispatch.
		if sub == "vault" || strings.HasPrefix(sub, "vault:") {
			runVault(rest)
			return
		}
		if sub == "api-key" || strings.HasPrefix(sub, "api-key:") {
			runAPIKey(rest)
			return
		}
		if sub == "admin" || strings.HasPrefix(sub, "admin:") {
			runAdmin(rest)
			return
		}
		if strings.HasPrefix(sub, "cluster:") {
			runCluster(rest)
			return
		}
		// muninn help <subcommand> → show subcommand help
		if strings.HasPrefix(sub, "help:") {
			helpKey := strings.TrimPrefix(sub, "help:")
			if fn, ok := subcommandHelp[helpKey]; ok {
				fn()
				return
			}
		}
		fmt.Fprintf(os.Stderr, "Unknown command: %q\n\n", sub)
		fmt.Fprintln(os.Stderr, "Run 'muninn help' to see all commands.")
		os.Exit(1)
	}
}

// runDefault is what happens when the user types bare `muninn`.
// Three states:
//  1. No data dir → first time → launch wizard
//  2. Data dir exists, not running → show status + exit
//  3. Running → show status flash + drop into shell
func runDefault() {
	_, err := os.Stat(defaultDataDir())
	if os.IsNotExist(err) {
		// First run — launch wizard directly
		runInit()
		return
	}

	// Show status to determine state
	state := printStatusDisplay(true)

	switch state {
	case stateStopped:
		// hints already printed by printStatusDisplay
	case stateDegraded:
		// fix command already printed by printStatusDisplay
	case stateRunning:
		// Drop into shell
		runShell()
	}
}
