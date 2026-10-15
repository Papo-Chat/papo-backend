package services

import (
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"papo/internal/models"
	"papo/internal/storage"
)

// normalizeTestURL parseia uma URL de teste (fixture offline).
func normalizeTestURL(raw string) (*url.URL, error) {
	return url.Parse(raw)
}

/* ------------------------------------------------------ provedor / hosts */

func TestGifProviderHost(t *testing.T) {
	cases := []struct {
		host string
		want string
	}{
		{"tenor.com", gifProviderTenor},
		{"www.tenor.com", gifProviderTenor},
		{"m.tenor.com", gifProviderTenor},
		{"media.tenor.com", gifProviderTenor},
		{"TENOR.com", gifProviderTenor},
		{"giphy.com", gifProviderGiphy},
		{"www.giphy.com", gifProviderGiphy},
		{"i.giphy.com", gifProviderGiphy},
		{"media.giphy.com", gifProviderGiphy},
		{"giphy.media", gifProviderGiphy},
		{"klipy.com", gifProviderKlipy},
		{"www.klipy.com", gifProviderKlipy},
		{"static.klipy.com", gifProviderKlipy},
		// Hosts que imitam o sufixo NÃO podem casar.
		{"tenor.com.evil.com", ""},
		{"giphy.com.evil.com", ""},
		{"klipy.com.evil.com", ""},
		{"xtenor.com", ""},
		{"example.com", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := gifProviderHost(c.host); got != c.want {
			t.Errorf("gifProviderHost(%q) = %q, esperado %q", c.host, got, c.want)
		}
	}
}

func TestIsGiphyID(t *testing.T) {
	valid := []string{"okfvUCpgArv3y", "H0rs8l0T3rCs", "abc12345", "QBd2kLB5qDmysEXre9"}
	for _, s := range valid {
		if !isGiphyID(s) {
			t.Errorf("isGiphyID(%q) = false, esperado true", s)
		}
	}
	invalid := []string{"", "short1", "all-lowercase-id", "alllowercaseid", "1234567", "has space1", "QBd2kLB5qDmysEXre9QBd2kLB"}
	for _, s := range invalid {
		if isGiphyID(s) {
			t.Errorf("isGiphyID(%q) = true, esperado false", s)
		}
	}
}

/* ------------------------------------------------------------- parse URLs */

func TestParseTenorPageURL(t *testing.T) {
	cases := []struct {
		in       string
		wantID   string
		wantPage string
		wantErr  bool
	}{
		{
			in:       "https://tenor.com/pt-BR/view/cats-dancing-brazil-dance-keffa-gif-7058550686737920259",
			wantID:   "7058550686737920259",
			wantPage: "https://tenor.com/view/cats-dancing-brazil-dance-keffa-gif-7058550686737920259",
		},
		{
			in:       "https://www.tenor.com/view/x-gif-1234567890",
			wantID:   "1234567890",
			wantPage: "https://tenor.com/view/x-gif-1234567890",
		},
		{
			in:       "https://tenor.com/embed/7058550686737920259",
			wantID:   "7058550686737920259",
			wantPage: "https://tenor.com/embed/7058550686737920259",
		},
		{
			// Link curto: sem id derivável, o fetch segue o 301.
			in:       "https://tenor.com/izAoRjiVUpb.gif",
			wantID:   "",
			wantPage: "https://tenor.com/izAoRjiVUpb.gif",
		},
		{in: "https://tenor.com/", wantErr: true},
	}
	for _, c := range cases {
		u, err := normalizeTestURL(c.in)
		if err != nil {
			t.Fatalf("URL de teste inválida %q: %v", c.in, err)
		}
		id, pageURL, err := parseTenorPageURL(u)
		if c.wantErr {
			if err == nil {
				t.Errorf("parseTenorPageURL(%q) deveria retornar erro", c.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseTenorPageURL(%q) retornou erro: %v", c.in, err)
			continue
		}
		if id != c.wantID {
			t.Errorf("parseTenorPageURL(%q) id = %q, esperado %q", c.in, id, c.wantID)
		}
		if pageURL != c.wantPage {
			t.Errorf("parseTenorPageURL(%q) pageURL = %q, esperado %q", c.in, pageURL, c.wantPage)
		}
	}
}

func TestParseGiphyPageURL(t *testing.T) {
	cases := []struct {
		in       string
		wantID   string
		wantPage string
		wantErr  bool
	}{
		{
			in:       "https://giphy.com/gifs/funny-happy-excited-okfvUCpgArv3y",
			wantID:   "okfvUCpgArv3y",
			wantPage: "https://giphy.com/gifs/funny-happy-excited-okfvUCpgArv3y",
		},
		{
			in:       "https://giphy.com/embed/okfvUCpgArv3y",
			wantID:   "okfvUCpgArv3y",
			wantPage: "https://giphy.com/embed/okfvUCpgArv3y",
		},
		{
			in:       "https://media.giphy.com/media/okfvUCpgArv3y/giphy.gif",
			wantID:   "okfvUCpgArv3y",
			wantPage: "https://giphy.com/gifs/okfvUCpgArv3y",
		},
		{
			in:       "https://media.giphy.com/media/v1.20250101/token/okfvUCpgArv3y/giphy.gif",
			wantID:   "okfvUCpgArv3y",
			wantPage: "https://giphy.com/gifs/okfvUCpgArv3y",
		},
		{
			// Slug sem id reconhecível: id vazio (resolvido pelo JSON-LD),
			// page URL preservado.
			in:       "https://giphy.com/gifs/some-all-lowercase-slug",
			wantID:   "",
			wantPage: "https://giphy.com/gifs/some-all-lowercase-slug",
		},
		{
			in:       "https://giphy.com/clips/okfvUCpgArv3y",
			wantID:   "okfvUCpgArv3y",
			wantPage: "https://giphy.com/clips/okfvUCpgArv3y",
		},
		{
			// Id atual do giphy (18 chars): com o teto antigo de 16 o tail do
			// slug era descartado e a resolução do link falha por completo.
			in:       "https://giphy.com/gifs/time-mr-bean-look-at-the-QBd2kLB5qDmysEXre9",
			wantID:   "QBd2kLB5qDmysEXre9",
			wantPage: "https://giphy.com/gifs/time-mr-bean-look-at-the-QBd2kLB5qDmysEXre9",
		},
		{
			in:       "https://media0.giphy.com/media/v1.Y2lkPTc5MGI3NjEx/token/QBd2kLB5qDmysEXre9/giphy.gif",
			wantID:   "QBd2kLB5qDmysEXre9",
			wantPage: "https://giphy.com/gifs/QBd2kLB5qDmysEXre9",
		},
		{in: "https://giphy.com/", wantErr: true},
	}
	for _, c := range cases {
		u, err := normalizeTestURL(c.in)
		if err != nil {
			t.Fatalf("URL de teste inválida %q: %v", c.in, err)
		}
		id, pageURL, err := parseGiphyPageURL(u)
		if c.wantErr {
			if err == nil {
				t.Errorf("parseGiphyPageURL(%q) deveria retornar erro", c.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseGiphyPageURL(%q) retornou erro: %v", c.in, err)
			continue
		}
		if id != c.wantID {
			t.Errorf("parseGiphyPageURL(%q) id = %q, esperado %q", c.in, id, c.wantID)
		}
		if pageURL != c.wantPage {
			t.Errorf("parseGiphyPageURL(%q) pageURL = %q, esperado %q", c.in, pageURL, c.wantPage)
		}
	}
}

func TestParseKlipyPageURL(t *testing.T) {
	cases := []struct {
		in          string
		wantSegment string
		wantSlug    string
		wantErr     bool
	}{
		{in: "https://klipy.com/gifs/orihime-bread-girl", wantSegment: "gifs", wantSlug: "orihime-bread-girl"},
		{in: "https://klipy.com/gifs/orihime-bread-girl/player", wantSegment: "gifs", wantSlug: "orihime-bread-girl"},
		{in: "https://klipy.com/clips/abc123", wantSegment: "clips", wantSlug: "abc123"},
		{in: "https://klipy.com/stickers/happy-sticker", wantSegment: "stickers", wantSlug: "happy-sticker"},
		{in: "https://klipy.com/users/foo", wantErr: true},
		{in: "https://klipy.com/gifs/", wantErr: true},
		{in: "https://klipy.com/gifs/bad*slug", wantErr: true},
	}
	for _, c := range cases {
		u, err := normalizeTestURL(c.in)
		if err != nil {
			t.Fatalf("URL de teste inválida %q: %v", c.in, err)
		}
		segment, slug, err := parseKlipyPageURL(u)
		if c.wantErr {
			if err == nil {
				t.Errorf("parseKlipyPageURL(%q) deveria retornar erro", c.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseKlipyPageURL(%q) retornou erro: %v", c.in, err)
			continue
		}
		if segment != c.wantSegment || slug != c.wantSlug {
			t.Errorf("parseKlipyPageURL(%q) = (%q, %q), esperado (%q, %q)",
				c.in, segment, slug, c.wantSegment, c.wantSlug)
		}
	}
}

/* ------------------------------------------------------------ tenor blob */

const tenorFixtureHTML = `<html><head><meta property="og:url" content="https://tenor.com/view/cats-dancing-gif-123456">` +
	`<meta property="og:title" content="Cats Dancing OG"></head><body>` +
	`<script nonce="abc123">{"appConfig":{"locale":"pt-BR"},"gifs":{"byId":{"123456":{"results":[` +
	`{"id":"123456","title":"Cats Dancing","h1_title":"Cats Dancing GIF","media_formats":{` +
	`"gif":{"url":"https://media.tenor.com/aaa/tenor.gif","dims":[498,498],"size":7000000},` +
	`"mediumgif":{"url":"https://media.tenor.com/bbb/medium.gif","dims":[220,220],"size":300000},` +
	`"tinygifpreview":{"url":"https://media.tenor.com/ccc/preview.png","dims":[120,120],"size":10000}` +
	`}}]}}}}</script></body></html>`

func TestTenorHydrationBlobAndItem(t *testing.T) {
	blob := tenorHydrationBlob([]byte(tenorFixtureHTML))
	if blob == nil {
		t.Fatalf("tenorHydrationBlob não localizou o blob de hidratação")
	}

	raw := tenorItemFromBlob(blob, "123456")
	if raw == nil {
		t.Fatalf("tenorItemFromBlob não resolveu o item pelo id")
	}
	if raw.H1Title != "Cats Dancing GIF" {
		t.Errorf("h1_title = %q, esperado %q", raw.H1Title, "Cats Dancing GIF")
	}

	renditions := tenorRenditionsFromFormats(raw.MediaFormats)
	if len(renditions) != 3 {
		t.Fatalf("esperava 3 renditions, obtive %d", len(renditions))
	}

	byKind := map[string]gifRendition{}
	for _, r := range renditions {
		byKind[r.Kind] = r
	}
	if got := byKind["gif"]; got.Tier != "hd" || got.Format != "gif" || got.Bytes != 7000000 {
		t.Errorf("rendition gif = %+v, esperado tier hd / gif / 7000000", byKind["gif"])
	}
	if got := byKind["mediumgif"]; got.Tier != "md" || got.Width != 220 {
		t.Errorf("rendition mediumgif = %+v, esperado tier md / width 220", byKind["mediumgif"])
	}
	if got := byKind["tinygifpreview"]; !got.Still {
		t.Errorf("rendition tinygifpreview deveria ser marcada como still, obtive %+v", byKind["tinygifpreview"])
	}

	// Sem o id (link curto pós-redirect): o único byId presente resolve o item.
	if raw := tenorItemFromBlob(blob, ""); raw == nil || raw.ID != "123456" {
		t.Errorf("tenorItemFromBlob sem id deveria resolver o único item do blob, obtive %+v", raw)
	}

	if meta := metaTagContent([]byte(tenorFixtureHTML), "og:url"); meta != "https://tenor.com/view/cats-dancing-gif-123456" {
		t.Errorf("metaTagContent(og:url) = %q", meta)
	}
}

/* ------------------------------------------------------------ giphy RSC */

func TestGiphyRSCBlobAndFindGifObject(t *testing.T) {
	inner := `prefix {"type":"gif","id":"okfvUCpgArv3y","slug":"funny-happy-excited-okfvUCpgArv3y",` +
		`"title":"Happy Skeleton GIF","url":"https://giphy.com/gifs/funny-happy-excited-okfvUCpgArv3y",` +
		`"images":{"fixed_height":{"height":200,"width":220,"size":"288760",` +
		`"url":"https://media.giphy.com/media/okfvUCpgArv3y/giphy.gif"},` +
		`"fixed_height_still":{"height":200,"width":220,"url":"https://media.giphy.com/still.png"}}} suffix`
	escaped := strings.ReplaceAll(inner, `"`, `\"`)
	html := `<html><body><script>self.__next_f.push([1,"` + escaped + `"])</script></body></html>`

	blob := giphyRSCBlob([]byte(html))
	if !strings.Contains(blob, `"type":"gif"`) {
		t.Fatalf("giphyRSCBlob não desescapou o chunk RSC: %q", blob)
	}

	raw := findGifObject(blob, "okfvUCpgArv3y")
	if raw == nil {
		t.Fatalf("findGifObject não localizou o objeto do gif")
	}
	if raw.Title != "Happy Skeleton GIF" {
		t.Errorf("title = %q, esperado %q", raw.Title, "Happy Skeleton GIF")
	}

	renditions := giphyRenditionsFromImages(raw.Images)
	byKind := map[string]gifRendition{}
	for _, r := range renditions {
		byKind[r.Kind] = r
	}
	fixed := byKind["fixed_height"]
	if fixed.Format != "gif" || fixed.Tier != "md" || fixed.Bytes != 288760 {
		t.Errorf("rendition fixed_height = %+v, esperado gif / md / 288760 (size string)", fixed)
	}
	still := byKind["fixed_height_still"]
	if !still.Still {
		t.Errorf("rendition fixed_height_still deveria ser marcada como still, obtive %+v", still)
	}
}

func TestBalancedAt(t *testing.T) {
	cases := []struct {
		text  string
		start int
		want  string
	}{
		{`{"a":{"b":1}}tail`, 0, `{"a":{"b":1}}`},
		{`{"a":"}\""}rest`, 0, `{"a":"}\""}`},
		{`{"a":[1,{"b":2}]}`, 0, `{"a":[1,{"b":2}]}`},
		{`{"broken":`, 0, ``},
	}
	for _, c := range cases {
		if got := balancedAt(c.text, c.start); got != c.want {
			t.Errorf("balancedAt(%q, %d) = %q, esperado %q", c.text, c.start, got, c.want)
		}
	}
}

/* ------------------------------------------------------------- klipy API */

func TestKlipyRenditionsAndEnvelopeErrors(t *testing.T) {
	file := map[string]map[string]klipyRenditionMeta{
		"md": {"gif": {URL: "https://static.klipy.com/ii/abc/md.gif", Width: 640, Height: 640, Size: 988000}},
		"hd": {"gif": {URL: "https://static.klipy.com/ii/abc/hd.gif", Width: 960, Height: 960, Size: 2000000}},
		"xs": {"webp": {URL: "https://static.klipy.com/ii/abc/xs.webp", Width: 120, Height: 120, Size: 20000}},
	}
	renditions := klipyRenditions(file)
	if len(renditions) != 3 {
		t.Fatalf("esperava 3 renditions, obtive %d", len(renditions))
	}
	byKind := map[string]gifRendition{}
	for _, r := range renditions {
		byKind[r.Kind] = r
	}
	if got := byKind["md.gif"]; got.Tier != "md" || got.Format != "gif" || got.Bytes != 988000 {
		t.Errorf("rendition md.gif = %+v", byKind["md.gif"])
	}
	if got := byKind["xs.webp"]; got.Format != "webp" {
		t.Errorf("rendition xs.webp format = %q, esperado webp", got.Format)
	}

	if msg := klipyErrorMessage([]byte(`["não encontrado","outro erro"]`)); msg != "não encontrado, outro erro" {
		t.Errorf("klipyErrorMessage(array) = %q", msg)
	}
	if msg := klipyErrorMessage([]byte(`"falha única"`)); msg != "falha única" {
		t.Errorf("klipyErrorMessage(string) = %q", msg)
	}
	if msg := klipyErrorMessage(nil); msg == "" {
		t.Errorf("klipyErrorMessage(vazio) deveria retornar um texto")
	}
}

/* ------------------------------------------------- escolha de rendition */

func TestPickGIFRendition(t *testing.T) {
	item := gifItem{Renditions: []gifRendition{
		{URL: "https://cdn/x/hd.gif", Format: "gif", Kind: "gif", Tier: "hd", Bytes: 7_000_000},
		{URL: "https://cdn/x/md.gif", Format: "gif", Kind: "mediumgif", Tier: "md", Bytes: 300_000},
		{URL: "https://cdn/x/md-preview.gif", Format: "gif", Kind: "gifpreview", Tier: "md", Bytes: 100_000, Still: true},
		{URL: "https://cdn/x/sm.gif", Format: "gif", Kind: "tinygif", Tier: "sm", Bytes: 100_000},
		{URL: "https://cdn/x/md.webp", Format: "webp", Kind: "webp", Tier: "md", Bytes: 200_000},
	}}

	got, err := pickGIFRendition(item, 5_000_000)
	if err != nil {
		t.Fatalf("pickGIFRendition retornou erro: %v", err)
	}
	if got.Kind != "mediumgif" {
		t.Errorf("esperava a md.gif (mediumgif), obtive %q", got.Kind)
	}

	// md estourando o teto → degrada para sm (nunca para hd maior).
	got, err = pickGIFRendition(gifItem{Renditions: []gifRendition{
		{URL: "a", Format: "gif", Kind: "mediumgif", Tier: "md", Bytes: 6_000_000},
		{URL: "b", Format: "gif", Kind: "tinygif", Tier: "sm", Bytes: 100_000},
	}}, 5_000_000)
	if err != nil {
		t.Fatalf("pickGIFRendition (degradação de tamanho) retornou erro: %v", err)
	}
	if got.Kind != "tinygif" {
		t.Errorf("esperava tinygif, obtive %q", got.Kind)
	}

	// Still nunca é preferida sobre animada do mesmo tier.
	got, err = pickGIFRendition(gifItem{Renditions: []gifRendition{
		{URL: "still", Format: "gif", Kind: "gifpreview", Tier: "md", Bytes: 10, Still: true},
		{URL: "anim", Format: "gif", Kind: "mediumgif", Tier: "md", Bytes: 20},
	}}, 5_000_000)
	if err != nil {
		t.Fatalf("pickGIFRendition (still) retornou erro: %v", err)
	}
	if got.Kind != "mediumgif" {
		t.Errorf("rendition still não deveria ser preferida, obtive %q", got.Kind)
	}

	// Sem GIF animado → erro (o embed é um embed GIF; webp não substitui).
	if _, err := pickGIFRendition(gifItem{Renditions: []gifRendition{
		{URL: "w", Format: "webp", Kind: "webp", Tier: "md"},
	}}, 5_000_000); err == nil {
		t.Errorf("pickGIFRendition sem rendition GIF deveria retornar erro")
	}

	// Bytes desconhecido (0): aceita e prefere o tier pedido.
	got, err = pickGIFRendition(gifItem{Renditions: []gifRendition{
		{URL: "hd", Format: "gif", Kind: "gif", Tier: "hd"},
		{URL: "md", Format: "gif", Kind: "mediumgif", Tier: "md"},
	}}, 5_000_000)
	if err != nil {
		t.Fatalf("pickGIFRendition (bytes 0) retornou erro: %v", err)
	}
	if got.Kind != "mediumgif" {
		t.Errorf("esperava mediumgif (tier md), obtive %q", got.Kind)
	}

	// Mesmo tier: escolha determinística (maior área), qualquer que seja a
	// ordem das renditions (elas vêm de maps, cuja ordem é aleatória).
	big := gifRendition{URL: "big", Format: "gif", Kind: "fixed_height", Tier: "md", Width: 334, Height: 200}
	small := gifRendition{URL: "small", Format: "gif", Kind: "fixed_width", Tier: "md", Width: 200, Height: 120}
	for _, order := range [][]gifRendition{{small, big}, {big, small}} {
		got, err := pickGIFRendition(gifItem{Renditions: order}, 5_000_000)
		if err != nil {
			t.Fatalf("pickGIFRendition (mesmo tier) retornou erro: %v", err)
		}
		if got.Kind != "fixed_height" {
			t.Errorf("pickGIFRendition deveria escolher a maior área do tier, obtive %q", got.Kind)
		}
	}
}

func TestMediaFormatFromURLAndTierByArea(t *testing.T) {
	if got := mediaFormatFromURL("https://cdn/x/y.gif?v=1#f"); got != "gif" {
		t.Errorf("mediaFormatFromURL(gif) = %q", got)
	}
	if got := mediaFormatFromURL("https://cdn/x/y.jpeg"); got != "jpg" {
		t.Errorf("mediaFormatFromURL(jpeg) = %q", got)
	}
	if got := mediaFormatFromURL("https://cdn/x/y"); got != "" {
		t.Errorf("mediaFormatFromURL(sem extensão) = %q, esperado vazio", got)
	}
	if got := tierByArea(640, 100); got != "hd" {
		t.Errorf("tierByArea(640,100) = %q, esperado hd", got)
	}
	if got := tierByArea(0, 220); got != "sm" {
		t.Errorf("tierByArea(0,220) = %q, esperado sm", got)
	}
}

// TestGIFHeaderDims cobre a guarda do caminho sem re-encode: o blob do provedor
// é gravado como veio, então as dimensões declaradas no cabeçalho são o que
// fecha a decompression bomb antes de entrar na media.
func TestGIFHeaderDims(t *testing.T) {
	w, h, err := gifHeaderDims(gifAvatarBytes(640, 480))
	if err != nil {
		t.Fatalf("gifHeaderDims retornou erro: %v", err)
	}
	if w != 640 || h != 480 {
		t.Errorf("gifHeaderDims = (%d, %d), esperado (640, 480)", w, h)
	}

	// Cabeçalho patológico: 65535x65535 declarados (arquivo minúsculo).
	bomb := append([]byte("GIF89a"), 0xFF, 0xFF, 0xFF, 0xFF, 0x00, 0x00, 0x00)
	if _, _, err := gifHeaderDims(bomb); err == nil {
		t.Errorf("gifHeaderDims deveria rejeitar dimensões acima do teto")
	}

	if _, _, err := gifHeaderDims([]byte("isso não é um gif")); err == nil {
		t.Errorf("gifHeaderDims deveria rejeitar conteúdo que não é GIF")
	}
}

/* ------------------------------------- deduplicação por URL (cache embed) */

// TestGetOrCreateEmbedGIFCacheHit cobre o requisito central: o primeiro scrape
// de uma URL GIF grava o embed (fetch_method='gif'); qualquer mensagem seguinte
// com a MESMA URL recebe o embed gravado direto do banco, sem scrape e sem
// gasto de rate limit (o cache do passo 2 decide antes do rate limit e do
// resolvedor). Também prova que o CHECK de fetch_method aceita 'gif'
// (migration 016).
func TestGetOrCreateEmbedGIFCacheHit(t *testing.T) {
	ctx := testCtx()
	user := newTestMessageUser(t)

	const gifURL = "https://tenor.com/view/cats-dancing-gif-1234567890"
	title := "Cats Dancing GIF (scrape do usuário A)"
	seed, err := storage.UpsertLinkEmbed(ctx, models.Embed{
		SourceType:  embedSourceLink,
		FetchMethod: embedFetchGIF,
		CacheKey:    strPtr(gifURL),
		URL:         strPtr(gifURL),
		Title:       &title,
		Provider:    strPtr("Tenor"),
	})
	if err != nil {
		t.Fatalf("UpsertLinkEmbed com fetch_method='gif' retornou erro: %v", err)
	}
	if seed.FetchMethod != embedFetchGIF {
		t.Fatalf("fetch_method gravado = %q, esperado %q", seed.FetchMethod, embedFetchGIF)
	}

	// "Usuário B" com a mesma URL: recebe o embed gravado, sem rede.
	got, refetched, err := GetOrCreateEmbed(ctx, user.ID, gifURL)
	if err != nil {
		t.Fatalf("GetOrCreateEmbed com cache hit de GIF retornou erro (deveria evitar o scrape): %v", err)
	}
	if refetched {
		t.Errorf("cache hit dentro do TTL não deve marcar refetch, obtive refetched=true")
	}
	if got.ID != seed.ID {
		t.Errorf("esperava o embed gravado (id %s), obtive id %s", seed.ID, got.ID)
	}
	if got.Title == nil || *got.Title != title {
		t.Errorf("esperava o título do scrape anterior, obtive %v", got.Title)
	}
	if got.FetchMethod != embedFetchGIF {
		t.Errorf("fetch_method = %q, esperado %q", got.FetchMethod, embedFetchGIF)
	}
}

// TestUpsertLinkEmbedGIFDedupByURL: a mesma URL nunca gera duas rows — o upsert
// por cache_key reescreve a row existente (deduplicação pelo lado da escrita).
func TestUpsertLinkEmbedGIFDedupByURL(t *testing.T) {
	ctx := testCtx()

	const gifURL = "https://giphy.com/gifs/happy-skeleton-okfvUCpgArv3y"
	first, err := storage.UpsertLinkEmbed(ctx, models.Embed{
		SourceType:  embedSourceLink,
		FetchMethod: embedFetchGIF,
		CacheKey:    strPtr(gifURL),
		URL:         strPtr(gifURL),
		Title:       strPtr("v1"),
		Provider:    strPtr("GIPHY"),
	})
	if err != nil {
		t.Fatalf("primeiro UpsertLinkEmbed retornou erro: %v", err)
	}

	second, err := storage.UpsertLinkEmbed(ctx, models.Embed{
		SourceType:  embedSourceLink,
		FetchMethod: embedFetchGIF,
		CacheKey:    strPtr(gifURL),
		URL:         strPtr(gifURL),
		Title:       strPtr("v2"),
		Provider:    strPtr("GIPHY"),
	})
	if err != nil {
		t.Fatalf("segundo UpsertLinkEmbed retornou erro: %v", err)
	}

	if second.ID != first.ID {
		t.Fatalf("a mesma URL deveria reescrever a mesma row, obtive ids %s e %s", first.ID, second.ID)
	}

	cached, err := storage.GetEmbedByCacheKey(ctx, gifURL)
	if err != nil {
		t.Fatalf("GetEmbedByCacheKey retornou erro: %v", err)
	}
	if cached.Title == nil || *cached.Title != "v2" {
		t.Errorf("esperava a row reescrita com title v2, obtive %v", cached.Title)
	}
	if cached.FetchedAt == nil || time.Since(*cached.FetchedAt) > time.Minute {
		t.Errorf("o upsert deveria atualizar fetched_at, obtive %v", cached.FetchedAt)
	}
}

// TestGetOrCreateEmbedGIFHostRouting: a deduplicação é por URL exata (a mesma
// regra dos link embeds): a URL já scrapada é devolvida do banco antes de
// qualquer rota de resolvedor/rede; uma URL diferente (outro path) não herda o
// cache da primeira.
func TestGetOrCreateEmbedGIFHostRouting(t *testing.T) {
	ctx := testCtx()
	user := newTestMessageUser(t)

	const canonical = "https://klipy.com/gifs/orihime-bread-girl"
	_, err := storage.UpsertLinkEmbed(ctx, models.Embed{
		SourceType:  embedSourceLink,
		FetchMethod: embedFetchGIF,
		CacheKey:    strPtr(canonical),
		URL:         strPtr(canonical),
		Title:       strPtr("Orihime Bread Girl"),
		Provider:    strPtr("KLIPY"),
	})
	if err != nil {
		t.Fatalf("UpsertLinkEmbed retornou erro: %v", err)
	}

	// URL exata → cache hit (sem resolvedor, sem rede).
	got, refetched, err := GetOrCreateEmbed(ctx, user.ID, canonical)
	if err != nil || refetched || got.Title == nil || *got.Title != "Orihime Bread Girl" {
		t.Fatalf("URL exata deveria acertar o cache: got=%+v refetched=%v err=%v", got, refetched, err)
	}

	// Outro path é outra URL: não existe cache para ela (a deduplicação é por
	// URL exata; o resolvedor rodaria para essa URL e a gravaria por ela).
	variant := "https://klipy.com/gifs/orihime-bread-girl/player"
	if cached, err := storage.GetEmbedByCacheKey(ctx, variant); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("a variante não deveria herdar o cache da URL original (deduplicação é por URL exata), err=%v embed=%+v", err, cached)
	}
}
