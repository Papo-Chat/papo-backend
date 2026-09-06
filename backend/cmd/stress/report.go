package main

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// epOrder define a ordem dos endpoints no relatório.
var epOrder = []string{
	epListChannels,
	epListMessages,
	epPollMessages,
	epSendMessage,
	epEditMessage,
	epDeleteMessage,
	epAddReaction,
	epListReactions,
	epRemoveReaction,
	epPinMessage,
	epUnpinMessage,
	epListPinned,
	epListUsers,
	epGetProfile,
	epProfileBatch,
	epUpdateSettings,
	epUpdateProfile,
	epUpdateStatus,
	epUpdateAvatar,
	epUpdateBanner,
	epListEmojis,
	epSearch,
	epDownloadAttachment,
	epDownloadMedia,
	epGetServer,
	epWhoami,
	epRefresh,
	epConnectedDevices,
}

// epChart são os endpoints plotados no gráfico (os mais usados pelo frontend).
var epChart = []string{
	epListChannels,
	epListMessages,
	epPollMessages,
	epSendMessage,
	epEditMessage,
	epAddReaction,
	epListUsers,
	epGetProfile,
	epSearch,
	epWhoami,
}

var chartColors = []string{
	"#e6194b", "#3cb44b", "#4363d8", "#f58231", "#911eb4",
	"#42d4f4", "#f032e6", "#bfef45", "#fabed4", "#469990",
}

type epSummary struct {
	Ep       string   `json:"endpoint"`
	N        int64    `json:"n"`
	Err      int64    `json:"errors"`
	ErrPct   float64  `json:"error_pct"`
	RL       int64    `json:"rate_limited"`
	P50Ms    float64  `json:"p50_ms"`
	P95Ms    float64  `json:"p95_ms"`
	P99Ms    float64  `json:"p99_ms"`
	MaxMs    float64  `json:"max_ms"`
	RPS      float64  `json:"rps"`
	ErrStats map[int]int `json:"error_status,omitempty"`
}

type phaseSummary struct {
	Index     int         `json:"index"`
	Label     string      `json:"label"`
	Start     time.Time   `json:"start"`
	End       time.Time   `json:"end"`
	DurationMs float64    `json:"duration_ms"`
	Endpoints []epSummary `json:"endpoints"`
}

type report struct {
	GeneratedAt string       `json:"generated_at"`
	BaseURL     string       `json:"base_url"`
	Phases      []phaseSummary `json:"phases"`
	SeedOps     int64        `json:"seed_ops"`
	SeedErrors  int64        `json:"seed_errors"`
	SeedRL      int64        `json:"seed_rate_limited"`
}

func ms(d time.Duration) float64 {
	return float64(d.Nanoseconds()) / 1e6
}

func phaseToSummary(p *PhaseStats, dur time.Duration) phaseSummary {
	eps := make([]epSummary, 0, len(epOrder))
	for _, name := range epOrder {
		e := p.Ep(name)
		if e == nil || e.N == 0 {
			continue
		}
		max := time.Duration(0)
		for _, d := range e.Dur {
			if d > max {
				max = d
			}
		}
		eps = append(eps, epSummary{
			Ep:       name,
			N:        e.N,
			Err:      e.Err,
			ErrPct:   float64(e.Err) / float64(e.N) * 100,
			RL:       e.RL,
			P50Ms:    ms(Percentile(e.Dur, 0.50)),
			P95Ms:    ms(Percentile(e.Dur, 0.95)),
			P99Ms:    ms(Percentile(e.Dur, 0.99)),
			MaxMs:    ms(max),
			RPS:      float64(e.N) / dur.Seconds(),
			ErrStats: e.ErrStatus,
		})
	}
	sort.Slice(eps, func(i, j int) bool { return eps[i].N > eps[j].N })
	return phaseSummary{
		Index:      p.Index,
		Label:      p.Label,
		Start:      p.Start,
		End:        p.End,
		DurationMs: ms(dur),
		Endpoints:  eps,
	}
}

func buildReport(phases []*PhaseStats, base string, ops, errs, rl int64) report {
	out := report{
		GeneratedAt: time.Now().Format(time.RFC3339),
		BaseURL:     base,
		SeedOps:     ops,
		SeedErrors:  errs,
		SeedRL:      rl,
	}
	for _, p := range phases {
		out.Phases = append(out.Phases, phaseToSummary(p, p.End.Sub(p.Start)))
	}
	return out
}

