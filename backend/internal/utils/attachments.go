package utils

import (
	"bytes"
	"encoding/binary"
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
	if mime := detectContainerMimeType(content); mime != "" {
		return mime
	}
	return http.DetectContentType(content)
}

// detectContainerMimeType recognizes common video containers that
// http.DetectContentType reports as application/octet-stream. It only inspects
// bounded container headers; it never trusts the uploaded filename or headers.
func detectContainerMimeType(content []byte) string {
	if len(content) >= 16 && string(content[4:8]) == "ftyp" {
		boxSize := uint64(binary.BigEndian.Uint32(content[:4]))
		brandOffset := 8
		if boxSize == 1 {
			if len(content) < 24 {
				return ""
			}
			boxSize = binary.BigEndian.Uint64(content[8:16])
			brandOffset = 16
		}
		// A file type box contains the header, major brand, and minor version.
		// Bound its declared size to the attachment upload limit.
		if boxSize < uint64(brandOffset+8) || boxSize > maxVideoFileSize {
			return ""
		}
		if len(content) < brandOffset+4 {
			return ""
		}
		brand := string(content[brandOffset : brandOffset+4])
		switch brand {
		case "qt  ":
			return "video/quicktime"
		case "M4A ", "M4B ", "M4P ", "F4A ":
			return "audio/mp4"
		case "avif", "avis":
			return "image/avif"
		case "heic", "heix", "hevc", "hevx", "heim", "heis", "hevm", "hevs", "mif1", "msf1":
			return "image/heic"
		case "3gp4", "3gp5", "3gp6", "3ge6", "3ge7":
			return "video/3gpp"
		case "3g2a", "3g2b":
			return "video/3gpp2"
		default:
			// ISO-BMFF ftyp brands cover MP4/M4V and compatible variants.
			return "video/mp4"
		}
	}

	if len(content) >= 12 && string(content[:4]) == "RIFF" && string(content[8:12]) == "AVI " {
		riFFSize := uint64(binary.LittleEndian.Uint32(content[4:8]))
		if riFFSize >= 4 && riFFSize+8 <= maxVideoFileSize {
			return "video/x-msvideo"
		}
	}

	if mime := detectEBMLMimeType(content); mime != "" {
		return mime
	}
	return ""
}

// detectEBMLMimeType reads the DocType element from the EBML header so a
// Matroska file is not mislabeled as WebM. The header is small and parsed with
// strict bounds; malformed or truncated headers fall back to generic sniffing.
func detectEBMLMimeType(content []byte) string {
	if len(content) < 6 || content[0] != 0x1a || content[1] != 0x45 ||
		content[2] != 0xdf || content[3] != 0xa3 {
		return ""
	}
	r := bytes.NewReader(content)
	headerID, idLen, ok := readEBMLVINT(r, 0, int64(len(content)), true)
	if !ok || headerID != ebmlHeaderID {
		return ""
	}
	size, sizeLen, ok := readEBMLVINT(r, int64(idLen), int64(len(content)), false)
	if !ok || size == 0 || size == (uint64(1)<<(7*sizeLen))-1 {
		return ""
	}
	payloadStart := int64(idLen + sizeLen)
	if size > uint64(int64(len(content))-payloadStart) {
		return ""
	}
	headerEnd := payloadStart + int64(size)
	for offset := payloadStart; offset < headerEnd; {
		elemID, elemIDLen, ok := readEBMLVINT(r, offset, headerEnd, true)
		if !ok {
			return ""
		}
		sizeOffset := offset + int64(elemIDLen)
		elemSize, elemSizeLen, ok := readEBMLVINT(r, sizeOffset, headerEnd, false)
		if !ok || elemSize == 0 || elemSize == (uint64(1)<<(7*elemSizeLen))-1 {
			return ""
		}
		dataOffset := sizeOffset + int64(elemSizeLen)
		if dataOffset > headerEnd || elemSize > uint64(headerEnd-dataOffset) {
			return ""
		}
		if elemID == 0x4282 { // DocType
			if elemSize > 32 {
				return ""
			}
			docType := string(content[dataOffset : dataOffset+int64(elemSize)])
			switch strings.ToLower(docType) {
			case "webm":
				return "video/webm"
			case "matroska":
				return "video/x-matroska"
			default:
				return ""
			}
		}
		if dataOffset+int64(elemSize) <= offset {
			return ""
		}
		offset = dataOffset + int64(elemSize)
	}
	return ""
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
