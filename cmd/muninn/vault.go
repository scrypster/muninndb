package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"sort"
	"strings"
	"time"
)

func printVaultUsage() {
	fmt.Println("Usage: muninn vault <command> [flags]")
	fmt.Println()
	fmt.Println("Commands:")
	fmt.Println("  create      <name> [--public]                    Create and register a new vault")
	fmt.Println("  list        [--pattern <glob>]                   List all vaults")
	fmt.Println("  delete      <name> [--yes] [--force]             Delete a vault and all its memories")
	fmt.Println("  clear       <name> [--yes] [--force]             Remove all memories from a vault")
	fmt.Println("  clone       <source> <new-name>                  Clone a vault into a new vault")
	fmt.Println("  merge       <source> <target> [--delete-source] [--yes]  Merge source into target vault")
	fmt.Println("  export      --vault <name> [--output <file>] [--reset-metadata]  Export vault to .muninn archive")
	fmt.Println("  export-markdown --vault <name> [--output <file>] [--all-vaults]  Export vault notes to markdown .tgz")
	fmt.Println("  import      <file> --vault <name> [--reset-metadata]             Import .muninn archive into new vault")
	fmt.Println("  reindex-fts <name>                               Rebuild FTS index with Porter2 stemming")
	fmt.Println("  rename      <old-name> <new-name>                  Rename a vault (metadata only)")
	fmt.Println("  reembed     <name>                               Clear embeddings and re-embed with current model")
	fmt.Println("  recall-mode <vault> [mode]                        Get or set default recall mode")
	fmt.Println()
	fmt.Println("Auth flags (MySQL-style, optional):")
	fmt.Println("  -u <user>         Admin username (default: root)")
	fmt.Println("  -p                Prompt for password")
	fmt.Println("  -p<password>      Inline password (no space)")
	fmt.Println("  -h <host:port>    Server host:port (default: localhost:8475)")
}

func runVault(args []string) {
	if len(args) == 0 {
		printVaultUsage()
		return
	}

	// Parse auth flags (-u, -p, -h), leaving the subcommand and its args.
	remaining, username, password, prompted := parseAdminFlags(args)
	if len(remaining) == 0 {
		printVaultUsage()
		return
	}

	sub := remaining[0]
	subArgs := remaining[1:]

	// "list" uses the public API — no admin auth needed.
	if sub == "list" {
		runVaultList(subArgs)
		return
	}

	// Validate the subcommand before authenticating so typos get fast feedback.
	switch sub {
	case "create", "delete", "clear", "clone", "merge", "rename", "export", "export-markdown", "import", "reindex-fts", "reembed", "recall-mode":
	default:
		fmt.Printf("Unknown vault command: %q\n", sub)
		printVaultUsage()
		return
	}

	// Authenticate with the admin API.
	if err := authenticateAdmin(username, password, prompted); err != nil {
		fmt.Fprintf(os.Stderr, "Authentication failed: %v\n", err)
		fmt.Fprintln(os.Stderr, "Is muninn running? Try: muninn status")
		osExit(1)
		return
	}

	switch sub {
	case "create":
		runVaultCreate(subArgs)
	case "delete":
		runVaultDelete(subArgs)
	case "clear":
		runVaultClear(subArgs)
	case "clone":
		runVaultClone(subArgs)
	case "rename":
		runVaultRename(subArgs)
	case "merge":
		runVaultMerge(subArgs)
	case "export":
		runVaultExport(subArgs)
	case "export-markdown":
		runVaultExportMarkdown(subArgs)
	case "import":
		runVaultImport(subArgs)
	case "reindex-fts":
		runVaultReindexFTS(subArgs)
	case "reembed":
		runVaultReembed(subArgs)
	case "recall-mode":
		runVaultRecallMode(subArgs)
	}
}

// ---------------------------------------------------------------------------
// vault create
// ---------------------------------------------------------------------------