// printPhaseTable imprime a tabela de uma fase no terminal.
func printPhaseTable(p *PhaseStats) {
	dur := p.End.Sub(p.Start)
	fmt.Printf("\n=== Fase %s (%s, %.1fs) ===\n", p.Label,
		p.Start.Format("15:04:05"), dur.Seconds())
	fmt.Printf("%-32s %7s %7s %6s %9s %9s %9s %8s\n",
		"endpoint", "n", "err%", "429", "p50(ms)", "p95(ms)", "p99(ms)", "rps")
	for _, name := range epOrder {
		e := p.Ep(name)
		if e == nil || e.N == 0 {
			continue
		}
		max := time.Duration(0)
		for _, d := range e.Dur {
			if d > max {
				max = d
			}
		}
		_ = max
		fmt.Printf("%-32s %7d %6.1f%% %6d %9.1f %9.1f %9.1f %8.1f\n",
			name, e.N, float64(e.Err)/float64(e.N)*100, e.RL,
			ms(Percentile(e.Dur, 0.50)), ms(Percentile(e.Dur, 0.95)),
			ms(Percentile(e.Dur, 0.99)), float64(e.N)/dur.Seconds())
	}
}

// printASCII imprime um gráfico ASCII de p95 por fase para os endpoints-chave.
func printASCII(phases []*PhaseStats) {
	fmt.Println("\n=== p95 (ms) por fase de seeding ===")
	header := "endpoint"
	for _, p := range phases {
		header += fmt.Sprintf(" %7s", p.Label)
	}
	fmt.Println(header)
	for _, name := range epChart {
		line := name
		for _, p := range phases {
			e := p.Ep(name)
			if e == nil || e.N == 0 {
				line += fmt.Sprintf(" %7s", "-")
				continue
			}
			line += fmt.Sprintf(" %7.1f", ms(Percentile(e.Dur, 0.95)))
		}
		fmt.Println(line)
	}
}

// printDegradation destaca a degradação de p95 do início para o fim.
func printDegradation(phases []*PhaseStats) {
	if len(phases) < 2 {
		return
	}
	first, last := phases[0], phases[len(phases)-1]
	fmt.Println("\n=== Degradação de p95 (primeira fase -> última) ===")
	fmt.Printf("%-32s %10s %10s %10s\n", "endpoint", "p95@0%", "p95@fim", "delta")
	for _, name := range epChart {
		a, b := first.Ep(name), last.Ep(name)
		if a == nil || a.N == 0 || b == nil || b.N == 0 {
			continue
		}
		pa, pb := ms(Percentile(a.Dur, 0.95)), ms(Percentile(b.Dur, 0.95))
		delta := "-"
		if pa > 0 {
			delta = fmt.Sprintf("%+.0f%%", (pb-pa)/pa*100)
		}
		fmt.Printf("%-32s %10.1f %10.1f %10s\n", name, pa, pb, delta)
	}
}

// writeReport grava report.json, report.csv e chart.svg no diretório.
func writeReport(dir string, r report) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	jb, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "report.json"), jb, 0o644); err != nil {
		return err
	}

	csvPath := filepath.Join(dir, "report.csv")
	f, err := os.Create(csvPath)
	if err != nil {
		return err
	}
	w := csv.NewWriter(f)
	_ = w.Write([]string{"phase", "label", "endpoint", "n", "errors", "error_pct",
		"rate_limited", "p50_ms", "p95_ms", "p99_ms", "max_ms", "rps"})
	for _, p := range r.Phases {
		for _, e := range p.Endpoints {
			_ = w.Write([]string{
				fmt.Sprintf("%d", p.Index), p.Label, e.Ep,
				fmt.Sprintf("%d", e.N), fmt.Sprintf("%d", e.Err),
				fmt.Sprintf("%.2f", e.ErrPct), fmt.Sprintf("%d", e.RL),
				fmt.Sprintf("%.2f", e.P50Ms), fmt.Sprintf("%.2f", e.P95Ms),
				fmt.Sprintf("%.2f", e.P99Ms), fmt.Sprintf("%.2f", e.MaxMs),
				fmt.Sprintf("%.1f", e.RPS),
			})
		}
	}
	w.Flush()
	if err := w.Error(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return writeSVG(filepath.Join(dir, "chart.svg"), r)
}

