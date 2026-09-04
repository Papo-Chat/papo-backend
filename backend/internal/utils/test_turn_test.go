package utils

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestTurnCredentialForUsername valida o vetor conhecido da credential RFC
// 5389: credential = base64(HMAC-SHA1(secret, username)) (formato exigido
// pelo coturn em modo REST API).
func TestTurnCredentialForUsername(t *testing.T) {
	got := turnCredentialForUsername([]byte("0123456789abcdef0123456789abcdef"), "1700000000:alice")
	want := "syM0noiTOlQo3X9F1YXulwwxriE="
	if got != want {
		t.Fatalf("credential = %s, esperado %s", got, want)
	}
}

func TestGenerateTurnCredential(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	userID := "alice"
	ttl := 3600 * time.Second

	before := time.Now().Add(ttl).Unix()
	username, credential := GenerateTurnCredential(secret, userID, ttl)
	after := time.Now().Add(ttl).Unix()

	// username = "<expiry_unix>:<user_id>"
	parts := strings.SplitN(username, ":", 2)
	if len(parts) != 2 || parts[1] != userID {
		t.Fatalf("username %q não tem o formato <expiry>:<user_id>", username)
	}
	expiry, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		t.Fatalf("expiry %q não é um inteiro: %v", parts[0], err)
	}
	if expiry < before || expiry > after {
		t.Errorf("expiry %d fora da janela [%d, %d]", expiry, before, after)
	}

	// credential = base64(HMAC-SHA1(secret, username)); SHA1 = 20 bytes = 28 base64.
	if len(credential) != 28 {
		t.Fatalf("credential tem %d chars, esperado 28", len(credential))
	}
	if got := turnCredentialForUsername(secret, username); got != credential {
		t.Errorf("credential %s != HMAC-SHA1(%q)=%s", credential, username, got)
	}
}
