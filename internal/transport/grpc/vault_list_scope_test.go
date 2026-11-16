package grpc_test

import (
	"context"
	"testing"

	"github.com/scrypster/muninndb/internal/auth"
	pb "github.com/scrypster/muninndb/proto/gen/go/muninn/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestListVaultsScope pins what the ListVaults RPC tells a key pinned to one
// vault: that vault, and no other. A call with no key is refused, as before.
//
// Before this, any key was told every vault on the server.
func TestListVaultsScope(t *testing.T) {
	s := newScopeServer(t)
	list := func(token string) ([]string, error) {
		req := &pb.ListVaultsRequest{}
		out, err := s.unary(token, req, func(ctx context.Context) (any, error) { return s.srv.ListVaults(ctx, req) })
		if err != nil {
			return nil, err
		}
		return out.(*pb.ListVaultsResponse).Vaults, nil
	}

	for _, mode := range []string{auth.ModeObserve, auth.ModeFull, auth.ModeAppend} {
		t.Run(mode+" key", func(t *testing.T) {
			vaults, err := list(s.key(t, mode))
			if err != nil {
				t.Fatalf("ListVaults: %v", err)
			}
			t.Logf("answer: %v", vaults)
			if len(vaults) != 1 || vaults[0] != s.pinned {
				t.Errorf("vaults = %v; want only the key's own, %q", vaults, s.pinned)
			}
		})
	}
	t.Run("write key", func(t *testing.T) {
		if _, err := list(s.key(t, auth.ModeWrite)); status.Code(err) != codes.PermissionDenied {
			t.Errorf("err = %v; want PermissionDenied", err)
		}
	})
	t.Run("no key", func(t *testing.T) {
		if _, err := list(""); status.Code(err) != codes.Unauthenticated {
			t.Errorf("err = %v; want Unauthenticated", err)
		}
	})
}
