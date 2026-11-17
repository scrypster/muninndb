package grpc_test

import (
	"context"
	"strings"
	"testing"

	"github.com/scrypster/muninndb/internal/auth"
	pb "github.com/scrypster/muninndb/proto/gen/go/muninn/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestInvalidVaultNameIsRefusedWithoutBeingRepeated pins that a request naming
// something no vault can be called is refused as that: the name is not looked
// up, so it is not remembered, and the refusal does not carry it back
// (SEC-27). A gRPC message can hold megabytes in that field, and a caller with
// no key is asked about its vault before it has shown any credential.
func TestInvalidVaultNameIsRefusedWithoutBeingRepeated(t *testing.T) {
	s := newScopeServer(t)
	stat := func(token, vault string) error {
		req := &pb.StatRequest{Vault: vault}
		_, err := s.unary(token, req, func(ctx context.Context) (any, error) { return s.srv.Stat(ctx, req) })
		return err
	}
	bad := []string{strings.Repeat("v", 65), strings.Repeat("v", 1<<20), "Harbour-Notices", "harbour notices"}

	// The status code is the one each caller got before the name was checked.
	for _, caller := range []struct {
		name, token string
		code        codes.Code
	}{
		{"no key", "", codes.Unauthenticated},
		{"a key naming a vault that is not its own", s.key(t, auth.ModeFull), codes.PermissionDenied},
	} {
		t.Run(caller.name, func(t *testing.T) {
			for _, name := range bad {
				err := stat(caller.token, name)
				if status.Code(err) != caller.code || status.Convert(err).Message() != "invalid vault name" {
					t.Errorf("%d-byte name %.20q: %.120v; want %s \"invalid vault name\"", len(name), name, err, caller.code)
				}
			}
		})
	}

	t.Run("well-formed names are answered as before", func(t *testing.T) {
		if err := stat("", s.public); err != nil {
			t.Errorf("no key, public vault: %v", err)
		}
		if err := stat("", "tug-roster"); status.Code(err) != codes.Unauthenticated || !strings.Contains(err.Error(), `vault "tug-roster" requires an API key`) {
			t.Errorf("no key, unconfigured vault: %v", err)
		}
		if err := stat(s.key(t, auth.ModeFull), s.public); status.Code(err) != codes.PermissionDenied {
			t.Errorf("a key naming another vault: %v", err)
		}
	})
}
