package main

import "fmt"

// muninnCommands is the canonical list of all top-level subcommands.
var muninnCommands = []string{
	"init", "start", "stop", "restart", "status",
	"shell", "logs", "mcp", "show", "completion", "help",
}

func printCompletion(shell string) {
	switch shell {
	case "bash":
		fmt.Print(bashCompletion)
	case "zsh":
		fmt.Print(zshCompletion)
	case "fish":
		fmt.Print(fishCompletion)
	default:
		fmt.Printf("Unknown shell %q. Supported: bash, zsh, fish\n", shell)
	}
}

const bashCompletion = `# muninn bash completion
# Add to ~/.bashrc:  source <(muninn completion bash)

_muninn_completions() {
    local cur="${COMP_WORDS[COMP_CWORD]}"
    local commands="init start stop restart status shell logs mcp show completion help"
    COMPREPLY=($(compgen -W "${commands}" -- "${cur}"))
}
complete -F _muninn_completions muninn
`

const zshCompletion = `#compdef muninn
# muninn zsh completion
# Add to ~/.zshrc:  source <(muninn completion zsh)

_muninn() {
    local -a commands
    commands=(
        'init:First-time setup wizard'
        'start:Start all services'
        'stop:Stop the running server'
        'restart:Stop and restart'
        'status:Show service health'
        'shell:Interactive memory explorer'
        'logs:Tail the log file'
        'mcp:stdio→HTTP MCP proxy (for OpenClaw)'
        'completion:Generate shell completion script'
        'help:Show help'
    )
    _describe 'commands' commands
}

_muninn
`

const fishCompletion = `# muninn fish completion
# Add to fish config:  muninn completion fish | source

complete -c muninn -f
complete -c muninn -n __fish_use_subcommand -a init      -d 'First-time setup wizard'
complete -c muninn -n __fish_use_subcommand -a start     -d 'Start all services'
complete -c muninn -n __fish_use_subcommand -a stop      -d 'Stop the running server'
complete -c muninn -n __fish_use_subcommand -a restart   -d 'Stop and restart'
complete -c muninn -n __fish_use_subcommand -a status    -d 'Show service health'
complete -c muninn -n __fish_use_subcommand -a shell     -d 'Interactive memory explorer'
complete -c muninn -n __fish_use_subcommand -a logs      -d 'Tail the log file'
complete -c muninn -n __fish_use_subcommand -a mcp       -d 'stdio→HTTP MCP proxy (for OpenClaw)'
complete -c muninn -n __fish_use_subcommand -a completion -d 'Generate shell completion script'
complete -c muninn -n __fish_use_subcommand -a help      -d 'Show help'
`
