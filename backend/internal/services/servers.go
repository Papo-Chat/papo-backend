package services

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"papo/internal/models"
	"papo/internal/storage"
	"papo/internal/utils"
)

// ErrServerNotFound indica que o servidor não existe.
var ErrServerNotFound = errors.New("servidor não encontrado")
var ErrServerAlreadyCreated = errors.New("servidor já existe")

// maxIconBytes é o tamanho máximo de um ícone decodificado (2MB, README).
const maxIconBytes = 2 << 20

// maxServerNameLength é o tamanho máximo do nome de um servidor (32 caracteres, README).
const maxServerNameLength = 32

// ListServers lista todos os servidores com o username do dono e as
// contagens de canais, membros e roles.
func ListServers(ctx context.Context) ([]models.ServerSummary, error) {
	return storage.ListServerSummaries(ctx)
}

// GetServer retorna o detalhe do servidor pelo id.
// Retorna ErrServerNotFound quando o servidor não existe.
func GetServer(ctx context.Context, id string) (models.ServerSummary, error) {
	if id == "" {
		return models.ServerSummary{}, ErrServerNotFound
	}

	summary, err := storage.GetServerSummary(ctx, id)
	if errors.Is(err, storage.ErrNotFound) {
		return models.ServerSummary{}, ErrServerNotFound
	}
	if err != nil {
		return models.ServerSummary{}, err
	}

	return summary, nil
}

// CreateServer cria um novo servidor público sem ícone. O usuário que cria o
// servidor é o dono dele (README).
// PS: USADO APENAS INTERNAMENTE PARA TESTES
func CreateServer(ctx context.Context, name string, ownerID *string) (models.Server, error) {
	return CreateServerWithIcon(ctx, name, "", "", true, nil, ownerID)
}

// CreateServerWithIcon cria um novo servidor com ícone opcional. O ícone,
// quando informado, deve ser base64 de um GIF, JPEG ou PNG de até 2MB
// (README). public nil significa servidor público (default do schema);
// servidor privado (public=false) exige password não vazio. Retorna
// ErrInvalidInput quando o nome está vazio ou acima de 32 caracteres, quando
// o ícone não é um GIF, JPEG ou PNG válido de até 2MB com dimensões de até
// 512px ou quando o servidor é privado sem senha.
// Atualmente um backend só roda um servidor, então ao tentar criar outro servidor
// com o servidor atual criado não funciona
func CreateServerWithIcon(ctx context.Context, name, icon, iconFormat string, public bool, password *string, ownerID *string) (models.Server, error) {
	if name == "" || utf8.RuneCountInString(name) > maxServerNameLength {
		return models.Server{}, ErrInvalidInput
	}

	svCount, err := storage.CountServers(ctx)
	if err != nil {
		return models.Server{}, err
	}

	if svCount > 0 {
		return models.Server{}, ErrServerAlreadyCreated
	}

	var iconBlob []byte
	format := ""
	if icon != "" || iconFormat != "" {
		decoded, err := base64.StdEncoding.DecodeString(icon)
		if err != nil {
			return models.Server{}, ErrInvalidInput
		}

		format = strings.ToUpper(iconFormat)
		if !avatarContentMatchesFormat(decoded, format) {
			return models.Server{}, ErrInvalidInput
		}

		if len(decoded) > maxIconBytes {
			return models.Server{}, ErrInvalidInput
		}

		if err := utils.ValidateImage(decoded, utils.MaxImageDimension); err != nil {
			return models.Server{}, ErrInvalidInput
		}

		iconBlob = decoded
	}

	if password != nil {
		passwordHash, err := serverPasswordHash(public, *password)
		if err != nil {
			return models.Server{}, err
		}
		return storage.CreateServerWithIcon(ctx, name, iconBlob, format, public, ownerID, passwordHash)
	}

	return storage.CreateServerWithIcon(ctx, name, iconBlob, format, public, ownerID, nil)

}

// serverPasswordHash resolve o password_hash a partir do estado final do
// servidor: público não tem senha (nil), privado exige senha não vazia
// (hash bcrypt). Retorna ErrInvalidInput quando o servidor privado não tem
// senha.
func serverPasswordHash(isPublic bool, password string) (*string, error) {
	if isPublic {
		return nil, nil
	}
	if password == "" {
		return nil, ErrInvalidInput
	}

	hash, err := utils.HashPassword(password)
	if err != nil {
		return nil, fmt.Errorf("falha ao gerar hash da senha do servidor: %w", err)
	}
	return &hash, nil
}

// UpdateServer atualiza o nome, o ícone, a visibilidade e a senha do
// servidor. Quando icon e iconFormat são vazios, o ícone é removido (blob
// NULL e ”). public nil mantém a visibilidade atual; quando o estado final
// é privado (public=false), password não pode ser vazio.
// Retorna ErrServerNotFound quando o servidor não existe, ErrInvalidInput
// quando o nome está vazio ou acima de 32 caracteres, quando o ícone não é
// um GIF, JPEG ou PNG válido de até 2MB com dimensões de até 512px ou quando
// o servidor privado não tem senha.
func UpdateServer(ctx context.Context, id, name, icon, iconFormat string, public *bool, password *string) error {
	if id == "" {
		return ErrServerNotFound
	}
	if name == "" || utf8.RuneCountInString(name) > maxServerNameLength {
		return ErrInvalidInput
	}

	current, err := storage.GetServerByID(ctx, id)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return ErrServerNotFound
		}
		return err
	}

	var iconBlob []byte
	format := ""
	if icon != "" || iconFormat != "" {
		decoded, err := base64.StdEncoding.DecodeString(icon)
		if err != nil {
			return ErrInvalidInput
		}

		format = strings.ToUpper(iconFormat)
		if !avatarContentMatchesFormat(decoded, format) {
			return ErrInvalidInput
		}

		if len(decoded) > maxIconBytes {
			return ErrInvalidInput
		}

		if err := utils.ValidateImage(decoded, utils.MaxImageDimension); err != nil {
			return ErrInvalidInput
		}

		iconBlob = decoded
	}

	isPublic := current.PublicServer
	if public != nil {
		isPublic = *public
	}

	//se o servidor não for publico e a senha não for em branco, grava os dados inteiros no banco
	//se o servidor for publico ou a senha estiver em branco, não grava senha e deixa o servidor público
	if isPublic == false && password != nil {
		passwordHash, err := serverPasswordHash(isPublic, *password)
		if err != nil {
			return err
		}
		if _, err := storage.UpdateServer(ctx, id, models.Server{
			Name:         name,
			IconBlob:     iconBlob,
			PublicServer: isPublic,
			IconFormat:   format,
		}, passwordHash); err != nil {
			return fmt.Errorf("falha ao atualizar o servidor: %w", err)
		}
		return nil
	} else {
		if _, err := storage.UpdateServer(ctx, id, models.Server{
			Name:         name,
			IconBlob:     iconBlob,
			PublicServer: true,
			IconFormat:   format,
		}, nil); err != nil {
			return fmt.Errorf("falha ao atualizar o servidor: %w", err)
		}
	}

	return nil
}
