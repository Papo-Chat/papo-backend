package handlers

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"papo/internal/models"

	"github.com/labstack/echo/v4"
)

// manageServerToken cria um usuário e lhe atribui uma role com manage_server,
// retornando o id e o token de autenticação.
func manageServerToken(t *testing.T, e *echo.Echo) (string, string) {
	t.Helper()
	ownerID, _ := registerAndLogin(t, e)
	actorID, actorToken := registerAndLogin(t, e)
	createServerFor(t, ownerID)
	role := createRoleFor(t, models.RolePermissions{ManageServer: true})
	assignRoleToUser(t, actorID, role.ID)
	return actorID, actorToken
}

// TestListAuditLogsRouteUnauthenticated garante que GET /admin/audit-logs exige
// autenticação.
func TestListAuditLogsRouteUnauthenticated(t *testing.T) {
	e := newApp()

	rec := do(t, e, http.MethodGet, "/admin/audit-logs", nil, nil)

	assertProblem(t, rec, http.StatusUnauthorized, "unauthorized",
		"Token inválido ou expirado", "token de autenticação ausente, inválido ou expirado")
}

// TestListAuditLogsRouteForbiddenWithoutManageServer garante que um usuário
// autenticado sem a permissão manage_server é negado com 403.
func TestListAuditLogsRouteForbiddenWithoutManageServer(t *testing.T) {
	e := newApp()
	_, token := registerAndLogin(t, e)

	rec := do(t, e, http.MethodGet, "/admin/audit-logs", nil, authCookie(token))

	assertProblem(t, rec, http.StatusForbidden, "forbidden", "Acesso negado",
		"usuário não possui a permissão necessária para esta operação")
}

// TestListAuditLogsRouteInvalidSince garante que um timestamp inválido em since
// produz 400 antes de qualquer consulta.
func TestListAuditLogsRouteInvalidSince(t *testing.T) {
	e := newApp()
	_, token := manageServerToken(t, e)

	rec := do(t, e, http.MethodGet, "/admin/audit-logs?since=nao-e-data", nil, authCookie(token))

	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido",
		"since deve ser um timestamp ISO 8601")
}

// TestListAuditLogsRouteReturnsLogs garante que um usuário com manage_server
// recebe os logs de auditoria no formato { logs, has_more }, sem expor campos
// sensíveis (actor_id, entity_id, ip_address, user_agent).
func TestListAuditLogsRouteReturnsLogs(t *testing.T) {
	e := newApp()
	_, actorToken := manageServerToken(t, e)
	targetID, _ := registerAndLogin(t, e)
	cleanupBan(t, targetID)

	// gera um log de auditoria (user.ban) via operação real
	if rec := do(t, e, http.MethodPut, "/users/"+targetID+"/ban", newBanBody(targetID, true), authCookie(actorToken)); rec.Code != http.StatusOK {
		t.Fatalf("ban: esperava status 200, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}

	rec := do(t, e, http.MethodGet, "/admin/audit-logs?action=user.ban", nil, authCookie(actorToken))
	if rec.Code != http.StatusOK {
		t.Fatalf("esperava status 200, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}

	body := rec.Body.String()
	for _, sensitive := range []string{"actor_id", "entity_id", "ip_address", "user_agent"} {
		if strings.Contains(body, sensitive) {
			t.Errorf("resposta expõe campo sensível %q: %s", sensitive, body)
		}
	}

	var resp models.AuditLogList
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("falha ao decodificar resposta: %v", err)
	}
	if len(resp.Logs) == 0 {
		t.Fatal("esperava ao menos 1 log, obtive 0")
	}
	found := false
	for _, log := range resp.Logs {
		if log.Action == "user.ban" && log.ActorUsername != "" &&
			log.TargetUserID != nil && *log.TargetUserID == targetID {
			found = true
		}
	}
	if !found {
		t.Errorf("não encontrei o log user.ban do alvo %s na resposta: %+v", targetID, resp.Logs)
	}
}
