package moderation

import (
	"papo/internal/config"
	"papo/internal/utils"
)

// Modos de política (MODERATION_NUDITY_MODE / MODERATION_GORE_MODE):
//   - off:   a categoria não gera ação (score ignorado);
//   - flag:  sensitive (o cliente exibe aviso);
//   - blur:  sensitive (o cliente exibe a imagem com blur);
//   - block: blocked (a mensagem inteira é excluída e o fato fica no log de
//     auditoria).
//
// flag e blur são o MESMO estado no backend (sensitive): a diferença está na
// renderização do cliente.
const (
	ModeOff   = "off"
	ModeFlag  = "flag"
	ModeBlur  = "blur"
	ModeBlock = "block"
)

// Policy mapeia as probabilidades do classificador para o estado final do
// attachment.
type Policy struct {
	NudityMode      string
	GoreMode        string
	NudityThreshold float64
	GoreThreshold   float64
}

// NewPolicy monta a política a partir da configuração, normalizando modos
// inválidos para "off".
func NewPolicy(cfg *config.Config) Policy {
	return Policy{
		NudityMode:      normalizeMode(cfg.ModerationNudityMode),
		GoreMode:        normalizeMode(cfg.ModerationGoreMode),
		NudityThreshold: cfg.ModerationNudityThreshold,
		GoreThreshold:   cfg.ModerationGoreThreshold,
	}
}

func normalizeMode(mode string) string {
	switch mode {
	case ModeOff, ModeFlag, ModeBlur, ModeBlock:
		return mode
	default:
		utils.Infof("moderação: modo %q inválido, usando %q", mode, ModeOff)
		return ModeOff
	}
}

// Evaluate aplica a política ao resultado da inferência. Retorna o estado
// final e o motivo da decisão ("nsfw"/"nsfl"/""), usado no log de auditoria.
// A categoria com modo "block" tem prioridade sobre "flag"/"blur".
func (p Policy) Evaluate(r Result) (Status, string) {
	if p.NudityMode == ModeBlock && r.Nudity >= p.NudityThreshold {
		return StatusBlocked, "nsfw"
	}
	if p.GoreMode == ModeBlock && r.Gore >= p.GoreThreshold {
		return StatusBlocked, "nsfl"
	}
	if p.NudityMode != ModeOff && r.Nudity >= p.NudityThreshold {
		return StatusSensitive, "nsfw"
	}
	if p.GoreMode != ModeOff && r.Gore >= p.GoreThreshold {
		return StatusSensitive, "nsfl"
	}
	return StatusClean, ""
}
