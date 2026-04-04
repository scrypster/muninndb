package auth

import (
	"fmt"
	"log/slog"
	"os"
)

// Bootstrap ensures an admin user, session secret, and default vault config exist.
// On first run, creates "root" with the default password "password", sets the
// "default" vault to public (no API key required), and prints a reminder to
// change the password. Subsequent runs are no-ops.
// secretPath is where the session signing secret is persisted (e.g. dataDir/auth_secret).
func Bootstrap(store *Store, secretPath string) (secret []byte, err error) {
	// Load or generate session secret
	secret, err = os.ReadFile(secretPath)
	if err != nil {
		secret, err = GenerateSecret()
		if err != nil {
			return nil, fmt.Errorf("generate session secret: %w", err)
		}
		if writeErr := os.WriteFile(secretPath, secret, 0600); writeErr != nil {
			return nil, fmt.Errorf("write session secret: %w", writeErr)
		}
		slog.Info("generated new session secret", "path", secretPath)
	}

	// Create root admin if none exists
	if !store.AdminExists() {
		envPassword := os.Getenv("MUNINN_ADMIN_PASSWORD")
		adminPassword := envPassword
		if adminPassword == "" {
			adminPassword = "password"
		}
		if err = store.CreateAdmin("root", adminPassword); err != nil {
			return nil, fmt.Errorf("create root admin: %w", err)
		}

		if envPassword != "" {
			fmt.Println("┌──────────────────────────────────────────────────┐")
			fmt.Println("│            MuninnDB — First Run Setup             │")
			fmt.Println("│                                                    │")
			fmt.Println("│  Admin username : root                             │")
			fmt.Println("│  Admin password : (set via MUNINN_ADMIN_PASSWORD)  │")
			fmt.Println("│                                                    │")
			fmt.Println("│  Default vault  : public (no API key required)     │")
			fmt.Println("└──────────────────────────────────────────────────┘")
		} else {
			fmt.Println("┌──────────────────────────────────────────────────┐")
			fmt.Println("│            MuninnDB — First Run Setup             │")
			fmt.Println("│                                                    │")
			fmt.Println("│  Admin username : root                             │")
			fmt.Println("│  Admin password : password                         │")
			fmt.Println("│                                                    │")
			fmt.Println("│  Default vault  : public (no API key required)     │")
			fmt.Println("│                                                    │")
			fmt.Println("│  Change your password and review vault settings    │")
			fmt.Println("│  in the admin UI before exposing to a network.     │")
			fmt.Println("└──────────────────────────────────────────────────┘")
		}
	}

	// Ensure at least one vault config exists. Covers both fresh installs
	// and upgrades from versions that didn't create vault configs during
	// bootstrap. Without this, fail-closed mode locks out all MCP clients.
	cfgs, cfgErr := store.ListVaultConfigs()
	if cfgErr == nil && len(cfgs) == 0 {
		if err = store.SetVaultConfig(VaultConfig{Name: "default", Public: true}); err != nil {
			return nil, fmt.Errorf("configure default vault: %w", err)
		}
		slog.Info("created default vault config (public)")
	}

	return secret, nil
}