func niceCeil(v float64) float64 {
	if v <= 0 {
		return 10
	}
	exp := math.Pow10(int(math.Floor(math.Log10(v))))
	for _, m := range []float64{1, 2, 2.5, 5, 10} {
		if v <= m*exp {
			return m * exp
		}
	}
	return 10 * exp
}

func writeSVG(path string, r report) error {
	const (
		width  = 1000
		height = 560
		left   = 70.0
		right  = 240.0
		top    = 50.0
		bottom = 60.0
	)
	plotW := float64(width) - left - right
	plotH := float64(height) - top - bottom

	n := len(r.Phases)
	if n == 0 {
		return nil
	}

	maxV := 0.0
	for _, p := range r.Phases {
		for _, e := range p.Endpoints {
			for _, name := range epChart {
				if e.Ep == name && e.P95Ms > maxV {
					maxV = e.P95Ms
				}
			}
		}
	}
	yMax := niceCeil(maxV)

	xFor := func(i int) float64 {
		if n == 1 {
			return left + plotW/2
		}
		return left + plotW*float64(i)/float64(n-1)
	}
	yFor := func(v float64) float64 {
		return top + plotH*(1 - v/yMax)
	}

	var b strings.Builder
	b.WriteString(fmt.Sprintf(`<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="%d" font-family="monospace">`, width, height))
	b.WriteString(fmt.Sprintf(`<rect width="%d" height="%d" fill="white"/>`, width, height))
	b.WriteString(fmt.Sprintf(`<text x="%d" y="28" font-size="16" font-weight="bold">p95 (ms) por fase de seeding — %s</text>`, width/2-150, r.BaseURL))

	// grid + eixo Y
	steps := 5
	for i := 0; i <= steps; i++ {
		v := yMax * float64(i) / float64(steps)
		y := yFor(v)
		b.WriteString(fmt.Sprintf(`<line x1="%d" y1="%.1f" x2="%.1f" y2="%.1f" stroke="#ddd"/>`,
			int(left), y, left+plotW, y))
		b.WriteString(fmt.Sprintf(`<text x="%.1f" y="%.1f" font-size="11" text-anchor="end">%.0f</text>`,
			left-8, y+4, v))
	}
	// eixo X
	for i, p := range r.Phases {
		x := xFor(i)
		b.WriteString(fmt.Sprintf(`<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" stroke="#ddd"/>`,
			x, top, x, top+plotH))
		b.WriteString(fmt.Sprintf(`<text x="%.1f" y="%.1f" font-size="11" text-anchor="middle">%s</text>`,
			x, top+plotH+18, p.Label))
	}
	b.WriteString(fmt.Sprintf(`<text x="%.1f" y="%d" font-size="12" text-anchor="middle">seeding concluído</text>`,
		left+plotW/2, height-12))
	b.WriteString(fmt.Sprintf(`<text x="16" y="%.1f" font-size="12" text-anchor="middle" transform="rotate(-90 16 %.1f)">ms</text>`,
		top+plotH/2, top+plotH/2))

	// séries
	for ci, name := range epChart {
		color := chartColors[ci%len(chartColors)]
		var pts []string
		for i, p := range r.Phases {
			var e *epSummary
			for j := range p.Endpoints {
				if p.Endpoints[j].Ep == name {
					e = &p.Endpoints[j]
					break
				}
			}
			if e == nil {
				continue
			}
			pts = append(pts, fmt.Sprintf("%.1f,%.1f", xFor(i), yFor(e.P95Ms)))
		}
		if len(pts) < 2 {
			continue
		}
		b.WriteString(fmt.Sprintf(`<polyline points="%s" fill="none" stroke="%s" stroke-width="2"/>`,
			strings.Join(pts, " "), color))
		for _, pt := range pts {
			xy := strings.Split(pt, ",")
			b.WriteString(fmt.Sprintf(`<circle cx="%s" cy="%s" r="2.5" fill="%s"/>`, xy[0], xy[1], color))
		}
	}

	// legenda
	ly := top + 10.0
	for ci, name := range epChart {
		color := chartColors[ci%len(chartColors)]
		b.WriteString(fmt.Sprintf(`<rect x="%.1f" y="%.1f" width="14" height="3" fill="%s"/>`,
			left+plotW+16, ly+4, color))
		b.WriteString(fmt.Sprintf(`<text x="%.1f" y="%.1f" font-size="11">%s</text>`,
			left+plotW+36, ly+8, name))
		ly += 20
	}
	b.WriteString(`</svg>`)
	return os.WriteFile(path, []byte(b.String()), 0o644)
}
