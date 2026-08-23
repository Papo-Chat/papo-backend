package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"papo/internal/models"
)

// userColumns inclui password_hash e é usado somente por GetUserByUsername,
// que é a única função autorizada a retornar o hash do banco.
const userColumns = "id, username, nickname, password_hash, avatar_blob, avatar_format, banned, reset_password, last_ip, status_message, status_updated_at, created_at"

// userPublicColumns é a visão de usuário sem password_hash, usada por todas
// as demais funções.
const userPublicColumns = "id, username, nickname, avatar_blob, avatar_format, banned, reset_password, last_ip, status_message, status_updated_at, created_at"

// userSummaryColumns é a visão reduzida para listagens (GET /users).
const userSummaryColumns = "id, username, nickname, status_message, status_updated_at, created_at"

func scanUser(row rowScanner) (models.User, error) {
	var user models.User
	err := row.Scan(
		&user.ID,
		&user.Username,
		&user.Nickname,
		&user.PasswordHash,
		&user.AvatarBlob,
		&user.AvatarFormat,
		&user.Banned,
		&user.ResetPassword,
		&user.LastIP,
		&user.StatusMessage,
		&user.StatusUpdatedAt,
		&user.CreatedAt,
	)
	if err != nil {
		return models.User{}, err
	}

	return user, nil
}

func scanUserPublic(row rowScanner) (models.User, error) {
	var user models.User
	err := row.Scan(
		&user.ID,
		&user.Username,
		&user.Nickname,
		&user.AvatarBlob,
		&user.AvatarFormat,
		&user.Banned,
		&user.ResetPassword,
		&user.LastIP,
		&user.StatusMessage,
		&user.StatusUpdatedAt,
		&user.CreatedAt,
	)
	if err != nil {
		return models.User{}, err
	}

	return user, nil
}

func scanUserSummary(row rowScanner) (models.UserSummary, error) {
	var user models.UserSummary
	err := row.Scan(
		&user.ID,
		&user.Username,
		&user.Nickname,
		&user.StatusMessage,
		&user.StatusUpdatedAt,
		&user.CreatedAt,
	)
	if err != nil {
		return models.UserSummary{}, err
	}

	return user, nil
}

// CreateUser cria um novo usuário com settings vazias e retorna o registro criado.
func CreateUser(ctx context.Context, username, passwordHash, ip string) (models.User, models.UserSettings, error) {
	tx, err := GetDB().BeginTx(ctx, nil)
	if err != nil {
		return models.User{}, models.UserSettings{}, fmt.Errorf("falha ao criar usuário: %w", err)
	}
	defer tx.Rollback()

	user, err := scanUserPublic(tx.QueryRowContext(ctx, "INSERT INTO users (username, password_hash, last_ip) VALUES ($1, $2, $3) RETURNING "+userPublicColumns,
		username, passwordHash, ip))
	if err != nil {
		return models.User{}, models.UserSettings{}, mapStorageError(err)
	}

	emptyUserSettings, errJson := json.Marshal(models.UserSettings{})

	if errJson != nil {
		return models.User{}, models.UserSettings{}, mapStorageError(err)
	}

	userSettings, err := scanUserSettings(tx.QueryRowContext(ctx,
		`INSERT INTO user_settings (user_id, config, version) VALUES ($1, $2, $3) RETURNING `+userSettingsColumns,
		user.ID, string(emptyUserSettings), models.CurrentVersion,
	))
	if err != nil {
		return models.User{}, models.UserSettings{}, mapStorageError(err)
	}

	if err := tx.Commit(); err != nil {
		return models.User{}, models.UserSettings{}, fmt.Errorf("falha ao criar usuário: %w", err)
	}

	return user, userSettings, nil
}

// GetUserByID busca um usuário pelo id, sem o password_hash.
func GetUserByID(ctx context.Context, id string) (models.User, error) {
	row := GetDB().QueryRowContext(ctx,
		"SELECT "+userPublicColumns+" FROM users WHERE id = $1",
		id,
	)

	user, err := scanUserPublic(row)
	if err != nil {
		return models.User{}, mapStorageError(err)
	}

	return user, nil
}

// GetUserByUsername busca um usuário pelo username, incluindo o password_hash.
// É a única função que retorna o hash do banco e deve ser usada somente
// internamente para validação de senha.
func GetUserByUsername(ctx context.Context, username string) (models.User, error) {
	row := GetDB().QueryRowContext(ctx,
		"SELECT "+userColumns+" FROM users WHERE username = $1",
		username,
	)

	user, err := scanUser(row)
	if err != nil {
		return models.User{}, mapStorageError(err)
	}

	return user, nil
}

// ListUsers lista os usuários ordenados por data de criação, sem campos
// sensíveis ou densos.
// Se since for fornecido, retorna apenas usuários criados após esse
// timestamp; se lastID for fornecido junto, o cursor é o par (created_at, id)
// e o filtro inclui usuários criados no mesmo timestamp com id maior que
// lastID (evita pular usuários com timestamp igual).
// Se limit for > 0, retorna no máximo limit usuários.
func ListUsers(ctx context.Context, since *time.Time, lastID string, limit int) ([]models.UserSummary, error) {
	query := "SELECT " + userSummaryColumns + " FROM users"
	args := []any{}
	if since != nil {
		if lastID != "" {
			query += " WHERE (created_at > $" + strconv.Itoa(len(args)+1) +
				" OR (created_at = $" + strconv.Itoa(len(args)+1) +
				" AND id > $" + strconv.Itoa(len(args)+2) + "))"
			args = append(args, *since, lastID)
		} else {
			query += " WHERE created_at > $" + strconv.Itoa(len(args)+1)
			args = append(args, *since)
		}
	}
	query += " ORDER BY created_at, id"
	if limit > 0 {
		query += " LIMIT $" + strconv.Itoa(len(args)+1)
		args = append(args, limit)
	}

	rows, err := GetDB().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("falha ao listar usuários: %w", err)
	}
	defer rows.Close()

	users := make([]models.UserSummary, 0)
	for rows.Next() {
		user, err := scanUserSummary(rows)
		if err != nil {
			return nil, fmt.Errorf("falha ao ler usuário: %w", err)
		}
		users = append(users, user)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("falha ao listar usuários: %w", err)
	}

	return users, nil
}

