package config

import "testing"

func TestKlipyServerSideApprovalDefaultsOff(t *testing.T) {
	t.Setenv("KLIPY_API_KEY", "test-key")
	t.Setenv("KLIPY_SERVER_SIDE_APPROVED", "")
	if got := LoadConfig().KlipyServerSideApproved; got {
		t.Fatal("KlipyServerSideApproved must default to false")
	}
	t.Setenv("KLIPY_SERVER_SIDE_APPROVED", "true")
	if got := LoadConfig().KlipyServerSideApproved; !got {
		t.Fatal("KlipyServerSideApproved should be true when explicitly enabled")
	}
}

func TestKlipyAPIKeyIsTrimmed(t *testing.T) {
	t.Setenv("KLIPY_API_KEY", "  app-key  ")
	if got := LoadConfig().KlipyAPIKey; got != "app-key" {
		t.Fatalf("KlipyAPIKey = %q, want %q", got, "app-key")
	}
}
