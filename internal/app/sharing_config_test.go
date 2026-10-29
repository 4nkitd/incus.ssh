package app

import "testing"

func TestSharingIsOptionalAndRequiresAnOperatorOriginAndDirectory(t *testing.T) {
	cfg := validConfig()
	if err := cfg.Validate(); err != nil || cfg.Sharing != nil {
		t.Fatal("default configuration enabled public sharing", err)
	}
	cfg.Sharing = &Sharing{StateDirectory: "/var/lib/incus-for-devs/opentunnel"}
	if err := cfg.Validate(); err != nil || cfg.Sharing.API != "https://opentunnel.xyz" {
		t.Fatal("explicit optional capability is invalid", err)
	}
	for _, sharing := range []Sharing{
		{API: "http://relay.test", StateDirectory: "/var/lib/incus-for-devs/opentunnel"},
		{API: "https://token:secret@relay.test", StateDirectory: "/var/lib/incus-for-devs/opentunnel"},
		{API: "https://relay.test/path", StateDirectory: "/var/lib/incus-for-devs/opentunnel"},
		{API: "https://relay.test?token=secret", StateDirectory: "/var/lib/incus-for-devs/opentunnel"},
		{StateDirectory: "relative"},
		{StateDirectory: "/"},
		{StateDirectory: "/var/lib/../state"},
	} {
		cfg := validConfig()
		cfg.Sharing = &sharing
		if err := cfg.Validate(); err == nil {
			t.Fatalf("accepted unsafe sharing configuration %+v", sharing)
		}
	}
}