// UpdateUser atualiza os campos de perfil do usuário (nickname e status)
// e retorna o registro atualizado, sem o password_hash.
func UpdateUser(ctx context.Context, id string, user models.User) (models.User, error) {
	row := GetDB().QueryRowContext(ctx,
		`UPDATE users
		 SET nickname = $2, status_message = $3, status_updated_at = $4
		 WHERE id = $1
		 RETURNING `+userPublicColumns,
		id, user.Nickname, user.StatusMessage, user.StatusUpdatedAt,
	)

	updated, err := scanUserPublic(row)
	if err != nil {
		return models.User{}, mapStorageError(err)
	}

	return updated, nil
}

// UpdateUserAvatar atualiza SOMENTE o avatar do usuário
func UpdateUserAvatar(ctx context.Context, avatarBlob []byte, avatarFormat, id string) error {
	result, err := GetDB().ExecContext(ctx,
		"UPDATE users SET avatar_blob = $2, avatar_format = $3 WHERE id = $1",
		id, avatarBlob, avatarFormat,
	)
	if err != nil {
		return fmt.Errorf("falha ao atualizar avatar do usuário: %w", err)
	}

	if n, _ := result.RowsAffected(); n == 0 {
		return ErrNotFound
	}

	return nil
}

// UpdateUserLastIP atualiza o último IP de conexão do usuário.
func UpdateUserLastIP(ctx context.Context, id, ip string) error {
	result, err := GetDB().ExecContext(ctx,
		"UPDATE users SET last_ip = $2 WHERE id = $1",
		id, ip,
	)
	if err != nil {
		return fmt.Errorf("falha ao atualizar last_ip do usuário: %w", err)
	}

	if n, _ := result.RowsAffected(); n == 0 {
		return ErrNotFound
	}

	return nil
}

// HasBannedUserByIP indica se existe algum usuário banido cujo último IP de conexão é o informado.
func HasBannedUserByIP(ctx context.Context, ip string) (bool, error) {
	var exists bool
	err := GetDB().QueryRowContext(ctx,
		"SELECT EXISTS (SELECT 1 FROM users WHERE banned = TRUE AND last_ip = $1)",
		ip,
	).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("falha ao verificar usuários banidos por IP: %w", err)
	}

	return exists, nil
}

// Função usada internamente apenas para NÃO banir dono do servidor
func isServerOwner(ctx context.Context, id string) (bool, error) {
	var isOwner bool

	err := GetDB().QueryRowContext(ctx,
		`SELECT EXISTS (
			SELECT 1
			FROM servers
			WHERE owner_id = $1
		)`,
		id,
	).Scan(&isOwner)

	if err != nil {
		return false, fmt.Errorf("falha ao ler estado de dono: %w", err)
	}

	return isOwner, nil
}

// SetUserBanned define o estado de banimento do usuário.
// O dono do servidor NÃO É BANÍVEL, a variável isOwner serve para assegurar isso
func SetUserBanned(ctx context.Context, id string, banned bool) (bool, error) {
	isOwner, err := isServerOwner(ctx, id)

	if err != nil {
		return false, err
	}

	if isOwner {
		return true, nil
	}

	result, err := GetDB().ExecContext(ctx,
		"UPDATE users SET banned = $2 WHERE id = $1",
		id, banned,
	)
	if err != nil {
		return false, fmt.Errorf("falha ao atualizar banimento do usuário: %w", err)
	}

	if n, _ := result.RowsAffected(); n == 0 {
		return false, ErrNotFound
	}

	return false, nil
}

// UpdateUserPassword substitui o hash de senha do usuário e reinicia a flag
// de reset de senha (users.reset_password = FALSE).
// Retorna ErrNotFound quando o usuário não existe.
func UpdateUserPassword(ctx context.Context, id, passwordHash string) error {
	result, err := GetDB().ExecContext(ctx,
		"UPDATE users SET password_hash = $2, reset_password = FALSE WHERE id = $1",
		id, passwordHash,
	)
	if err != nil {
		return fmt.Errorf("falha ao atualizar a senha do usuário: %w", err)
	}

	if n, _ := result.RowsAffected(); n == 0 {
		return ErrNotFound
	}

	return nil
}

// SetUserResetPassword marca o usuário para redefinir a senha
// (users.reset_password = TRUE). Retorna ErrNotFound quando o usuário não existe.
func SetUserResetPassword(ctx context.Context, id string) error {
	result, err := GetDB().ExecContext(ctx,
		"UPDATE users SET reset_password = TRUE WHERE id = $1",
		id,
	)
	if err != nil {
		return fmt.Errorf("falha ao marcar o usuário para reset de senha: %w", err)
	}

	if n, _ := result.RowsAffected(); n == 0 {
		return ErrNotFound
	}

	return nil
}
