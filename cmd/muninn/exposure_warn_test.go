package main

import "testing"

func TestIsLoopbackHost(t *testing.T) {
	cases := []struct {
		host string
		want bool
	}{
		{"127.0.0.1", true},
		{"::1", true},
		{"localhost", true},
		{"0.0.0.0", false},      // bind-all
		{"::", false},           // bind-all v6
		{"", false},             // empty host → binds all interfaces
		{"192.168.1.10", false}, // LAN
		{"203.0.113.7", false},  // public
		{"muninn.example.com", false},
	}
	for _, c := range cases {
		if got := isLoopbackHost(c.host); got != c.want {
			t.Errorf("isLoopbackHost(%q) = %v, want %v", c.host, got, c.want)
		}
	}
}

func TestShouldWarnDefaultPasswordExposure(t *testing.T) {
	cases := []struct {
		host         string
		defaultInUse bool
		want         bool
		reason       string
	}{
		{"127.0.0.1", true, false, "loopback + default password is fine (not reachable off-host)"},
		{"0.0.0.0", true, true, "exposed + default password must warn"},
		{"0.0.0.0", false, false, "exposed but password changed is fine"},
		{"192.168.1.10", true, true, "LAN + default password must warn"},
		{"localhost", true, false, "localhost is loopback"},
		{"", true, true, "bind-all + default password must warn"},
	}
	for _, c := range cases {
		if got := shouldWarnDefaultPasswordExposure(c.host, c.defaultInUse); got != c.want {
			t.Errorf("shouldWarnDefaultPasswordExposure(%q, %v) = %v, want %v — %s",
				c.host, c.defaultInUse, got, c.want, c.reason)
		}
	}
}
