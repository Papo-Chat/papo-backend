package main

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math/rand"
	"strings"
)

var words = []string{
	"chat", "papo", "reunião", "amanhã", "projeto", "deploy", "bug", "teste",
	"cliente", "prazo", "revisão", "feedback", "equipe", "sprint", "release",
	"documentação", "recurso", "tarefa", "prioridade", "alinhamento", "status",
	"atualização", "melhoria", "correção", "integração", "ambiente", "homologação",
	"produção", "monitoramento", "alerta", "incidente", "sla", "uptime",
	"latência", "throughput", "banco", "consulta", "índice", "transação",
	"autenticação", "permissão", "papéis", "canal", "mensagem", "anexo",
	"imagem", "vídeo", "áudio", "link", "referência", "contexto", "obs",
	"nota", "lembrete", "segunda", "terça", "quarta", "quinta", "sexta",
	"madrugada", "tarde", "manhã", "fim", "semana", "mês", "trimestre",
	"orçamento", "meta", "resultado", "crescimento", "evolução", "plano",
	"ideia", "opinião", "sugestão", "acordo", "decisão", "próximo",
	"conseguiu", "cheguei", "vi", "li", "enviei", "confirma", "obrigado",
	"valeu", "show", "perfeito", "legal", "bom", "ótimo", "excelente",
	"alpha", "beta", "gamma", "delta", "omega", "kappa", "sigma", "zeta",
}

var unicodeEmojis = []string{
	"👍", "❤️", "😂", "🔥", "🎉", "😮", "😢", "👏", "💯", "🙏",
	"😎", "🤔", "✅", "🚀", "⭐", "🍕", "☕", "🐛", "📌", "🎯",
}

// DataGen gera dados falsos. Usa o rand global (seguro para concorrência,
// Go 1.20+) porque os workers de seeding chamam estes métodos em paralelo.
type DataGen struct{}

func NewDataGen() *DataGen {
	return &DataGen{}
}

func (d *DataGen) Words(n int) string {
	if n <= 0 {
		return ""
	}
	b := make([]string, 0, n)
	for i := 0; i < n; i++ {
		b = append(b, words[rand.Intn(len(words))])
	}
	return strings.Join(b, " ")
}

func (d *DataGen) Content() string {
	return d.Words(3 + rand.Intn(38))
}

func (d *DataGen) Username(i int) string { return fmt.Sprintf("st%06d", i) }

func (d *DataGen) Password(i int) string {
	return fmt.Sprintf("Stress!%06d", i)
}

func (d *DataGen) Nickname(i int) string {
	return fmt.Sprintf("Nick %06d", i)
}

// PNG gera um PNG pequeno e válido (cor sólida com ruído).
func (d *DataGen) PNG(w, h int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	base := color.RGBA{
		uint8(rand.Intn(256)), uint8(rand.Intn(256)), uint8(rand.Intn(256)), 255,
	}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			n := rand.Intn(41) - 20
			img.SetRGBA(x, y, color.RGBA{
				clampByte(int(base.R) + n),
				clampByte(int(base.G) + n),
				clampByte(int(base.B) + n),
				255,
			})
		}
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}

func clampByte(v int) uint8 {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return uint8(v)
}

// TextFile gera um arquivo texto de aproximadamente sizeKB kilobytes.
func (d *DataGen) TextFile(sizeKB int) (string, []byte) {
	target := sizeKB * 1024
	var sb strings.Builder
	for sb.Len() < target {
		sb.WriteString(d.Words(20))
		sb.WriteByte('\n')
	}
	name := fmt.Sprintf("doc_%06d.txt", rand.Intn(1<<24))
	return name, []byte(sb.String())
}

func (d *DataGen) Settings() map[string]any {
	return map[string]any{
		"theme": []string{"dark", "light", "system"}[rand.Intn(3)],
		"notifications": map[string]any{
			"enabled":        rand.Intn(2) == 1,
			"messagePreview": rand.Intn(2) == 1,
			"sound":          rand.Intn(2) == 1,
			"mentions":       true,
		},
		"display": map[string]any{
			"fontSize":       []string{"small", "medium", "huge"}[rand.Intn(3)],
			"messageDensity": []string{"compact", "normal", "comfortable"}[rand.Intn(3)],
			"showTimestamps": rand.Intn(2) == 1,
			"showAvatars":    rand.Intn(2) == 1,
		},
	}
}
