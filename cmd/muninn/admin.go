package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"golang.org/x/term"
)

func printAdminUsage() {
	fmt.Println("Usage: muninn admin <command> [flags]")
	fmt.Println()
	fmt.Println("Commands:")
	fmt.Println("  change-password      Interactively change the admin password")
	fmt.Println()
	fmt.Println("Auth flags (MySQL-style, optional):")
	fmt.Println("  -u <user>         Admin username (default: root)")
	fmt.Println("  -p                Prompt for current password")
	fmt.Println("  -p<password>      Inline current password (no space)")
	fmt.Println("  -h <host:port>    Server host:port (default: 127.0.0.1:8475)")
}

func runAdmin(args []string) {
	if len(args) == 0 {
		printAdminUsage()
		return
	}

	// Parse auth flags (-u, -p, -h), leaving the subcommand and its args.
	remaining, username, password, prompted := parseAdminFlags(args)
	if len(remaining) == 0 {
		printAdminUsage()
		return
	}

	sub := remaining[0]
	subArgs := remaining[1:]

	// Validate the subcommand before authenticating.
	switch sub {
	case "change-password":
	default:
		fmt.Printf("Unknown admin command: %q\n", sub)
		printAdminUsage()
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
	case "change-password":
		runAdminChangePassword(username, subArgs)
	}
}

// ---------------------------------------------------------------------------
// admin change-password
// ---------------------------------------------------------------------------

func runAdminChangePassword(username string, args []string) {
	// Allow --user / -u override inside the subcommand args too.
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-u" || a == "--user":
			if i+1 < len(args) {
				i++
				username = args[i]
			}
		case strings.HasPrefix(a, "--user="):
			username = strings.TrimPrefix(a, "--user=")
		}
	}

	if username == "" {
		username = "root"
	}

	fmt.Printf("Changing password for admin user %q.\n", username)
	fmt.Print("New password: ")
	newPassBytes, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Println()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error reading password: %v\n", err)
		osExit(1)
		return
	}

	newPass := strings.TrimSpace(string(newPassBytes))
	if newPass == "" {
		fmt.Fprintln(os.Stderr, "Error: new password cannot be empty.")
		osExit(1)
		return
	}

	fmt.Print("Confirm new password: ")
	confirmBytes, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Println()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error reading password: %v\n", err)
		osExit(1)
		return
	}

	if string(confirmBytes) != newPass {
		fmt.Fprintln(os.Stderr, "Error: passwords do not match.")
		osExit(1)
		return
	}

	bodyBytes, err := json.Marshal(map[string]string{
		"username":     username,
		"new_password": newPass,
	})
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return
	}

	client := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequest("PUT",
		fmt.Sprintf("%s/api/admin/password", vaultAdminBase),
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

	switch resp.StatusCode {
	case http.StatusOK:
		fmt.Println("  Password updated.")
	case http.StatusBadRequest:
		printHTTPError(resp)
	case http.StatusNotFound:
		fmt.Printf("  Admin user %q not found.\n", username)
	case http.StatusUnauthorized:
		fmt.Println("  Not authenticated. Use -u <user> -p to authenticate.")
	default:
		printHTTPError(resp)
	}
}
