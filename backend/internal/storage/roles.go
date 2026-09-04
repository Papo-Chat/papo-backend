package storage

import (
	"context"
	"encoding/json"
	"fmt"

	"papo/internal/models"
)

const roleColumns = "id, name, color, permissions, created_at"

const userRoleColumns = "user_id, role_id, assigned_at"

func scanRole(row rowScanner) (models.Role, error) {
	var role models.Role
	var permissions []byte
	err := row.Scan(
		&role.ID,
		&role.Name,
		&role.Color,
		&permissions,
		&role.CreatedAt,
	)
	if err != nil {
		return models.Role{}, err
	}

	role.Permissions = models.RolePermissions{}
	if len(permissions) > 0 {
		if err := json.Unmarshal(permissions, &role.Permissions); err != nil {
			return models.Role{}, fmt.Errorf("falha ao decodificar permissões da role: %w", err)
		}
	}

	return role, nil
}

func scanUserRole(row rowScanner) (models.UserRole, error) {
	var userRole models.UserRole
	err := row.Scan(
		&userRole.UserID,
		&userRole.RoleID,
		&userRole.AssignedAt,
	)
	if err != nil {
		return models.UserRole{}, err
	}

	return userRole, nil
}

// CreateRole cria uma nova role e retorna o registro criado.
func CreateRole(ctx context.Context, name string, color *string, permissions models.RolePermissions) (models.Role, error) {
	permissionJSON, err := json.Marshal(permissions)
	if err != nil {
		return models.Role{}, fmt.Errorf("falha ao codificar permissões da role: %w", err)
	}

	row := GetDB().QueryRowContext(ctx,
		"INSERT INTO roles (name, color, permissions) VALUES ($1, $2, $3) RETURNING "+roleColumns,
		name, color, string(permissionJSON),
	)

	role, err := scanRole(row)
	if err != nil {
		return models.Role{}, mapStorageError(err)
	}

	return role, nil
}

// GetRoleByID busca uma role pelo id.
func GetRoleByID(ctx context.Context, id string) (models.Role, error) {
	row := GetDB().QueryRowContext(ctx,
		"SELECT "+roleColumns+" FROM roles WHERE id = $1",
		id,
	)

	role, err := scanRole(row)
	if err != nil {
		return models.Role{}, mapStorageError(err)
	}

	return role, nil
}

// ListRoles lista as roles ordenadas por data de criação.
func ListRoles(ctx context.Context) ([]models.Role, error) {
	rows, err := GetDB().QueryContext(ctx,
		"SELECT "+roleColumns+" FROM roles ORDER BY created_at, id",
	)
	if err != nil {
		return nil, fmt.Errorf("falha ao listar roles: %w", err)
	}
	defer rows.Close()

	roles := make([]models.Role, 0)
	for rows.Next() {
		role, err := scanRole(rows)
		if err != nil {
			return nil, fmt.Errorf("falha ao ler role: %w", err)
		}
		roles = append(roles, role)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("falha ao listar roles: %w", err)
	}

	return roles, nil
}

// UpdateRole atualiza o nome, a cor e as permissões da role
// e retorna o registro atualizado.
func UpdateRole(ctx context.Context, id string, role models.Role) (models.Role, error) {
	permissionJSON, err := json.Marshal(role.Permissions)
	if err != nil {
		return models.Role{}, fmt.Errorf("falha ao codificar permissões da role: %w", err)
	}

	row := GetDB().QueryRowContext(ctx,
		`UPDATE roles
		 SET name = $2, color = $3, permissions = $4
		 WHERE id = $1
		 RETURNING `+roleColumns,
		id, role.Name, role.Color, string(permissionJSON),
	)

	updated, err := scanRole(row)
	if err != nil {
		return models.Role{}, mapStorageError(err)
	}

	return updated, nil
}

// DeleteRole deleta uma role por id e remove sua entrada das permissões
// de todos os canais, de maneira atômica
// na table user_roles isso já ocorre no cascade.
func DeleteRole(ctx context.Context, id string) error {
	tx, err := GetDB().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("falha ao excluir role: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx,
		"UPDATE channels SET permissions = permissions - $1 WHERE permissions ? $1",
		id,
	); err != nil {
		return fmt.Errorf("falha ao excluir role: %w", err)
	}

	result, err := tx.ExecContext(ctx,
		"DELETE FROM roles WHERE id = $1",
		id,
	)
	if err != nil {
		return fmt.Errorf("falha ao excluir role: %w", err)
	}

	if n, _ := result.RowsAffected(); n == 0 {
		return ErrNotFound
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("falha ao excluir role: %w", err)
	}

	return nil
}

// AssignUserRole atribui uma role a um usuário e retorna o registro criado.
// Atribuir a mesma role duas vezes retorna ErrUniqueViolation.
func AssignUserRole(ctx context.Context, userID, roleID string) (models.UserRole, error) {
	row := GetDB().QueryRowContext(ctx,
		"INSERT INTO user_roles (user_id, role_id) VALUES ($1, $2) RETURNING "+userRoleColumns,
		userID, roleID,
	)

	userRole, err := scanUserRole(row)
	if err != nil {
		return models.UserRole{}, mapStorageError(err)
	}

	return userRole, nil
}

// GetUserRole retorna a atribuição de uma role a um usuário.
func GetUserRole(ctx context.Context, userID, roleID string) (models.UserRole, error) {
	row := GetDB().QueryRowContext(ctx,
		"SELECT "+userRoleColumns+" FROM user_roles WHERE user_id = $1 AND role_id = $2",
		userID, roleID,
	)

	userRole, err := scanUserRole(row)
	if err != nil {
		return models.UserRole{}, mapStorageError(err)
	}

	return userRole, nil
}

