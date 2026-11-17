package auth

import (
	"context"
	"reflect"
	"testing"
)

// TestVaultsVisibleTo pins which vault names a caller may be told: every one
// for a verified admin session, and otherwise the single vault the request was
// authorised for.
func TestVaultsVisibleTo(t *testing.T) {
	all := []string{"customs-ledger", "harbour-notices", "pilot-station"}
	keyed := context.WithValue(context.Background(), ContextVault, "pilot-station")

	cases := []struct {
		name string
		ctx  context.Context
		want []string
	}{
		{"a key's vault, or the public vault a keyless caller named", keyed, []string{"pilot-station"}},
		{"admin session", context.WithValue(keyed, ContextAdminSession, true), all},
		{"a vault that holds nothing yet is still the caller's own", context.WithValue(context.Background(), ContextVault, "new-vault"), []string{"new-vault"}},
		{"no vault in the context at all", context.Background(), []string{}},
		{"an admin marker that is not true", context.WithValue(keyed, ContextAdminSession, false), []string{"pilot-station"}},
		{"an admin marker of the wrong type", context.WithValue(keyed, ContextAdminSession, "true"), []string{"pilot-station"}},
	}
	for _, tc := range cases {
		if got := VaultsVisibleTo(tc.ctx, all); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}
