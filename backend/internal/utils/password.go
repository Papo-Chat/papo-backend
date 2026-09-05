package utils

import (
	"errors"
	"unicode"
	"unicode/utf8"
)

// Erros de política de senha.
var (
	ErrPasswordTooShort    = errors.New("senha abaixo do tamanho mínimo")
	ErrPasswordNoUppercase = errors.New("senha sem letra maiúscula")
	ErrPasswordNoSpecial   = errors.New("senha sem caractere especial")
)

// ValidatePassword aplica a política de senha: ao menos minLen caracteres,
// ao menos uma letra maiúscula e ao menos um caractere especial (qualquer
// caractere que não seja letra nem número).
func ValidatePassword(password string, minLen int) error {
	if utf8.RuneCountInString(password) < minLen {
		return ErrPasswordTooShort
	}

	var hasUpper, hasSpecial bool
	for _, r := range password {
		if unicode.IsUpper(r) {
			hasUpper = true
		}
		if !unicode.IsLetter(r) && !unicode.IsNumber(r) {
			hasSpecial = true
		}
		if hasUpper && hasSpecial {
			break
		}
	}

	if !hasUpper {
		return ErrPasswordNoUppercase
	}
	if !hasSpecial {
		return ErrPasswordNoSpecial
	}
	return nil
}
