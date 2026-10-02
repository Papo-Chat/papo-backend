package services

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"unicode/utf8"

	"papo/internal/config"
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
// quando informado, deve ser base64 de um GIF, JPEG, PNG ou WEBP de até 2MB
// (README). public nil significa servidor público (default do schema);
// servidor privado (public=false) exige password não vazio. Retorna
// ErrInvalidInput quando o nome está vazio ou acima de 32 caracteres, quando
// o ícone não é um GIF, JPEG, PNG ou WEBP válido de até 2MB com dimensões de
// até 512px ou quando o servidor é privado sem senha.
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

		format := normalizeImageFormat(iconFormat)
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
// (hash bcrypt) conforme à política de senha (tamanho mínimo, maiúscula,
// especial). Retorna ErrInvalidInput quando o servidor privado não tem
// senha e os erros de utils quando a senha viola a política.
func serverPasswordHash(isPublic bool, password string) (*string, error) {
	if isPublic {
		return nil, nil
	}
	if password == "" {
		return nil, ErrInvalidInput
	}

	cfg := config.LoadConfig()
	if err := utils.ValidatePassword(password, cfg.MinPasswordLength); err != nil {
		return nil, err
	}

	hash, err := utils.HashPassword(password)
	if err != nil {
		return nil, fmt.Errorf("falha ao gerar hash da senha do servidor: %w", err)
	}
	return &hash, nil
}

// ServerUpdateResult descreve efeitos de segurança da atualização do servidor.
type ServerUpdateResult struct {
	PasswordChanged bool
}

// ReplaceServer substitui todos os campos mutáveis do servidor. Todos os
// argumentos fazem parte do estado completo: name, icon/iconFormat, public e
// password. Ícone vazio remove o ícone; servidor privado exige senha válida.
func ReplaceServer(ctx context.Context, actorID, name, icon, iconFormat, password string, public bool) error {
	_, err := ReplaceServerWithResult(ctx, actorID, name, icon, iconFormat, password, public)
	return err
}

// ReplaceServerWithResult tem a mesma semântica de ReplaceServer e também
// informa se a credencial de acesso do servidor mudou.
func ReplaceServerWithResult(ctx context.Context, actorID, name, icon, iconFormat, password string, public bool) (ServerUpdateResult, error) {
	return updateServer(ctx, actorID, &name, &icon, &iconFormat, &public, &password, true)
}

// PatchServer aplica uma atualização parcial nos campos mutáveis do servidor.
// Campos ausentes preservam o estado atual. icon vazio remove o ícone;
// icon não vazio exige iconFormat. public nil mantém a visibilidade;
// password nil mantém a senha quando o servidor já é privado. Ao tornar um
// servidor público, o hash da senha é removido.
func PatchServer(ctx context.Context, actorID string, name, icon, iconFormat *string, public *bool, password *string) error {
	_, err := PatchServerWithResult(ctx, actorID, name, icon, iconFormat, public, password)
	return err
}

// PatchServerWithResult tem a mesma semântica de PatchServer e também informa
// se a credencial de acesso do servidor mudou.
func PatchServerWithResult(ctx context.Context, actorID string, name, icon, iconFormat *string, public *bool, password *string) (ServerUpdateResult, error) {
	return updateServer(ctx, actorID, name, icon, iconFormat, public, password, false)
}

func serverPasswordChanged(currentHash, nextHash *string, password *string) bool {
	if currentHash == nil || nextHash == nil {
		return (currentHash == nil) != (nextHash == nil)
	}
	if password == nil {
		return false
	}
	return utils.CheckPassword(*password, *currentHash) != nil
}

func updateServer(ctx context.Context, actorID string, name, icon, iconFormat *string, public *bool, password *string, requireAll bool) (ServerUpdateResult, error) {
	if requireAll && (name == nil || icon == nil || iconFormat == nil || public == nil || password == nil) {
		return ServerUpdateResult{}, ErrInvalidInput
	}

	current, err := storage.GetServerWithPasswordHash(ctx)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return ServerUpdateResult{}, ErrServerNotFound
		}
		return ServerUpdateResult{}, err
	}

	finalName := current.Name
	if name != nil {
		if *name == "" || utf8.RuneCountInString(*name) > maxServerNameLength {
			return ServerUpdateResult{}, ErrInvalidInput
		}
		finalName = *name
	}

	iconMedia := current.IconMedia
	switch {
	case icon == nil && iconFormat == nil:
		// PATCH sem campos de ícone preserva o ícone atual.
	case icon == nil:
		return ServerUpdateResult{}, ErrInvalidInput
	case *icon == "":
		if iconFormat != nil && *iconFormat != "" {
			return ServerUpdateResult{}, ErrInvalidInput
		}
		iconMedia = nil
	default:
		if iconFormat == nil || *iconFormat == "" {
			return ServerUpdateResult{}, ErrInvalidInput
		}

		decoded, err := base64.StdEncoding.DecodeString(*icon)
		if err != nil {
			return ServerUpdateResult{}, ErrInvalidInput
		}

		format := normalizeImageFormat(*iconFormat)
		if !avatarContentMatchesFormat(decoded, format) {
			return ServerUpdateResult{}, ErrInvalidInput
		}
		if len(decoded) > maxIconBytes {
			return ServerUpdateResult{}, ErrInvalidInput
		}
		if err := utils.ValidateImage(decoded, utils.MaxImageDimension); err != nil {
			return ServerUpdateResult{}, ErrInvalidInput
		}

		sha, _, err := StoreMediaFromBytes(ctx, decoded, formatToMime(format))
		if err != nil {
			return ServerUpdateResult{}, fmt.Errorf("falha ao gravar o ícone do servidor: %w", err)
		}
		iconMedia = &sha
	}

	isPublic := current.PublicServer
	if public != nil {
		isPublic = *public
	}

	passwordHash := current.PasswordHash
	if isPublic {
		passwordHash = nil
	} else if password != nil {
		passwordHash, err = serverPasswordHash(false, *password)
		if err != nil {
			return ServerUpdateResult{}, err
		}
	} else if passwordHash == nil {
		return ServerUpdateResult{}, ErrInvalidInput
	}

	passwordChanged := serverPasswordChanged(current.PasswordHash, passwordHash, password)
	next := models.Server{
		Name:         finalName,
		IconMedia:    iconMedia,
		PublicServer: isPublic,
	}

	if passwordChanged {
		if _, _, err := storage.UpdateServerAndRevokeAllConnections(ctx, current.ID, next, passwordHash); err != nil {
			return ServerUpdateResult{}, fmt.Errorf("falha ao atualizar o servidor: %w", err)
		}
	} else if _, err := storage.UpdateServer(ctx, current.ID, next, passwordHash); err != nil {
		return ServerUpdateResult{}, fmt.Errorf("falha ao atualizar o servidor: %w", err)
	}

	RecordAudit(ctx, AuditEntry{
		ActorID:    actorID,
		Action:     ActionServerUpdate,
		EntityType: EntityServer,
		EntityID:   &current.ID,
		Metadata: map[string]any{
			"name":   finalName,
			"public": isPublic,
		},
	})

	return ServerUpdateResult{PasswordChanged: passwordChanged}, nil
}
