package auth

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
)

// Bootstrap ensures an admin user, session secret, and default vault config exist.
// On first run, creates "root" with the default password "password" (or the value
// of MUNINN_ADMIN_PASSWORD if set), sets the "default" vault to public (no API key
// required), and prints a reminder to change the password.
// On subsequent runs, if MUNINN_ADMIN_PASSWORD is set it is applied to the existing
// "root" account so that containerised/automated deployments can rotate the password
// via environment variable without a first-run wipe.
// secretPath is where the session signing secret is persisted (e.g. dataDir/auth_secret).
func Bootstrap(store *Store, secretPath string) (secret []byte, err error) {
	secret, err = loadOrCreateSessionSecret(secretPath)
	if err != nil {
		return nil, err
	}

	envPassword := os.Getenv("MUNINN_ADMIN_PASSWORD")

	// Create root admin if none exists
	if !store.AdminExists() {
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
	} else if envPassword != "" {
		// Existing instance: MUNINN_ADMIN_PASSWORD is set — update the root password
		// so operators can rotate credentials via environment variable.
		if changeErr := store.ChangeAdminPassword("root", envPassword); changeErr != nil {
			slog.Warn("failed to apply MUNINN_ADMIN_PASSWORD to existing root account",
				"err", changeErr)
		} else {
			slog.Info("root admin password updated from MUNINN_ADMIN_PASSWORD")
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

// loadOrCreateSessionSecret returns the persisted session signing secret,
// creating it on first run. It never returns an empty secret with a nil error:
// the REST and UI servers treat an empty secret as "admin auth not configured"
// and serve admin routes without it, so an empty secret must not leave here.
//
//   - missing file: first run, generate and persist.
//   - blank file (see isBlankSecret): there is no secret to preserve — a
//     first-run write that failed part-way, a truncated restore, a placeholder,
//     a zero-filled file left by a crash. Generate and persist, with a WARN.
//   - any other read failure: an error. A secret that exists but cannot be read
//     is not replaced; replacing it would silently invalidate every session and
//     every id derived from it.
//
// A non-blank file is returned byte for byte, whatever its length: an operator
// may have provisioned it, and existing sessions are signed with exactly those
// bytes.
func loadOrCreateSessionSecret(path string) ([]byte, error) {
	secret, err := os.ReadFile(path)
	switch {
	case err == nil && !isBlankSecret(secret):
		return secret, nil
	case err == nil:
		slog.Warn("session secret file is blank; generating a new secret (any existing admin session is invalidated)",
			"path", path)
	case errors.Is(err, fs.ErrNotExist):
		// First run.
	default:
		return nil, fmt.Errorf("read session secret %s: %w", path, err)
	}

	secret, err = GenerateSecret()
	if err != nil {
		return nil, fmt.Errorf("generate session secret: %w", err)
	}
	if isBlankSecret(secret) {
		return nil, errors.New("generate session secret: generator returned a blank secret")
	}
	if err := writeSecretFile(path, secret); err != nil {
		return nil, fmt.Errorf("write session secret: %w", err)
	}
	slog.Info("generated new session secret", "path", path)
	return secret, nil
}

// isBlankSecret reports whether b holds no key material: it is empty, or every
// byte is NUL or ASCII whitespace. An empty secret switches admin auth off; a
// file of only newlines or only zero bytes is a key anyone can guess, so a
// session cookie can be forged under it. None of them is a secret.
func isBlankSecret(b []byte) bool {
	return len(bytes.Trim(b, "\x00 \t\n\v\f\r")) == 0
}

// writeSecretFile persists secret at path. See writeFileAtomic.
func writeSecretFile(path string, secret []byte) error {
	return writeFileAtomic(path, secret, func(f *os.File, b []byte) error {
		_, err := f.Write(b)
		return err
	})
}

// writeFileAtomic writes data to a temporary file in path's directory and
// renames it into place, so a write that fails part-way (disk full, crash)
// leaves either the previous file or no file — never a truncated one.
// os.CreateTemp creates the file owner-only (0600). fill is the step that
// writes the bytes; it is a parameter so a test can make it fail part-way.
func writeFileAtomic(path string, data []byte, fill func(f *os.File, b []byte) error) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".auth_secret-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if err := fill(tmp, data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}
