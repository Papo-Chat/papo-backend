package utils

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// JWTExpiration é a validade do token (24h, igual ao Max-Age do cookie Auth).
const JWTExpiration = 24 * time.Hour

// TempTokenExpiration é a validade do token temporário de acesso ao servidor
// (30min, igual ao Max-Age do cookie Auth emitido por /auth/login_server).
const TempTokenExpiration = 30 * time.Minute

// tempTokenClaims estende os claims registrados com a marca "temp" que
// identifica o token como autorização temporária de acesso ao servidor (não
// é um token de sessão de usuário).
type tempTokenClaims struct {
	jwt.RegisteredClaims
	Temp bool `json:"temp"`
}

// GenerateSessionToken gera o JWT de sessão (HS256) do usuário de forma
// determinística a partir de (userID, issuedAt): iat truncado a segundos e
// exp = iat + JWTExpiration. O token é uma função pura de (user_id, iat,
// segredo), então o servidor consegue re-derivá-lo a partir do
// token_issued_at guardado no banco (janela de graça da rotação).
func GenerateSessionToken(userID string, issuedAt time.Time, secret string) (string, error) {
	iat := issuedAt.Truncate(time.Second)
	claims := jwt.RegisteredClaims{
		Subject:   userID,
		IssuedAt:  jwt.NewNumericDate(iat),
		ExpiresAt: jwt.NewNumericDate(iat.Add(JWTExpiration)),
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString([]byte(secret))
}

// GenerateToken gera um JWT (HS256) de sessão para o usuário, com o ID do
// usuário como subject e o horário atual como iat.
func GenerateToken(userID, secret string) (string, error) {
	return GenerateSessionToken(userID, time.Now(), secret)
}

// HashToken retorna o SHA-256 hex do token: é a única forma em que o token é
// guardado no banco (user_connections.token_hash).
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// ValidateToken valida o JWT e retorna o ID do usuário (subject).
func ValidateToken(tokenString, secret string) (string, error) {
	token, err := jwt.Parse(tokenString, func(token *jwt.Token) (interface{}, error) {
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("método de assinatura inesperado: %v", token.Header["alg"])
		}
		return []byte(secret), nil
	})
	if err != nil {
		return "", err
	}
	if !token.Valid {
		return "", fmt.Errorf("token inválido")
	}
	return token.Claims.GetSubject()
}

// GenerateTempToken gera o JWT temporário de acesso ao servidor (HS256). Ele é
// a pré-autenticação de servidores não públicos: após /auth/login_server validar
// a senha do servidor, o token permite que o usuário tente logar ou registrar
// antes de consolidar o cookie de sessão.
func GenerateTempToken(secret string) (string, error) {
	now := time.Now()
	claims := tempTokenClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(TempTokenExpiration)),
		},
		Temp: true,
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString([]byte(secret))
}

// ValidateTempToken valida o JWT e retorna true apenas quando ele é um token
// temporário de acesso ao servidor válido (assinatura HMAC correta, não
// expirado e com a marca "temp"). Tokens de sessão de usuário retornam false.
func ValidateTempToken(tokenString, secret string) (bool, error) {
	token, err := jwt.ParseWithClaims(tokenString, &tempTokenClaims{}, func(token *jwt.Token) (interface{}, error) {
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("método de assinatura inesperado: %v", token.Header["alg"])
		}
		return []byte(secret), nil
	})
	if err != nil {
		return false, err
	}
	if !token.Valid {
		return false, fmt.Errorf("token inválido")
	}
	claims, ok := token.Claims.(*tempTokenClaims)
	if !ok || !claims.Temp {
		return false, fmt.Errorf("não é um token temporário de acesso ao servidor")
	}
	return true, nil
}