func runVaultCreate(args []string) {
	var name string
	var public bool

	for _, a := range args {
		switch a {
		case "--public":
			public = true
		default:
			if !strings.HasPrefix(a, "-") && name == "" {
				name = a
			}
		}
	}

	if name == "" {
		fmt.Println("Usage: muninn vault create <vault-name> [--public]")
		fmt.Println()
		fmt.Println("  Registers a new vault in the auth store.")
		fmt.Println("  By default the vault is locked (API key required). Use --public to allow open access.")
		fmt.Println("  The vault will appear in 'muninn vault list' immediately after creation.")
		return
	}

	bodyBytes, err := json.Marshal(map[string]any{"name": name, "public": public})
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return
	}

	client := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequest("PUT",
		fmt.Sprintf("%s/api/admin/vaults/config", vaultAdminBase),
		bytes.NewReader(bodyBytes))
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	addSessionCookie(req)

	resp, err := client.Do(req)
	if err != nil {
		fmt.Printf("Error connecting to MuninnDB: %v\n", err)
		fmt.Println("Is muninn running? Try: muninn status")
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		printHTTPError(resp)
		return
	}

	fmt.Printf("  Vault %q created.\n", name)
	if public {
		fmt.Println("  Access: public (no API key required)")
	} else {
		fmt.Println("  Access: locked (API key required — use 'muninn api-key create' to generate one)")
	}
}

func runVaultDelete(args []string) {
	name, yes, force := parseVaultArgs(args, "delete")
	if name == "" {
		return
	}
	if !yes && !confirmVaultAction(name, "delete") {
		fmt.Println("Cancelled.")
		return
	}
	doVaultRequestForce("DELETE",
		fmt.Sprintf("%s/api/admin/vaults/%s", vaultAdminBase, url.PathEscape(name)),
		"Vault deleted.", force)
}

func runVaultClear(args []string) {
	name, yes, force := parseVaultArgs(args, "clear")
	if name == "" {
		return
	}
	if !yes && !confirmVaultAction(name, "clear") {
		fmt.Println("Cancelled.")
		return
	}
	doVaultRequestForce("POST",
		fmt.Sprintf("%s/api/admin/vaults/%s/clear", vaultAdminBase, url.PathEscape(name)),
		"Vault cleared.", force)
}

// ---------------------------------------------------------------------------
// vault list
// ---------------------------------------------------------------------------

func runVaultList(args []string) {
	var pattern string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--pattern":
			if i+1 < len(args) {
				i++
				pattern = args[i]
			}
		case strings.HasPrefix(a, "--pattern="):
			pattern = strings.TrimPrefix(a, "--pattern=")
		case !strings.HasPrefix(a, "-") && pattern == "":
			pattern = a
		}
	}

	resp, err := http.Get(vaultAdminBase + "/api/vaults")
	if err != nil {
		fmt.Printf("Error connecting to MuninnDB: %v\n", err)
		fmt.Println("Is muninn running? Try: muninn status")
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		fmt.Printf("Error: HTTP %d\n", resp.StatusCode)
		return
	}

	var raw json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		fmt.Printf("Error parsing response: %v\n", err)
		return
	}

	var names []string
	if err := json.Unmarshal(raw, &names); err != nil {
		var wrapped map[string][]string
		if err2 := json.Unmarshal(raw, &wrapped); err2 == nil {
			names = wrapped["vaults"]
		}
	}

	if len(names) == 0 {
		fmt.Println("No vaults found.")
		return
	}

	var matched int
	for _, name := range names {
		if pattern != "" {
			ok, _ := path.Match(pattern, name)
			if !ok {
				continue
			}
		}
		matched++
		fmt.Printf("  %s\n", name)
	}

	if pattern != "" {
		fmt.Printf("\n  %d of %d vaults matched %q\n", matched, len(names), pattern)
	} else {
		fmt.Printf("\n  %d vault(s)\n", matched)
	}
}

