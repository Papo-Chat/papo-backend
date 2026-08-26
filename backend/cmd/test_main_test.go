package main

import (
	"strings"
	"testing"
)

func TestValidateJWTSecret(t *testing.T) {
	cases := []struct {
		name    string
		secret  string
		wantErr bool
		wantMsg string
	}{
		{"vazio", "", true, "ausente"},
		{"1 caractere", "a", true, "muito curto"},
		{"31 caracteres", strings.Repeat("a", 31), true, "muito curto"},
		{"32 caracteres", strings.Repeat("a", 32), false, ""},
		{"33 caracteres", strings.Repeat("a", 33), false, ""},
		{"segredo longo", strings.Repeat("b", 172), false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateJWTSecret(tc.secret)
			if (err != nil) != tc.wantErr {
				t.Fatalf("validateJWTSecret erro = %v, esperado erro = %v", err, tc.wantErr)
			}
			if tc.wantErr && !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("validateJWTSecret mensagem = %q, deveria conter %q", err.Error(), tc.wantMsg)
			}
		})
	}
}
