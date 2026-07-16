package auth

const (
	prefixAdminUser      byte = 0x11
	prefixAPIKey         byte = 0x12
	prefixAPIKeyVIdx     byte = 0x13
	prefixVaultCfg       byte = 0x14
	prefixCapability     byte = 0x40
	prefixCapabilityVIdx byte = 0x41
)

func adminUserKey(username string) []byte {
	key := make([]byte, 1+len(username))
	key[0] = prefixAdminUser
	copy(key[1:], username)
	return key
}

func apiKeyStorageKey(hash16 []byte) []byte {
	key := make([]byte, 1+16)
	key[0] = prefixAPIKey
	copy(key[1:], hash16)
	return key
}

// apiKeyVaultIdxKey indexes keys by vault for listing/revocation.
func apiKeyVaultIdxKey(vault string, keyID []byte) []byte {
	key := make([]byte, 1+len(vault)+1+8)
	key[0] = prefixAPIKeyVIdx
	copy(key[1:], vault)
	key[1+len(vault)] = 0x00
	copy(key[1+len(vault)+1:], keyID[:8])
	return key
}

// apiKeyVaultIdxPrefix returns the scan prefix for all keys in a vault.
func apiKeyVaultIdxPrefix(vault string) []byte {
	key := make([]byte, 1+len(vault)+1)
	key[0] = prefixAPIKeyVIdx
	copy(key[1:], vault)
	key[1+len(vault)] = 0x00
	return key
}

func capabilityStorageKey(hash16 []byte) []byte {
	key := make([]byte, 1+16)
	key[0] = prefixCapability
	copy(key[1:], hash16)
	return key
}

// capabilityVaultIdxKey indexes capabilities by vault for listing/revocation.
func capabilityVaultIdxKey(vault string, capID []byte) []byte {
	key := make([]byte, 1+len(vault)+1+8)
	key[0] = prefixCapabilityVIdx
	copy(key[1:], vault)
	key[1+len(vault)] = 0x00
	copy(key[1+len(vault)+1:], capID[:8])
	return key
}

// capabilityVaultIdxPrefix returns the scan prefix for all capabilities in a vault.
func capabilityVaultIdxPrefix(vault string) []byte {
	key := make([]byte, 1+len(vault)+1)
	key[0] = prefixCapabilityVIdx
	copy(key[1:], vault)
	key[1+len(vault)] = 0x00
	return key
}

func vaultConfigKey(vault string) []byte {
	key := make([]byte, 1+len(vault))
	key[0] = prefixVaultCfg
	copy(key[1:], vault)
	return key
}

func vaultConfigUpperBound() []byte {
	return []byte{prefixVaultCfg + 1}
}
