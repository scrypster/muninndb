package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

func printAPIKeyUsage() {
	fmt.Println("Usage: muninn api-key <command> [flags]")
	fmt.Println()
	fmt.Println("Commands:")
	fmt.Println("  create  --vault <vault> [--label <label>] [--mode full|observe] [--expires 90d]")
	fmt.Println("                                           Create a new API key (token shown once)")
	fmt.Println("  list    [--vault <vault>]                List API keys (no token values)")
	fmt.Println("  revoke  <key-id> [--vault <vault>]      Revoke an API key immediately")
	fmt.Println()
	fmt.Println("Auth flags (MySQL-style, optional):")
	fmt.Println("  -u <user>         Admin username (default: root)")
	fmt.Println("  -p                Prompt for password")
	fmt.Println("  -p<password>      Inline password (no space)")
	fmt.Println("  -h <host:port>    Server host:port (default: 127.0.0.1:8475)")
}

func runAPIKey(args []string) {
	if len(args) == 0 {
		printAPIKeyUsage()
		return
	}

	// Parse auth flags (-u, -p, -h), leaving the subcommand and its args.
	remaining, username, password, prompted := parseAdminFlags(args)
	if len(remaining) == 0 {
		printAPIKeyUsage()
		return
	}

	sub := remaining[0]
	subArgs := remaining[1:]

	// Validate the subcommand before authenticating.
	switch sub {
	case "create", "list", "revoke":
	default:
		fmt.Printf("Unknown api-key command: %q\n", sub)
		printAPIKeyUsage()
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
		runAPIKeyCreate(subArgs)
	case "list":
		runAPIKeyList(subArgs)
	case "revoke":
		runAPIKeyRevoke(subArgs)
	}
}

// ---------------------------------------------------------------------------
// api-key create
// ---------------------------------------------------------------------------

