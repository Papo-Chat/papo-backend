package services

import (
	"encoding/json"
	"testing"
)

// TestGiphyImagesNumberShapes: o mapa "images" do giphy mistura o shape do
// número — "width":480 e "width":"498" (e size como string). Com int rígido o
// Unmarshal do objeto inteiro falhava e o resolvedor caía no caminho degradado.
func TestGiphyImagesNumberShapes(t *testing.T) {
	const raw = `{"fixed_height":{"height":"200","width":"220","size":"288760",` +
		`"url":"https://media.giphy.com/media/QBd2kLB5qDmysEXre9/200.gif"},` +
		`"looping":{"height":120,"width":200,"size":56740,` +
		`"url":"https://media.giphy.com/media/QBd2kLB5qDmysEXre9/looping.gif"}}`

	var images map[string]giphyImageEntry
	if err := json.Unmarshal([]byte(raw), &images); err != nil {
		t.Fatalf("mapa images do giphy não parseou: %v", err)
	}

	renditions := giphyRenditionsFromImages(images)
	byKind := map[string]gifRendition{}
	for _, r := range renditions {
		byKind[r.Kind] = r
	}

	fixed := byKind["fixed_height"]
	if fixed.Width != 220 || fixed.Height != 200 {
		t.Errorf("fixed_height dims = (%d, %d), esperado (220, 200) vindos de string", fixed.Width, fixed.Height)
	}
	if fixed.Format != "gif" || fixed.Tier != "md" || fixed.Bytes != 288760 {
		t.Errorf("fixed_height = %+v, esperado gif / md / 288760", fixed)
	}

	looping := byKind["looping"]
	if looping.Width != 200 || looping.Height != 120 || looping.Tier != "sm" {
		t.Errorf("looping = %+v, esperado 200x120 / sm", looping)
	}
}

// TestGiphyJSONLDFromQuantitativeValue: o JSON-LD do giphy entrega width/height
// como objeto QuantitativeValue. Com int rígido o parse do documento falhava e
// o título + a media URL autoritativa (fallback de id) se perdiam.
func TestGiphyJSONLDFromQuantitativeValue(t *testing.T) {
	body := []byte(`<script type="application/ld+json">{"@context":"https://schema.org",` +
		`"@type":"Article","headline":"Mr Bean Waiting GIF by MOODMAN","image":` +
		`{"@type":"ImageObject","url":"https://media0.giphy.com/media/v1.tok/QBd2kLB5qDmysEXre9/giphy.gif",` +
		`"height":{"@type":"QuantitativeValue","value":288},"width":{"@type":"QuantitativeValue","value":480}}}` +
		`</script>`)

	ld := giphyJSONLDFrom(body)
	if ld == nil {
		t.Fatalf("giphyJSONLDFrom retornou nil para um JSON-LD válido do giphy")
	}
	if ld.Title != "Mr Bean Waiting GIF by MOODMAN" {
		t.Errorf("headline = %q", ld.Title)
	}
	if ld.Image.URL == "" {
		t.Errorf("image.url vazio: o id autoritativo do fallback se perderia")
	}
	if w, h := jsonInt(ld.Image.Width), jsonInt(ld.Image.Height); w != 480 || h != 288 {
		t.Errorf("image dims = (%d, %d), esperado (480, 288)", w, h)
	}
}

func TestJSONNumberShapes(t *testing.T) {
	cases := []struct {
		in   string
		want int64
	}{
		{`480`, 480},
		{`"480"`, 480},
		{`{"@type":"QuantitativeValue","value":480}`, 480},
		{`{"value":"480"}`, 480},
		{`null`, 0},
		{`{}`, 0},
		{`"abc"`, 0},
	}
	for _, c := range cases {
		if got := jsonNumber(json.RawMessage(c.in)); got != c.want {
			t.Errorf("jsonNumber(%s) = %d, esperado %d", c.in, got, c.want)
		}
	}
}
