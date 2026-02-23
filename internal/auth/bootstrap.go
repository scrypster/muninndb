package auth

import (
	"fmt"
	"log/slog"
	"os"
)

// Bootstrap ensures an admin user and session secret exist.
// On first run, creates "root" with the default password "password" and prints a
// reminder to change it. Subsequent runs are no-ops.
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
		if err = store.CreateAdmin("root", "password"); err != nil {
			return nil, fmt.Errorf("create root admin: %w", err)
		}
		fmt.Println("┌─────────────────────────────────────────┐")
		fmt.Println("│         MuninnDB — First Run Auth        │")
		fmt.Println("│                                          │")
		fmt.Println("│  Admin username: root                    │")
		fmt.Println("│  Admin password: password                │")
		fmt.Println("│                                          │")
		fmt.Println("│  Change this password after first login. │")
		fmt.Println("└─────────────────────────────────────────┘")
	}

	return secret, nil
}