// parseVaultArgs parses: <name> [--yes|-y] [--force|-f]
func parseVaultArgs(args []string, cmd string) (name string, yes bool, force bool) {
	for _, a := range args {
		switch a {
		case "--yes", "-y":
			yes = true
		case "--force", "-f":
			force = true
		default:
			if !strings.HasPrefix(a, "-") {
				name = a
			}
		}
	}
	if name == "" {
		fmt.Printf("Usage: muninn vault %s <vault-name> [--yes] [--force]\n", cmd)
	}
	return
}

// confirmVaultAction prompts the user to type the vault name to confirm.
func confirmVaultAction(name, action string) bool {
	fmt.Printf("\n  WARNING: This will %s vault %q and all its memories.\n", action, name)
	fmt.Printf("  Type the vault name to confirm: ")
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Scan()
	typed := strings.TrimSpace(scanner.Text())
	if typed != name {
		fmt.Printf("  Confirmation did not match %q.\n", name)
		return false
	}
	return true
}

func doVaultRequestForce(method, reqURL, successMsg string, force bool) {
	client := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequest(method, reqURL, nil)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return
	}
	if force {
		req.Header.Set("X-Allow-Default", "true")
	}
	addSessionCookie(req)

	resp, err := client.Do(req)
	if err != nil {
		fmt.Printf("Error connecting to MuninnDB: %v\n", err)
		fmt.Println("Is muninn running? Try: muninn status")
		return
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusNoContent:
		fmt.Println(" ", successMsg)
	case http.StatusNotFound:
		fmt.Println("  Vault not found.")
	case http.StatusConflict:
		if !force {
			fmt.Println("  Protected vault. Use --force to operate on the default vault.")
		} else {
			fmt.Println("  Protected vault. Cannot override.")
		}
	case http.StatusUnauthorized:
		fmt.Println("  Not authenticated. Use -u <user> -p to authenticate.")
	default:
		fmt.Printf("  Error: HTTP %d\n", resp.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// vault clone
// ---------------------------------------------------------------------------

func runVaultClone(args []string) {
	if len(args) < 2 {
		fmt.Println("Usage: muninn vault clone <source-vault> <new-name>")
		return
	}
	source := args[0]
	newName := args[1]

	fmt.Printf("Cloning vault %q to %q...\n", source, newName)

	bodyBytes, err := json.Marshal(map[string]any{"new_name": newName})
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return
	}
	req, err := http.NewRequest("POST",
		fmt.Sprintf("%s/api/admin/vaults/%s/clone", vaultAdminBase, url.PathEscape(source)),
		bytes.NewReader(bodyBytes))
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	addSessionCookie(req)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		fmt.Printf("Error connecting to MuninnDB: %v\n", err)
		fmt.Println("Is muninn running? Try: muninn status")
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusAccepted {
		printHTTPError(resp)
		return
	}

	var result struct {
		JobID string `json:"job_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil || result.JobID == "" {
		fmt.Println("  Error: could not read job ID from response.")
		return
	}

	pollProgressBar(result.JobID, source)
}

// ---------------------------------------------------------------------------
// vault rename
// ---------------------------------------------------------------------------

func runVaultRename(args []string) {
	if len(args) < 2 {
		fmt.Println("Usage: muninn vault rename <old-name> <new-name>")
		return
	}
	oldName := args[0]
	newName := args[1]

	bodyBytes, err := json.Marshal(map[string]any{"new_name": newName})
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return
	}
	req, err := http.NewRequest("POST",
		fmt.Sprintf("%s/api/admin/vaults/%s/rename", vaultAdminBase, url.PathEscape(oldName)),
		bytes.NewReader(bodyBytes))
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	addSessionCookie(req)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		fmt.Printf("Error connecting to MuninnDB: %v\n", err)
		fmt.Println("Is muninn running? Try: muninn status")
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		printHTTPError(resp)
		return
	}

	fmt.Printf("  Vault renamed from %q to %q.\n", oldName, newName)
}

// ---------------------------------------------------------------------------
// vault merge
// ---------------------------------------------------------------------------

func runVaultMerge(args []string) {
	var source, target string
	var deleteSource, yes bool

	for i, a := range args {
		switch {
		case a == "--delete-source":
			deleteSource = true
		case a == "--yes" || a == "-y":
			yes = true
		case source == "" && !strings.HasPrefix(a, "-"):
			source = a
		case target == "" && !strings.HasPrefix(a, "-") && i > 0:
			target = a
		}
	}

	if source == "" || target == "" {
		fmt.Println("Usage: muninn vault merge <source> <target> [--delete-source] [--yes]")
		return
	}

	if source == target {
		fmt.Fprintln(os.Stderr, "Error: cannot merge a vault into itself")
		osExit(1)
		return
	}

	if !yes {
		fmt.Printf("\n  Merge Vault Wizard\n")
		fmt.Printf("  Source: %q\n", source)
		fmt.Printf("  Target: %q\n", target)
		if deleteSource {
			fmt.Printf("  Source vault will be deleted after merge.\n")
		}
		fmt.Printf("\n  Type 'merge' to confirm: ")
		scanner := bufio.NewScanner(os.Stdin)
		scanner.Scan()
		if strings.TrimSpace(scanner.Text()) != "merge" {
			fmt.Println("Cancelled.")
			return
		}
	}

	fmt.Printf("Merging %q into %q...\n", source, target)

	bodyBytes, err := json.Marshal(map[string]any{"target": target, "delete_source": deleteSource})
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return
	}
	req, err := http.NewRequest("POST",
		fmt.Sprintf("%s/api/admin/vaults/%s/merge-into", vaultAdminBase, url.PathEscape(source)),
		bytes.NewReader(bodyBytes))
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	addSessionCookie(req)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		fmt.Printf("Error connecting to MuninnDB: %v\n", err)
		fmt.Println("Is muninn running? Try: muninn status")
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusAccepted {
		printHTTPError(resp)
		return
	}

	var result struct {
		JobID string `json:"job_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil || result.JobID == "" {
		fmt.Println("  Error: could not read job ID from response.")
		return
	}

	pollProgressBar(result.JobID, source)
}

// ---------------------------------------------------------------------------
// vault reindex-fts
// ---------------------------------------------------------------------------

func runVaultReindexFTS(args []string) {
	var vaultName string
	for _, a := range args {
		if !strings.HasPrefix(a, "-") && vaultName == "" {
			vaultName = a
		}
	}
	if vaultName == "" {
		fmt.Println("Usage: muninn vault reindex-fts <vault-name>")
		fmt.Println("  Rebuilds the FTS index for the vault using the current Porter2-stemmed tokenizer.")
		fmt.Println("  Old posting lists are deleted and re-created with stemmed tokens.")
		fmt.Println("  After this completes, the vault FTS version marker is set to 1.")
		return
	}

	fmt.Printf("Re-indexing FTS for vault %q...\n", vaultName)

	reqURL := fmt.Sprintf("%s/api/admin/vaults/%s/reindex-fts", vaultAdminBase, url.PathEscape(vaultName))
	client := &http.Client{Timeout: 30 * time.Minute}
	req, err := http.NewRequest("POST", reqURL, nil)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return
	}
	addSessionCookie(req)

	resp, err := client.Do(req)
	if err != nil {
		fmt.Printf("Error connecting to MuninnDB: %v\n", err)
		fmt.Println("Is muninn running? Try: muninn status")
		return
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		var result struct {
			Vault            string `json:"vault"`
			EngramsReindexed int64  `json:"engrams_reindexed"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			fmt.Println("  Done (could not parse response).")
			return
		}
		fmt.Printf("  Re-indexed %d engrams in vault %q.\n", result.EngramsReindexed, result.Vault)
		fmt.Println("  FTS version marker set to 1 (Porter2 stemming active).")
	case http.StatusNotFound:
		fmt.Println("  Vault not found.")
	case http.StatusUnauthorized:
		fmt.Println("  Not authenticated. Use -u <user> -p to authenticate.")
	default:
		printHTTPError(resp)
	}
}

// ---------------------------------------------------------------------------
// vault reembed
// ---------------------------------------------------------------------------

func runVaultReembed(args []string) {
	var vaultName string
	for _, a := range args {
		if !strings.HasPrefix(a, "-") && vaultName == "" {
			vaultName = a
		}
	}
	if vaultName == "" {
		fmt.Println("Usage: muninn vault reembed <vault-name>")
		fmt.Println("  Clears stale embeddings and lets the RetroactiveProcessor")
		fmt.Println("  re-embed every engram with the current embedding model.")
		fmt.Println("  The vault stays queryable during migration (degraded recall).")
		return
	}

	fmt.Printf("Re-embedding vault %q...\n", vaultName)

	reqURL := fmt.Sprintf("%s/api/admin/vaults/%s/reembed", vaultAdminBase, url.PathEscape(vaultName))
	client := &http.Client{Timeout: 30 * time.Second}
	req, err := http.NewRequest("POST", reqURL, nil)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return
	}
	addSessionCookie(req)

	resp, err := client.Do(req)
	if err != nil {
		fmt.Printf("Error connecting to MuninnDB: %v\n", err)
		fmt.Println("Is muninn running? Try: muninn status")
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusAccepted {
		printHTTPError(resp)
		return
	}

	var result struct {
		JobID string `json:"job_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil || result.JobID == "" {
		fmt.Println("  Error: could not read job ID from response.")
		return
	}

	pollProgressBar(result.JobID, vaultName)
	fmt.Println("  Embedding flags cleared. RetroactiveProcessor will re-embed in the background.")
	fmt.Println("  Monitor progress: GET /api/admin/embed/status")
}

// ---------------------------------------------------------------------------
// progress bar
// ---------------------------------------------------------------------------

type statusSnap struct {
	Status       string  `json:"status"`
	Phase        string  `json:"phase"`
	CopyTotal    int64   `json:"copy_total"`
	CopyCurrent  int64   `json:"copy_current"`
	IndexTotal   int64   `json:"index_total"`
	IndexCurrent int64   `json:"index_current"`
	Pct          float64 `json:"pct"`
	Error        string  `json:"error,omitempty"`
}

// printHTTPError decodes and prints the JSON error body from a non-success response.
func printHTTPError(resp *http.Response) {
	var errResp struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&errResp); err == nil && errResp.Error.Message != "" {
		fmt.Printf("  Error: %s\n", errResp.Error.Message)
		return
	}
	fmt.Printf("  Error: HTTP %d\n", resp.StatusCode)
}

