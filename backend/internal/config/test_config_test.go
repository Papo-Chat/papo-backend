package config

import (
	"os"
	"testing"
)

func TestThumbnailEnabled(t *testing.T) {
	cases := []struct {
		name  string
		value string
		set   bool
		want  bool
	}{
		{"false", "false", true, false},
		{"zero", "0", true, false},
		{"true", "true", true, true},
		{"vazio usa padrao", "", true, true},
		{"invalido usa padrao", "lixo", true, true},
		{"ausente usa padrao", "", false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.set {
				t.Setenv("THUMBNAIL_ENABLED", tc.value)
			} else {
				t.Setenv("THUMBNAIL_ENABLED", "")
				os.Unsetenv("THUMBNAIL_ENABLED")
			}
			if got := LoadConfig().ThumbnailEnabled; got != tc.want {
				t.Errorf("ThumbnailEnabled = %v, esperado %v", got, tc.want)
			}
		})
	}
}

func TestGetEnvNonNegativeIntAllowsZero(t *testing.T) {
	t.Setenv("VOICE_ICE_UDP_PORT", "0")
	if got := getEnvNonNegativeInt("VOICE_ICE_UDP_PORT", 50000); got != 0 {
		t.Fatalf("got %d, want 0", got)
	}
}
