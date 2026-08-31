package models

import "time"

// SearchRequest é o corpo de POST /search. Pelo menos 1 filtro é
// obrigatório (text, author, date_start, date_end ou contains_attachment);
// os filtros podem ser combinados.
type SearchRequest struct {
	Text               string `json:"text"`
	Author             string `json:"author"`
	Order              string `json:"order"`
	DateStart          string `json:"date_start"`
	DateEnd            string `json:"date_end"`
	ContainsAttachment *bool  `json:"contains_attachment"`
}

// SearchResult é um resultado da busca (POST /search). Type é sempre
// "message". Score é preenchido apenas quando a busca tem termo de texto.
type SearchResult struct {
	Type           string    `json:"type"`
	ID             string    `json:"id"`
	Content        *string   `json:"content"`
	ChannelID      string    `json:"channel_id"`
	ChannelName    string    `json:"channel_name"`
	AuthorID       *string   `json:"author_id"`
	AuthorUsername *string   `json:"author_username"`
	CreatedAt      time.Time `json:"created_at"`
	Score          *float64  `json:"score,omitempty"`
}

// SearchResponse é a resposta de POST /search (paginada: até 100 resultados
// por página; has_more indica se existe próxima página).
type SearchResponse struct {
	Results []SearchResult `json:"results"`
	HasMore bool           `json:"has_more"`
}