const pollTimeout = 30 * time.Minute

func pollProgressBar(jobID, vaultName string) {
	isTTY := isTerminal()
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.After(pollTimeout)

	for {
		select {
		case <-deadline:
			if isTTY {
				fmt.Println()
			}
			fmt.Printf("Timed out after %s waiting for job to complete.\n", pollTimeout)
			fmt.Printf("The job may still be running on the server.\n")
			fmt.Printf("Check status: muninn vault job-status %s\n", jobID)
			os.Exit(1)
		case <-ticker.C:
			snap := fetchJobStatus(jobID, vaultName)
			if snap == nil {
				fmt.Println("Job not found.")
				return
			}

			bar := renderBar(*snap)
			if isTTY {
				fmt.Printf("\r%s", bar)
			} else {
				fmt.Printf("%s\n", bar)
			}

			if snap.Status == "done" {
				if isTTY {
					fmt.Println()
				}
				fmt.Println("Done!")
				return
			}
			if snap.Status == "error" {
				if isTTY {
					fmt.Println()
				}
				fmt.Printf("Error: %s\n", snap.Error)
				return
			}
		}
	}
}

func fetchJobStatus(jobID, vaultName string) *statusSnap {
	u := fmt.Sprintf("%s/api/admin/vaults/%s/job-status?job_id=%s",
		vaultAdminBase, url.PathEscape(vaultName), url.QueryEscape(jobID))
	client := &http.Client{Timeout: 5 * time.Second}
	req, _ := http.NewRequest("GET", u, nil)
	addSessionCookie(req)
	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	var snap statusSnap
	if err := json.NewDecoder(resp.Body).Decode(&snap); err != nil {
		return nil
	}
	return &snap
}