// RemoveUserRole remove a atribuição de uma role de um usuário.
func RemoveUserRole(ctx context.Context, userID, roleID string) error {
	result, err := GetDB().ExecContext(ctx,
		"DELETE FROM user_roles WHERE user_id = $1 AND role_id = $2",
		userID, roleID,
	)
	if err != nil {
		return fmt.Errorf("falha ao remover role do usuário: %w", err)
	}

	if n, _ := result.RowsAffected(); n == 0 {
		return ErrNotFound
	}

	return nil
}

// GetRolesByUser retorna as roles atribuídas a um usuário.
func GetRolesByUser(ctx context.Context, userID string) ([]models.Role, error) {
	rows, err := GetDB().QueryContext(ctx,
		`SELECT r.`+roleColumns+`
		 FROM roles r
		 JOIN user_roles ur ON ur.role_id = r.id
		 WHERE ur.user_id = $1
		 ORDER BY r.created_at, r.id`,
		userID,
	)
	if err != nil {
		return nil, fmt.Errorf("falha ao listar roles do usuário: %w", err)
	}
	defer rows.Close()

	roles := make([]models.Role, 0)
	for rows.Next() {
		role, err := scanRole(rows)
		if err != nil {
			return nil, fmt.Errorf("falha ao ler role: %w", err)
		}
		roles = append(roles, role)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("falha ao listar roles do usuário: %w", err)
	}

	return roles, nil
}

// GetRoleSummariesByUser retorna as roles atribuídas a um usuário
// (id, nome e cor), na ordem de criação.
func GetRoleSummariesByUser(ctx context.Context, userID string) ([]models.RoleSummary, error) {
	rows, err := GetDB().QueryContext(ctx,
		`SELECT r.id, r.name, r.color
		 FROM roles r
		 JOIN user_roles ur ON ur.role_id = r.id
		 WHERE ur.user_id = $1
		 ORDER BY r.created_at, r.id`,
		userID,
	)
	if err != nil {
		return nil, fmt.Errorf("falha ao listar roles do usuário: %w", err)
	}
	defer rows.Close()

	roles := make([]models.RoleSummary, 0)
	for rows.Next() {
		var role models.RoleSummary
		if err := rows.Scan(&role.ID, &role.Name, &role.Color); err != nil {
			return nil, fmt.Errorf("falha ao ler role: %w", err)
		}
		roles = append(roles, role)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("falha ao listar roles do usuário: %w", err)
	}

	return roles, nil
}

// GetRoleSummariesByUsers retorna as roles atribuídas a cada um dos usuários
// informados (id, nome e cor), agrupadas por user_id. Todos os ids informados
// aparecem no mapa (usuários sem roles recebem fatia vazia).
func GetRoleSummariesByUsers(ctx context.Context, userIDs []string) (map[string][]models.RoleSummary, error) {
	rolesByUser := make(map[string][]models.RoleSummary, len(userIDs))
	for _, id := range userIDs {
		rolesByUser[id] = make([]models.RoleSummary, 0)
	}
	if len(userIDs) == 0 {
		return rolesByUser, nil
	}

	rows, err := GetDB().QueryContext(ctx,
		`SELECT ur.user_id, r.id, r.name, r.color
		 FROM user_roles ur
		 JOIN roles r ON r.id = ur.role_id
		 WHERE ur.user_id = ANY($1)
		 ORDER BY ur.user_id, r.created_at, r.id`,
		userIDs,
	)
	if err != nil {
		return nil, fmt.Errorf("falha ao listar roles dos usuários: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var userID string
		var role models.RoleSummary
		if err := rows.Scan(&userID, &role.ID, &role.Name, &role.Color); err != nil {
			return nil, fmt.Errorf("falha ao ler role: %w", err)
		}
		rolesByUser[userID] = append(rolesByUser[userID], role)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("falha ao listar roles dos usuários: %w", err)
	}

	return rolesByUser, nil
}

// GetUsersByRoles retorna os ids distintos dos usuários atribuídos a ao
// menos uma das roles informadas.
func GetUsersByRoles(ctx context.Context, roleIDs []string) ([]string, error) {
	if len(roleIDs) == 0 {
		return []string{}, nil
	}

	rows, err := GetDB().QueryContext(ctx,
		"SELECT DISTINCT user_id FROM user_roles WHERE role_id = ANY($1) ORDER BY user_id",
		roleIDs,
	)
	if err != nil {
		return nil, fmt.Errorf("falha ao listar usuários das roles: %w", err)
	}
	defer rows.Close()

	userIDs := make([]string, 0)
	for rows.Next() {
		var userID string
		if err := rows.Scan(&userID); err != nil {
			return nil, fmt.Errorf("falha ao ler usuário: %w", err)
		}
		userIDs = append(userIDs, userID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("falha ao listar usuários das roles: %w", err)
	}

	return userIDs, nil
}

// GetUsersByRole retorna os ids dos usuários atribuídos a uma role.
func GetUsersByRole(ctx context.Context, roleID string) ([]string, error) {
	rows, err := GetDB().QueryContext(ctx,
		"SELECT user_id FROM user_roles WHERE role_id = $1 ORDER BY user_id",
		roleID,
	)
	if err != nil {
		return nil, fmt.Errorf("falha ao listar usuários da role: %w", err)
	}
	defer rows.Close()

	userIDs := make([]string, 0)
	for rows.Next() {
		var userID string
		if err := rows.Scan(&userID); err != nil {
			return nil, fmt.Errorf("falha ao ler usuário da role: %w", err)
		}
		userIDs = append(userIDs, userID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("falha ao listar usuários da role: %w", err)
	}

	return userIDs, nil
}