func runAPIKeyCreate(args []string) {
	var vault, label, mode, expires string

	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--vault" || a == "-v":
			if i+1 < len(args) {
				i++
				vault = args[i]
			}
		case strings.HasPrefix(a, "--vault="):
			vault = strings.TrimPrefix(a, "--vault=")
		case a == "--label" || a == "-l":
			if i+1 < len(args) {
				i++
				label = args[i]
			}
		case strings.HasPrefix(a, "--label="):
			label = strings.TrimPrefix(a, "--label=")
		case a == "--mode" || a == "-m":
			if i+1 < len(args) {
				i++
				mode = args[i]
			}
		case strings.HasPrefix(a, "--mode="):
			mode = strings.TrimPrefix(a, "--mode=")
		case a == "--expires" || a == "-e":
			if i+1 < len(args) {
				i++
				expires = args[i]
			}
		case strings.HasPrefix(a, "--expires="):
			expires = strings.TrimPrefix(a, "--expires=")
		}
	}

	if vault == "" {
		fmt.Println("Usage: muninn api-key create --vault <vault> [--label <label>] [--mode full|observe] [--expires 90d]")
		return
	}

	if mode == "" {
		mode = "full"
	}
	if mode != "full" && mode != "observe" {
		fmt.Printf("Error: invalid mode %q — must be 'full' or 'observe'\n", mode)
		return
	}

	body := map[string]any{
		"vault": vault,
		"label": label,
		"mode":  mode,
	}
	if expires != "" {
		body["expires"] = expires
	}

	bodyBytes, err := json.Marshal(body)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return
	}

	client := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequest("POST",
		fmt.Sprintf("%s/api/admin/keys", vaultAdminBase),
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

	if resp.StatusCode != http.StatusCreated {
		printHTTPError(resp)
		return
	}

	var result struct {
		Token string `json:"token"`
		Key   struct {
			ID        string     `json:"id"`
			Vault     string     `json:"vault"`
			Label     string     `json:"label"`
			Mode      string     `json:"mode"`
			CreatedAt time.Time  `json:"created_at"`
			ExpiresAt *time.Time `json:"expires_at,omitempty"`
		} `json:"key"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		fmt.Printf("Error parsing response: %v\n", err)
		return
	}

	fmt.Println()
	fmt.Println("  API key created.")
	fmt.Println()
	fmt.Printf("  Token  : %s\n", result.Token)
	fmt.Println()
	fmt.Println("  IMPORTANT: This token will NOT be shown again. Copy it now.")
	fmt.Println()
	fmt.Printf("  ID     : %s\n", result.Key.ID)
	fmt.Printf("  Vault  : %s\n", result.Key.Vault)
	fmt.Printf("  Label  : %s\n", result.Key.Label)
	fmt.Printf("  Mode   : %s\n", result.Key.Mode)
	fmt.Printf("  Created: %s\n", result.Key.CreatedAt.Format(time.RFC3339))
	if result.Key.ExpiresAt != nil {
		fmt.Printf("  Expires: %s\n", result.Key.ExpiresAt.Format(time.RFC3339))
	} else {
		fmt.Println("  Expires: never")
	}
	fmt.Println()
}

// ---------------------------------------------------------------------------
// api-key list
// ---------------------------------------------------------------------------

func runAPIKeyList(args []string) {
	var vault string

	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--vault" || a == "-v":
			if i+1 < len(args) {
				i++
				vault = args[i]
			}
		case strings.HasPrefix(a, "--vault="):
			vault = strings.TrimPrefix(a, "--vault=")
		case !strings.HasPrefix(a, "-") && vault == "":
			vault = a
		}
	}

	reqURL := fmt.Sprintf("%s/api/admin/keys", vaultAdminBase)
	if vault != "" {
		reqURL += "?vault=" + url.QueryEscape(vault)
	}

	client := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequest("GET", reqURL, nil)
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

	var result struct {
		Keys []struct {
			ID        string     `json:"id"`
			Vault     string     `json:"vault"`
			Label     string     `json:"label"`
			Mode      string     `json:"mode"`
			CreatedAt time.Time  `json:"created_at"`
			ExpiresAt *time.Time `json:"expires_at,omitempty"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		fmt.Printf("Error parsing response: %v\n", err)
		return
	}

	if len(result.Keys) == 0 {
		if vault != "" {
			fmt.Printf("  No API keys found for vault %q.\n", vault)
		} else {
			fmt.Println("  No API keys found.")
		}
		return
	}

	fmt.Println()
	fmt.Printf("  %-12s  %-16s  %-20s  %-8s  %-20s  %s\n",
		"ID", "Vault", "Label", "Mode", "Created", "Expires")
	fmt.Printf("  %s\n", strings.Repeat("-", 92))
	for _, k := range result.Keys {
		expires := "never"
		if k.ExpiresAt != nil {
			expires = k.ExpiresAt.Format("2006-01-02")
		}
		label := k.Label
		if label == "" {
			label = "(none)"
		}
		fmt.Printf("  %-12s  %-16s  %-20s  %-8s  %-20s  %s\n",
			k.ID,
			k.Vault,
			label,
			k.Mode,
			k.CreatedAt.Format("2006-01-02 15:04:05"),
			expires,
		)
	}
	fmt.Printf("\n  %d key(s)\n", len(result.Keys))
}

// ---------------------------------------------------------------------------
// api-key revoke
// ---------------------------------------------------------------------------

func runAPIKeyRevoke(args []string) {
	var keyID, vault string

	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--vault" || a == "-v":
			if i+1 < len(args) {
				i++
				vault = args[i]
			}
		case strings.HasPrefix(a, "--vault="):
			vault = strings.TrimPrefix(a, "--vault=")
		case !strings.HasPrefix(a, "-") && keyID == "":
			keyID = a
		}
	}

	if keyID == "" {
		fmt.Println("Usage: muninn api-key revoke <key-id> [--vault <vault>]")
		return
	}

	reqURL := fmt.Sprintf("%s/api/admin/keys/%s", vaultAdminBase, url.PathEscape(keyID))
	if vault != "" {
		reqURL += "?vault=" + url.QueryEscape(vault)
	}

	client := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequest("DELETE", reqURL, nil)
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
		fmt.Println("  API key revoked.")
	case http.StatusNotFound:
		fmt.Printf("  API key %q not found.\n", keyID)
	case http.StatusUnauthorized:
		fmt.Println("  Not authenticated. Use -u <user> -p to authenticate.")
	default:
		printHTTPError(resp)
	}
}