func renderBar(snap statusSnap) string {
	pct := snap.Pct
	filled := int(pct / 5) // 20-char bar
	if filled > 20 {
		filled = 20
	}
	bar := strings.Repeat("█", filled) + strings.Repeat("░", 20-filled)
	phase := "Copying"
	current, total := snap.CopyCurrent, snap.CopyTotal
	if snap.Phase == "indexing" {
		phase = "Re-indexing"
		current, total = snap.IndexCurrent, snap.IndexTotal
	}
	return fmt.Sprintf("[%s] %5.1f%%  %s  (%d/%d)",
		bar, pct, phase, current, total)
}

func isTerminal() bool {
	fi, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}

// ---------------------------------------------------------------------------
// vault export
// ---------------------------------------------------------------------------

func runVaultExport(args []string) {
	var vaultName, outputFile string
	var resetMetadata bool

	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--vault" || a == "-v":
			if i+1 < len(args) {
				i++
				vaultName = args[i]
			}
		case strings.HasPrefix(a, "--vault="):
			vaultName = strings.TrimPrefix(a, "--vault=")
		case a == "--output" || a == "-o":
			if i+1 < len(args) {
				i++
				outputFile = args[i]
			}
		case strings.HasPrefix(a, "--output="):
			outputFile = strings.TrimPrefix(a, "--output=")
		case a == "--reset-metadata":
			resetMetadata = true
		case !strings.HasPrefix(a, "-") && vaultName == "":
			vaultName = a
		}
	}

	if vaultName == "" {
		fmt.Println("Usage: muninn vault export --vault <name> [--output <file>] [--reset-metadata]")
		return
	}

	if outputFile == "" {
		outputFile = vaultName + ".muninn"
	}

	exportURL := fmt.Sprintf("%s/api/admin/vaults/%s/export", vaultAdminBase, url.PathEscape(vaultName))
	if resetMetadata {
		exportURL += "?reset_metadata=true"
	}

	fmt.Printf("Exporting vault %q to %q...\n", vaultName, outputFile)

	client := &http.Client{Timeout: 30 * time.Minute}
	req, err := http.NewRequest("GET", exportURL, nil)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return
	}
	addSessionCookie(req)
	resp, err := client.Do(req)
	if err != nil {
		fmt.Printf("Error connecting to MuninnDB: %v\n", err)
		fmt.Println("Is muninn running? Try: muninn status")
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		printHTTPError(resp)
		return
	}

	f, err := os.Create(outputFile)
	if err != nil {
		fmt.Printf("Error creating output file: %v\n", err)
		return
	}
	defer f.Close()

	n, err := io.Copy(f, resp.Body)
	if err != nil {
		fmt.Printf("Error writing archive: %v\n", err)
		return
	}
	fmt.Printf("  Exported %d bytes to %q\n", n, outputFile)
}

