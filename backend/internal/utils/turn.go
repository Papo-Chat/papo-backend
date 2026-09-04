package utils

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"fmt"
	"time"
)

// GenerateTurnCredential gera uma credencial efêmera RFC 5389 para um
// usuário: username = "<ttl_unix>:<user_id>" e
// credential = base64(HMAC-SHA1(secret, username)).
//
// O user_id no username torna a credencial auditável e revogável por
// usuário (o coturn registra o username em log); o TTL curto limita a
// janela de validade. Nunca logar o credential.
func GenerateTurnCredential(secret []byte, userID string, ttl time.Duration) (username, credential string) {
	username = fmt.Sprintf("%d:%s", time.Now().Add(ttl).Unix(), userID)
	credential = turnCredentialForUsername(secret, username)
	return username, credential
}

// turnCredentialForUsername computa a credential RFC 5389 para um username
// (base64(HMAC-SHA1(secret, username))). Pura, para teste com vetor conhecido.
//
// O coturn (modo REST API) exige base64, não hex: usar hex faz o TURN
// autenticar como falha (credential inválida).
func turnCredentialForUsername(secret []byte, username string) string {
	mac := hmac.New(sha1.New, secret)
	mac.Write([]byte(username))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}
