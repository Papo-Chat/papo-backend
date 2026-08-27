package storage

import (
	"context"
	"fmt"

	"papo/internal/models"
)

// serverColumns inclui password_hash e é usado somente por
// GetServerWithPasswordHash, que é a única função autorizada a retornar o
// hash do banco.
const serverColumns = "id, owner_id, name, icon_media, public_server, password_hash, created_at"

// serverPublicColumns é a visão de servidor sem password_hash, usada por
// todas as demais funções.
const serverPublicColumns = "id, owner_id, name, icon_media, public_server, created_at"

func scanServer(row rowScanner) (models.Server, error) {
	var server models.Server
	err := row.Scan(
		&server.ID,
		&server.OwnerID,
		&server.Name,
		&server.IconMedia,
		&server.PublicServer,
		&server.PasswordHash,
		&server.CreatedAt,
	)
	if err != nil {
		return models.Server{}, err
	}

	return server, nil
}

func scanServerPublic(row rowScanner) (models.Server, error) {
	var server models.Server
	err := row.Scan(
		&server.ID,
		&server.OwnerID,
		&server.Name,
		&server.IconMedia,
		&server.PublicServer,
		&server.CreatedAt,
	)
	if err != nil {
		return models.Server{}, err
	}

	return server, nil
}

// CreateServer cria um novo servidor público sem ícone e retorna o registro
// criado.
func CreateServer(ctx context.Context, name string, ownerID *string) (models.Server, error) {
	return CreateServerWithIcon(ctx, name, nil, true, ownerID, nil)
}

// CreateServerWithIcon cria um novo servidor com ícone opcional e retorna o
// registro criado, sem o password_hash. iconMedia é a referência do blob do
// ícone na tabela media (nil sem ícone); passwordHash é o hash bcrypt da
// senha (nil para servidor público).
func CreateServerWithIcon(ctx context.Context, name string, iconMedia *string, public bool, ownerID *string, passwordHash *string) (models.Server, error) {

	row := GetDB().QueryRowContext(ctx,
		"INSERT INTO servers (name, owner_id, icon_media, public_server, password_hash) VALUES ($1, $2, $3, $4, $5) RETURNING "+serverPublicColumns,
		name, ownerID, iconMedia, public, passwordHash,
	)

	server, err := scanServerPublic(row)
	if err != nil {
		return models.Server{}, mapStorageError(err)
	}

	return server, nil
}

// GetServerByID busca um servidor pelo id, sem o password_hash.
func GetServerByID(ctx context.Context, id string) (models.Server, error) {
	row := GetDB().QueryRowContext(ctx,
		"SELECT "+serverPublicColumns+" FROM servers WHERE id = $1",
		id,
	)

	server, err := scanServerPublic(row)
	if err != nil {
		return models.Server{}, mapStorageError(err)
	}

	return server, nil
}

// GetServerWithPasswordHash busca o servidor do backend (1 backend = 1
// servidor) incluindo o password_hash (nil para servidor público). É a única
// forma de obter o hash sem o id e deve ser usada somente para validar a
// senha do servidor em /auth/loginServer e para a regra de acesso de
// login/registro em servidores não públicos.
func GetServerWithPasswordHash(ctx context.Context) (models.Server, error) {
	row := GetDB().QueryRowContext(ctx,
		"SELECT "+serverColumns+" FROM servers LIMIT 1",
	)

	server, err := scanServer(row)
	if err != nil {
		return models.Server{}, mapStorageError(err)
	}

	return server, nil
}

// UserOwnsAnyServer indica se o usuário é dono de algum servidor.
func UserOwnsAnyServer(ctx context.Context, userID string) (bool, error) {
	var exists bool
	err := GetDB().QueryRowContext(ctx,
		"SELECT EXISTS (SELECT 1 FROM servers WHERE owner_id = $1)",
		userID,
	).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("falha ao verificar posse de servidor: %w", err)
	}

	return exists, nil
}

