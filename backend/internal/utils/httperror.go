package utils

import (
	"encoding/json"
	"strings"

	"github.com/labstack/echo/v4"
)

// mimeProblemJSON é o content type do formato RFC 7807.
const mimeProblemJSON = "application/problem+json"

// ProblemDetails é o formato de erro da API (RFC 7807).
type ProblemDetails struct {
	Type     string `json:"type"`
	Title    string `json:"title"`
	Status   int    `json:"status"`
	Detail   string `json:"detail"`
	Instance string `json:"instance"`
}

// ProblemTypeURL gera o URI do campo "type" a partir da base configurada e do slug do erro.
// Ex.: base "https://papo.com/" + slug "invalid-param" -> "https://papo.com/errors/invalid-param".
func ProblemTypeURL(baseURL, slug string) string {
	return strings.TrimSuffix(baseURL, "/") + "/errors/" + slug
}

// SendProblem responde com um erro no formato RFC 7807 (application/problem+json).
// O campo "type" é montado a partir de baseURL e do slug do erro.
func SendProblem(c echo.Context, baseURL string, status int, slug, title, detail string) error {
	problem := ProblemDetails{
		Type:     ProblemTypeURL(baseURL, slug),
		Title:    title,
		Status:   status,
		Detail:   detail,
		Instance: c.Request().URL.Path,
	}

	c.Response().Header().Set(echo.HeaderContentType, mimeProblemJSON)
	c.Response().WriteHeader(status)
	return json.NewEncoder(c.Response()).Encode(problem)
}
