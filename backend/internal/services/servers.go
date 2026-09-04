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

// GetServer retorna o servidor do backend (1 backend = 1 servidor) com o
// username do dono e as contagens de canais, membros e roles. O blob do ícone
// e o formato são resolvidos da tabela media e do disco.
// Retorna ErrServerNotFound quando o servidor não existe.
func GetServer(ctx context.Context) (models.ServerSummary, error) {
	summary, err := storage.GetServerSummary(ctx)
	if errors.Is(err, storage.ErrNotFound) {
		return models.ServerSummary{}, ErrServerNotFound
	}
	if err != nil {
		return models.ServerSummary{}, err
	}

	if err := resolveServerSummaryIcon(ctx, &summary); err != nil {
		return models.ServerSummary{}, err
	}

	return summary, nil
}

// resolveServerSummaryIcon preenche IconBlob e IconFormat a partir da
// referência media do servidor (sem efeito quando não há ícone).
func resolveServerSummaryIcon(ctx context.Context, summary *models.ServerSummary) error {
	if summary.IconMedia == nil {
		return nil
	}

	media, err := storage.GetMediaByHash(ctx, *summary.IconMedia)
	if err != nil {
		return err
	}
	blob, err := MediaContent(*summary.IconMedia)
	if err != nil {
		return err
	}

	summary.IconBlob = blob
	summary.IconFormat = mimeToFormat(media.MimeType)
	return nil
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
// O sistema tem um único servidor (coluna singleton UNIQUE na tabela
// servers): tentar criar um segundo servidor retorna ErrServerAlreadyCreated.
// Com ícone, o singleton é verificado antes de gravar o blob (evita blob
// órfão); a constraint única continua como garantia final.
func CreateServerWithIcon(ctx context.Context, name, icon, iconFormat string, public bool, password *string, ownerID *string) (models.Server, error) {
	if name == "" || utf8.RuneCountInString(name) > maxServerNameLength {
		return models.Server{}, ErrInvalidInput
	}

	var iconMedia *string
	if icon != "" || iconFormat != "" {
		decoded, err := base64.StdEncoding.DecodeString(icon)
		if err != nil {
			return models.Server{}, ErrInvalidInput
		}

		format := strings.ToUpper(iconFormat)
		if !avatarContentMatchesFormat(decoded, format) {
			return models.Server{}, ErrInvalidInput
		}

		if len(decoded) > maxIconBytes {
			return models.Server{}, ErrInvalidInput
		}

		if err := utils.ValidateImage(decoded, utils.MaxImageDimension); err != nil {
			return models.Server{}, ErrInvalidInput
		}

		// Verifica o singleton antes de gravar o blob: sem isso, uma criação
		// repetida com ícone distinto grava mídia e a constraint única só
		// falha na inserção, deixando blob órfão (acumulável via POST /server).
		if _, err := storage.GetServer(ctx); err == nil {
			return models.Server{}, ErrServerAlreadyCreated
		} else if !errors.Is(err, storage.ErrNotFound) {
			return models.Server{}, err
		}

		sha, _, err := StoreMediaFromBytes(ctx, decoded, formatToMime(format))
		if err != nil {
			return models.Server{}, fmt.Errorf("falha ao gravar o ícone do servidor: %w", err)
		}
		iconMedia = &sha
	}

	var passwordHash *string
	if password != nil {
		hash, err := serverPasswordHash(public, *password)
		if err != nil {
			return models.Server{}, err
		}
		passwordHash = hash
	}

	server, err := storage.CreateServerWithIcon(ctx, name, iconMedia, public, ownerID, passwordHash)
	if errors.Is(err, storage.ErrUniqueViolation) {
		return models.Server{}, ErrServerAlreadyCreated
	}
	if err != nil {
		return models.Server{}, err
	}

	if ownerID != nil {
		RecordAudit(ctx, AuditEntry{
			ActorID:    *ownerID,
			Action:     ActionServerCreate,
			EntityType: EntityServer,
			EntityID:   &server.ID,
			Metadata: map[string]any{
				"name":   name,
				"public": public,
			},
		})
	}

	return server, nil
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
func UpdateServer(ctx context.Context, actorID, name, icon, iconFormat string, public *bool, password *string) error {
	if name == "" || utf8.RuneCountInString(name) > maxServerNameLength {
		return ErrInvalidInput
	}

	current, err := storage.GetServer(ctx)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return ErrServerNotFound
		}
		return err
	}

	var iconMedia *string
	if icon != "" || iconFormat != "" {
		decoded, err := base64.StdEncoding.DecodeString(icon)
		if err != nil {
			return ErrInvalidInput
		}

		format := strings.ToUpper(iconFormat)
		if !avatarContentMatchesFormat(decoded, format) {
			return ErrInvalidInput
		}

		if len(decoded) > maxIconBytes {
			return ErrInvalidInput
		}

		if err := utils.ValidateImage(decoded, utils.MaxImageDimension); err != nil {
			return ErrInvalidInput
		}

		sha, _, err := StoreMediaFromBytes(ctx, decoded, formatToMime(format))
		if err != nil {
			return fmt.Errorf("falha ao gravar o ícone do servidor: %w", err)
		}
		iconMedia = &sha
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
		if _, err := storage.UpdateServer(ctx, current.ID, models.Server{
			Name:         name,
			IconMedia:    iconMedia,
			PublicServer: isPublic,
		}, passwordHash); err != nil {
			return fmt.Errorf("falha ao atualizar o servidor: %w", err)
		}
	} else {
		if _, err := storage.UpdateServer(ctx, current.ID, models.Server{
			Name:         name,
			IconMedia:    iconMedia,
			PublicServer: true,
		}, nil); err != nil {
			return fmt.Errorf("falha ao atualizar o servidor: %w", err)
		}
	}

	RecordAudit(ctx, AuditEntry{
		ActorID:    actorID,
		Action:     ActionServerUpdate,
		EntityType: EntityServer,
		EntityID:   &current.ID,
		Metadata: map[string]any{
			"name":   name,
			"public": isPublic,
		},
	})

	return nil
}
