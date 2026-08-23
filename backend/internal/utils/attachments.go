package utils

import (
	"net/http"
	"net/url"
	"strings"
	"unicode"
)

// SanitizeFileName limpa o nome de arquivo recebido do usuário de um upload:
// remove os componentes de caminho (separadores Unix e Windows) e os
// caracteres de controle, e faz trim das pontas. O nome resultante é seguro
// para uso em respostas da API e em header Content-Disposition. O caminho
// do blob em disco nunca é derivado deste nome (é derivado do sha_hash).
func SanitizeFileName(name string) string {
	if idx := strings.LastIndexAny(name, "/\\"); idx >= 0 {
		name = name[idx+1:]
	}

	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, name)

	return strings.TrimSpace(name)
}

// DetectMimeType detecta o MIME type a partir dos bytes iniciais do
// conteúdo (magic bytes), sem confiar no header de content type do upload.
func DetectMimeType(content []byte) string {
	return http.DetectContentType(content)
}

// ContentDisposition monta o header Content-Disposition de um download
// (README: GET /attachments/:file_id). Nomes ASCII são usados direto em
// filename; nomes com caracteres fora do ASCII usam um fallback ASCII em
// filename e o nome UTF-8 em filename* (RFC 6266).
func ContentDisposition(filename string) string {
	ascii := strings.Map(func(r rune) rune {
		if r >= 0x20 && r < 0x7f && r != '"' && r != '\\' {
			return r
		}
		return -1
	}, filename)
	if ascii == "" {
		ascii = "download"
	}
	if ascii == filename {
		return `attachment; filename="` + ascii + `"`
	}
	return `attachment; filename="` + ascii + `"; filename*=UTF-8''` + url.PathEscape(filename)
}
