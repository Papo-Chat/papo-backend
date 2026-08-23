package utils

//Esse código contem implementação base para biblioteca CSAM no envio de mensagens
//CSAM é um sistema de filtro por hash para arquivos sensíveis, porém as APIS são privadas
//Normalmente se precisa da autorização de uma entidade ou governo para ter acesso a essa API
//Se o administrador do servidor quiser implementar CSAM será necessária a implementação
//Da conexão com a API escolhida (existem várias, cada uma do seu jeito)

//Particularmente recomendamos usar o Cloudflare, pois ele já tem uma opção que
//integra filtro CSAM na aplicação.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"time"
)

// Scanner é o contrato que qualquer provedor precisa satisfazer.
// O admin self-host escolhe a implementação via config, não via
// endpoint genérico — porque o "como perguntar" muda por provedor.
type Scanner interface {
	CheckImageCSAM(ctx context.Context, imageData []byte) (ScanResult, error)
}

type ScanResult struct {
	Matched    bool
	Confidence float64 // nem todo provedor retorna isso; 0 se não aplicável
}

// --- Implementação de exemplo: provedor genérico via API key simples ---
// Serve de referência pra qualquer serviço com contrato REST + API key,
// mas NÃO assuma que PhotoDNA/Thorn tenham exatamente esse shape de payload
// sem checar a documentação específica deles.

type genericAPIScanner struct {
	endpoint string
	apiKey   string
	client   *http.Client
}

func NewGenericAPIScanner(endpoint, apiKey string) Scanner {
	return &genericAPIScanner{
		endpoint: endpoint,
		apiKey:   apiKey,
		client:   &http.Client{Timeout: 10 * time.Second},
	}
}

func (s *genericAPIScanner) CheckImageCSAM(ctx context.Context, imageData []byte) (ScanResult, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", s.endpoint, bytes.NewReader(imageData))
	if err != nil {
		return ScanResult{}, err
	}
	req.Header.Set("Authorization", "Bearer "+s.apiKey) // varia: pode ser header próprio, mTLS, OAuth2, etc
	req.Header.Set("Content-Type", "application/octet-stream")

	resp, err := s.client.Do(req)
	if err != nil {
		return ScanResult{}, err
	}
	defer resp.Body.Close()

	var result struct {
		Matched    bool    `json:"matched"`
		Confidence float64 `json:"confidence"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return ScanResult{}, err
	}

	return ScanResult{Matched: result.Matched, Confidence: result.Confidence}, nil
}