// ---------------------------------------------------------------------------
// vault export-markdown
// ---------------------------------------------------------------------------

func runVaultExportMarkdown(args []string) {
	var vaultName, outputFile string
	var allVaults bool

	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--vault" || a == "-v":
			if i+1 < len(args) {
				i++
				vaultName = args[i]
			}
		case strings.HasPrefix(a, "--vault="):
			vaultName = strings.TrimPrefix(a, "--vault=")
		case a == "--output" || a == "-o":
			if i+1 < len(args) {
				i++
				outputFile = args[i]
			}
		case strings.HasPrefix(a, "--output="):
			outputFile = strings.TrimPrefix(a, "--output=")
		case a == "--all-vaults":
			allVaults = true
		case !strings.HasPrefix(a, "-") && vaultName == "":
			vaultName = a
		}
	}

	if allVaults {
		names, err := listVaultNamesAdmin()
		if err != nil {
			fmt.Printf("Error listing vaults: %v\n", err)
			return
		}
		outDir := outputFile
		if outDir == "" {
			outDir = "."
		}
		for _, name := range names {
			fmt.Printf("Exporting vault %q...\n", name)
			dest := fmt.Sprintf("%s/%s.markdown.tgz", outDir, name)
			if err := exportVaultMarkdownOne(name, dest); err != nil {
				fmt.Printf("  Error exporting %q: %v\n", name, err)
			}
		}
		return
	}

	if vaultName == "" {
		fmt.Println("Usage: muninn vault export-markdown --vault <name> [--output <file>] [--all-vaults]")
		return
	}

	if outputFile == "" {
		outputFile = vaultName + ".markdown.tgz"
	}

	if err := exportVaultMarkdownOne(vaultName, outputFile); err != nil {
		fmt.Printf("Error: %v\n", err)
	}
}

