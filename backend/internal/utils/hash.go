package utils

import "golang.org/x/crypto/bcrypt"

// HashPassword gera o hash bcrypt de uma senha.
func HashPassword(password string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(hash), nil
}

// CheckPassword compara uma senha com o hash bcrypt correspondente.
// Retorna bcrypt.ErrMismatchedHashAndPassword quando a senha não confere.
func CheckPassword(password, hash string) error {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password))
}
