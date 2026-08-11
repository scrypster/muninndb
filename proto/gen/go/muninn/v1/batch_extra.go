// Code generated manually for MuninnDB. DO NOT EDIT.
//
// Convenience methods on protoc-gen-go message types that protoc-gen-go itself
// does not produce. Kept in a separate file so `make proto` (which regenerates
// service.pb.go from service.proto) does not delete them.

package muninn_v1

// GetVault returns the vault of the first WriteRequest in the batch. The gRPC
// auth interceptor asserts an anonymous `interface{ GetVault() string }` against
// inbound requests to resolve the vault for the no-token / public-vault auth
// path. BatchWriteRequest has no `vault` field of its own (only repeated
// WriteRequest), so protoc-gen-go does not generate this getter; without it,
// an unkeyed BatchWrite would resolve against "default" instead of the first
// item's vault.
func (m *BatchWriteRequest) GetVault() string {
	if len(m.Requests) > 0 {
		return m.Requests[0].Vault
	}
	return ""
}
