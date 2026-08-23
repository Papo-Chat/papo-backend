package services

import (
	"context"
	"strings"
	"time"

	"papo/internal/models"
	"papo/internal/storage"
)

// searchResultLimit é o número máximo de resultados por página de busca
// (100 por requisição).
const searchResultLimit = 100

// searchDateLayout é o formato das datas de filtro (YYYY-MM-DD, openapi).
const searchDateLayout = "2006-01-02"

// SearchMessages executa a busca de mensagens (POST /search) com full-text
// search em português (mesmo config 'portuguese' do tsvector) e filtros
// combináveis: texto, autor, intervalo de datas (inclusive) e attachment.
// Pelo menos 1 filtro é obrigatório.
//
// serverID é opcional: quando informado, filtra os resultados por servidor.
//
// A autorização é a mesma da leitura de mensagens: os resultados são
// limitados a canais legíveis pelo usuário (canais de servidores dos quais
// ele é dono, canais abertos sem permissões definidas ou canais em que uma
// das suas roles do servidor tem read_channel).
//
// A paginação usa o cursor (since, last_id) = (created_at, id), 100
// resultados por página; has_more indica se existe próxima página. A
// ordenação é por created_at (order asc/desc, padrão desc).
//
// Retorna ErrInvalidInput quando o usuário está ausente, quando não há
// nenhum filtro, quando order não é asc/desc, quando date_start ou date_end
// estão em formato inválido, quando date_start é depois de date_end ou
// quando since/last_id são fornecidos separados.
func SearchMessages(ctx context.Context, req models.SearchRequest, serverID string, since *time.Time, lastID string, userID string) (models.SearchResponse, error) {
	if userID == "" {
		return models.SearchResponse{}, ErrInvalidInput
	}

	text := strings.TrimSpace(req.Text)
	if text == "" && req.Author == "" && req.DateStart == "" && req.DateEnd == "" && req.ContainsAttachment == nil {
		return models.SearchResponse{}, ErrInvalidInput
	}

	orderAsc := false
	switch req.Order {
	case "":
		// padrão desc
	case "desc":
	case "asc":
		orderAsc = true
	default:
		return models.SearchResponse{}, ErrInvalidInput
	}

	var dateStart, dateEndExclusive *time.Time
	if req.DateStart != "" {
		d, err := time.Parse(searchDateLayout, req.DateStart)
		if err != nil {
			return models.SearchResponse{}, ErrInvalidInput
		}
		dateStart = &d
	}
	if req.DateEnd != "" {
		d, err := time.Parse(searchDateLayout, req.DateEnd)
		if err != nil {
			return models.SearchResponse{}, ErrInvalidInput
		}
		// date_end é inclusivo: o limite vira o início do dia seguinte
		end := d.AddDate(0, 0, 1)
		dateEndExclusive = &end
	}
	if dateStart != nil && dateEndExclusive != nil && !dateStart.Before(*dateEndExclusive) {
		return models.SearchResponse{}, ErrInvalidInput
	}

	if (since == nil) != (lastID == "") {
		return models.SearchResponse{}, ErrInvalidInput
	}

	params := storage.SearchParams{
		UserID:             userID,
		Text:               text,
		AuthorID:           req.Author,
		ServerID:           serverID,
		DateStart:          dateStart,
		DateEndExclusive:   dateEndExclusive,
		ContainsAttachment: req.ContainsAttachment,
		Since:              since,
		LastID:             lastID,
		OrderAsc:           orderAsc,
		Limit:              searchResultLimit,
	}

	results, err := storage.SearchMessages(ctx, params)
	if err != nil {
		return models.SearchResponse{}, err
	}

	hasMore := len(results) > searchResultLimit
	if hasMore {
		results = results[:searchResultLimit]
	}

	return models.SearchResponse{Results: results, HasMore: hasMore}, nil
}