// ListServers lista todos os servidores, sem o password_hash, ordenados por
// data de criação.
func ListServers(ctx context.Context) ([]models.Server, error) {
	rows, err := GetDB().QueryContext(ctx,
		"SELECT "+serverPublicColumns+" FROM servers ORDER BY created_at, id",
	)
	if err != nil {
		return nil, fmt.Errorf("falha ao listar servidores: %w", err)
	}
	defer rows.Close()

	servers := make([]models.Server, 0)
	for rows.Next() {
		server, err := scanServerPublic(rows)
		if err != nil {
			return nil, fmt.Errorf("falha ao ler servidor: %w", err)
		}
		servers = append(servers, server)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("falha ao listar servidores: %w", err)
	}

	return servers, nil
}

// serverSummaryColumns é a seleção comum da visão ServerSummary: dados do
// servidor, username do dono (LEFT JOIN, pode ser NULL) e contagens de
// canais, membros (por enquanto todos os usuários pertencem ao mesmo
// servidor, então é o total da tabela users) e roles.
const serverSummaryColumns = `s.id, s.owner_id, u.username AS owner_username, s.name, s.icon_media,
	s.public_server, s.created_at,
	(SELECT COUNT(*) FROM channels ch WHERE ch.server_id = s.id) AS channel_count,
	(SELECT COUNT(*) FROM users) AS member_count,
	(SELECT COUNT(*) FROM roles r WHERE r.server_id = s.id) AS role_count`

func scanServerSummary(row rowScanner) (models.ServerSummary, error) {
	var summary models.ServerSummary
	err := row.Scan(
		&summary.ID,
		&summary.OwnerID,
		&summary.OwnerUsername,
		&summary.Name,
		&summary.IconMedia,
		&summary.Public,
		&summary.CreatedAt,
		&summary.ChannelCount,
		&summary.MemberCount,
		&summary.RoleCount,
	)
	if err != nil {
		return models.ServerSummary{}, err
	}

	return summary, nil
}

// ListServerSummaries lista todos os servidores com a visão ServerSummary,
// ordenados por data de criação.
func ListServerSummaries(ctx context.Context) ([]models.ServerSummary, error) {
	rows, err := GetDB().QueryContext(ctx,
		"SELECT "+serverSummaryColumns+
			" FROM servers s LEFT JOIN users u ON u.id = s.owner_id ORDER BY s.created_at, s.id",
	)
	if err != nil {
		return nil, fmt.Errorf("falha ao listar servidores: %w", err)
	}
	defer rows.Close()

	summaries := make([]models.ServerSummary, 0)
	for rows.Next() {
		summary, err := scanServerSummary(rows)
		if err != nil {
			return nil, fmt.Errorf("falha ao ler servidor: %w", err)
		}
		summaries = append(summaries, summary)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("falha ao listar servidores: %w", err)
	}

	return summaries, nil
}

// GetServerSummary busca um servidor pelo id com a visão ServerSummary.
func GetServerSummary(ctx context.Context, id string) (models.ServerSummary, error) {
	row := GetDB().QueryRowContext(ctx,
		"SELECT "+serverSummaryColumns+
			" FROM servers s LEFT JOIN users u ON u.id = s.owner_id WHERE s.id = $1",
		id,
	)

	summary, err := scanServerSummary(row)
	if err != nil {
		return models.ServerSummary{}, mapStorageError(err)
	}

	return summary, nil
}

// CountServers é usado para saber se o server já foi criado
func CountServers(ctx context.Context) (int, error) {
	var count int

	row := GetDB().QueryRowContext(ctx, "SELECT COUNT(*) FROM servers")
	err := row.Scan(&count)

	if err != nil {
		return 0, mapStorageError(err)
	}

	return count, nil
}

// UpdateServer atualiza o nome, o ícone, a visibilidade e a senha do servidor
// e retorna o registro atualizado, sem o password_hash. server.IconMedia é a
// referência do blob do ícone na tabela media (nil remove o ícone);
// passwordHash é o hash bcrypt da nova senha (nil para servidor público).
func UpdateServer(ctx context.Context, id string, server models.Server, passwordHash *string) (models.Server, error) {
	row := GetDB().QueryRowContext(ctx,
		`UPDATE servers
		 SET name = $2, icon_media = $3, public_server = $4, password_hash = $5
		 WHERE id = $1
		 RETURNING `+serverPublicColumns,
		id, server.Name, server.IconMedia, server.PublicServer, passwordHash,
	)

	updated, err := scanServerPublic(row)
	if err != nil {
		return models.Server{}, mapStorageError(err)
	}

	return updated, nil
}
