package mbp

import (
	"encoding/json"
	"testing"
)

// TestStatRequest_ServerWideCannotComeOffTheWire pins the property SEC-24
// rests on: StatRequest.ServerWide has no wire name, so nothing a client can
// send sets it. Each payload below spells the field the ways a decoder might
// plausibly accept; all must leave it false.
func TestStatRequest_ServerWideCannotComeOffTheWire(t *testing.T) {
	spellings := []string{"server_wide", "ServerWide", "serverwide", "serverWide", "SERVERWIDE", "-"}

	for _, name := range spellings {
		payload, err := EncodeMsgpack(map[string]any{"vault": "pilot-station", name: true})
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		var req StatRequest
		if err := DecodeMsgpack(payload, &req); err != nil {
			t.Fatalf("msgpack %q: decode: %v", name, err)
		}
		if req.ServerWide {
			t.Errorf("a msgpack STAT payload set ServerWide through the key %q", name)
		}
		if req.Vault != "pilot-station" {
			t.Errorf("msgpack %q: vault = %q; the payload was not decoded at all", name, req.Vault)
		}
	}

	for _, name := range spellings {
		body, _ := json.Marshal(map[string]any{"vault": "pilot-station", name: true})
		var req StatRequest
		if err := json.Unmarshal(body, &req); err != nil {
			t.Fatalf("json %q: decode: %v", name, err)
		}
		if req.ServerWide {
			t.Errorf("a JSON body set ServerWide through the key %q", name)
		}
	}

	// And it never goes out either: a request that has it set encodes without it.
	out, err := EncodeMsgpack(&StatRequest{Vault: "pilot-station", ServerWide: true})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var seen map[string]any
	if err := DecodeMsgpack(out, &seen); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(seen) != 1 {
		t.Errorf("an encoded StatRequest carries %v; want only the vault", seen)
	}
}