func exportVaultMarkdownOne(vaultName, outputFile string) error {
	exportURL := fmt.Sprintf("%s/api/admin/vaults/%s/export-markdown", vaultAdminBase, url.PathEscape(vaultName))

	client := &http.Client{Timeout: 30 * time.Minute}
	req, err := http.NewRequest("GET", exportURL, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	addSessionCookie(req)
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("connect to MuninnDB: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	f, err := os.Create(outputFile)
	if err != nil {
		return fmt.Errorf("create output file: %w", err)
	}
	defer f.Close()

	n, err := io.Copy(f, resp.Body)
	if err != nil {
		return fmt.Errorf("write archive: %w", err)
	}
	fmt.Printf("  Exported %d bytes to %q\n", n, outputFile)
	return nil
}

func listVaultNamesAdmin() ([]string, error) {
	resp, err := http.Get(vaultAdminBase + "/api/vaults")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	var names []string
	if err := json.NewDecoder(resp.Body).Decode(&names); err != nil {
		return nil, err
	}
	sort.Strings(names)
	return names, nil
}

// ---------------------------------------------------------------------------
// vault import
// ---------------------------------------------------------------------------

func runVaultImport(args []string) {
	var filePath, vaultName string
	var resetMetadata bool

	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--vault" || a == "-v":
			if i+1 < len(args) {
				i++
				vaultName = args[i]
			}
		case strings.HasPrefix(a, "--vault="):
			vaultName = strings.TrimPrefix(a, "--vault=")
		case a == "--reset-metadata":
			resetMetadata = true
		case !strings.HasPrefix(a, "-") && filePath == "":
			filePath = a
		}
	}

	if filePath == "" || vaultName == "" {
		fmt.Println("Usage: muninn vault import <file> --vault <name> [--reset-metadata]")
		return
	}

	f, err := os.Open(filePath)
	if err != nil {
		fmt.Printf("Error opening archive: %v\n", err)
		return
	}
	defer f.Close()

	stat, err := f.Stat()
	if err != nil {
		fmt.Printf("Error stat-ing archive: %v\n", err)
		return
	}

	importURL := fmt.Sprintf("%s/api/admin/vaults/import?vault=%s", vaultAdminBase, url.QueryEscape(vaultName))
	if resetMetadata {
		importURL += "&reset_metadata=true"
	}

	fmt.Printf("Importing %q into vault %q...\n", filePath, vaultName)

	client := &http.Client{Timeout: 30 * time.Minute}
	req, err := http.NewRequest("POST", importURL, f)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return
	}
	req.ContentLength = stat.Size()
	req.Header.Set("Content-Type", "application/octet-stream")
	addSessionCookie(req)

	resp, err := client.Do(req)
	if err != nil {
		fmt.Printf("Error connecting to MuninnDB: %v\n", err)
		fmt.Println("Is muninn running? Try: muninn status")
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusAccepted {
		printHTTPError(resp)
		return
	}

	var result struct {
		JobID string `json:"job_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil || result.JobID == "" {
		fmt.Println("  Error: could not read job ID from response.")
		return
	}

	pollProgressBar(result.JobID, vaultName)
}

// ---------------------------------------------------------------------------
// vault recall-mode
// ---------------------------------------------------------------------------

func runVaultRecallMode(args []string) {
	if len(args) == 0 {
		fmt.Println("Usage: muninn vault recall-mode <vault> [mode]")
		fmt.Println()
		fmt.Println("  With one argument, prints the vault's default recall mode.")
		fmt.Println("  With two arguments, sets the vault's default recall mode.")
		fmt.Println()
		fmt.Println("  Valid modes: semantic, recent, balanced, deep")
		return
	}

	vaultName := args[0]
	plasticityURL := fmt.Sprintf("%s/api/admin/vault/%s/plasticity", vaultAdminBase, url.PathEscape(vaultName))

	if len(args) == 1 {
		// GET current recall mode
		client := &http.Client{Timeout: 5 * time.Second}
		req, err := http.NewRequest("GET", plasticityURL, nil)
		if err != nil {
			fmt.Printf("Error: %v\n", err)
			return
		}
		addSessionCookie(req)
		resp, err := client.Do(req)
		if err != nil {
			fmt.Printf("Error connecting to MuninnDB: %v\n", err)
			fmt.Println("Is muninn running? Try: muninn status")
			return
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			printHTTPError(resp)
			return
		}

		var data struct {
			Resolved struct {
				RecallMode string `json:"recall_mode"`
			} `json:"resolved"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
			fmt.Printf("Error parsing response: %v\n", err)
			return
		}
		mode := data.Resolved.RecallMode
		if mode == "" {
			mode = "balanced"
		}
		fmt.Printf("  Vault %q recall mode: %s\n", vaultName, mode)
		return
	}

	// SET recall mode
	newMode := args[1]
	validModes := map[string]bool{"semantic": true, "recent": true, "balanced": true, "deep": true}
	if !validModes[newMode] {
		fmt.Printf("Error: invalid recall mode %q (valid: semantic, recent, balanced, deep)\n", newMode)
		return
	}

	// GET current plasticity config
	client := &http.Client{Timeout: 5 * time.Second}
	getReq, err := http.NewRequest("GET", plasticityURL, nil)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return
	}
	addSessionCookie(getReq)
	getResp, err := client.Do(getReq)
	if err != nil {
		fmt.Printf("Error connecting to MuninnDB: %v\n", err)
		return
	}
	defer getResp.Body.Close()

	if getResp.StatusCode != http.StatusOK {
		printHTTPError(getResp)
		return
	}

	var data struct {
		Config json.RawMessage `json:"config"`
	}
	if err := json.NewDecoder(getResp.Body).Decode(&data); err != nil {
		fmt.Printf("Error parsing response: %v\n", err)
		return
	}

	// Merge recall_mode into existing config
	var cfgMap map[string]any
	if data.Config != nil && string(data.Config) != "null" {
		if err := json.Unmarshal(data.Config, &cfgMap); err != nil {
			cfgMap = map[string]any{}
		}
	} else {
		cfgMap = map[string]any{}
	}
	cfgMap["recall_mode"] = newMode

	bodyBytes, err := json.Marshal(cfgMap)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return
	}

	putReq, err := http.NewRequest("PUT", plasticityURL, bytes.NewReader(bodyBytes))
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return
	}
	putReq.Header.Set("Content-Type", "application/json")
	addSessionCookie(putReq)

	putResp, err := client.Do(putReq)
	if err != nil {
		fmt.Printf("Error connecting to MuninnDB: %v\n", err)
		return
	}
	defer putResp.Body.Close()

	if putResp.StatusCode != http.StatusOK {
		printHTTPError(putResp)
		return
	}

	fmt.Printf("  Vault %q recall mode set to: %s\n", vaultName, newMode)
}
