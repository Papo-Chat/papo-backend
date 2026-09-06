package main

import (
	"flag"
	"fmt"
	"os"
	"time"
)

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "\nERRO: "+format+"\n", args...)
	os.Exit(1)
}

func main() {
	baseURL := flag.String("url", "http://localhost:8080", "URL base do backend")
	users := flag.Int("users", 1000, "total de usuários a criar (além do owner)")
	messages := flag.Int("messages", 500000, "total de mensagens a criar")
	channelsN := flag.Int("channels", 40, "total de canais de texto")
	emojisN := flag.Int("emojis", 500, "total de emojis customizados (máx. 500)")
	chunks := flag.Int("chunks", 10, "fatias de seeding; a medição roda após cada fatia")
	concurrency := flag.Int("concurrency", 16, "concorrência do seeding de mensagens")
	userConc := flag.Int("user-concurrency", 8, "concorrência do seeding de usuários")
	measureConc := flag.Int("measure-concurrency", 20, "concorrência da medição")
	perPhase := flag.Int("requests", 2000, "total de tarefas por passada de medição")
	reportDir := flag.String("report-dir", "", "diretório do relatório (default ./stress-report-<ts>)")
	measureOnly := flag.Bool("measure-only", false, "mede apenas, sem seeding (exige -username e -password)")
	loginUser := flag.String("username", "", "usuário para -measure-only")
	loginPass := flag.String("password", "", "senha para -measure-only")
	flag.Parse()

	if *chunks < 1 || *chunks > 20 {
		fatal("-chunks deve estar entre 1 e 20")
	}
	if *emojisN < 0 || *emojisN > 500 {
		fatal("-emojis deve estar entre 0 e 500 (limite do backend)")
	}
	if *measureOnly && (*loginUser == "" || *loginPass == "") {
		fatal("-measure-only exige -username e -password")
	}

	c := NewClient(*baseURL)
	st := NewStats()
	dg := NewDataGen()
	s := newState(c, st, dg, 1337)

	fmt.Printf("stress: %s | %d usuários | %d mensagens | %d canais | %d emojis\n",
		*baseURL, *users, *messages, *channelsN, *emojisN)
	fmt.Println("aviso: o backend deve rodar com rate limits altos, ex.:")
	fmt.Println("  RATE_LIMIT=1000 RATE_BURST=2000 AUTH_RATE_LIMIT=200 AUTH_RATE_BURST=400")
	fmt.Println("  LINK_PREVIEW_ENABLED=false THUMBNAIL_ENABLED=false (recomendado)")

	if *measureOnly {
		if err := runMeasureOnly(s, *loginUser, *loginPass, *perPhase, *measureConc); err != nil {
			fatal("%v", err)
		}
	} else {
		if err := runFull(s, *users, *messages, *channelsN, *emojisN, *chunks,
			*userConc, *concurrency, *measureConc, *perPhase); err != nil {
			fatal("%v", err)
		}
	}

	dir := *reportDir
	if dir == "" {
		dir = "stress-report-" + time.Now().Format("20060102-150405")
	}
	ops, errs, rl := c.SeedTotals()
	rpt := buildReport(st.Phases(), *baseURL, ops, errs, rl)
	phases := st.Phases()
	printASCII(phases)
	printDegradation(phases)
	if err := writeReport(dir, rpt); err != nil {
		fatal("gravar relatório: %v", err)
	}
	fmt.Printf("\nseed total: %d operações, %d erros, %d rate-limited\n", ops, errs, rl)
	fmt.Printf("relatório em %s (report.json, report.csv, chart.svg)\n", dir)
}

func runFull(s *state, users, messages, channelsN, emojisN, chunks,
	userConc, msgConc, measureConc, perPhase int) error {

	fmt.Println("setup: servidor, role member, canais...")
	if err := s.setup("Stress Server"); err != nil {
		return err
	}
	if err := s.createChannels(channelsN); err != nil {
		return err
	}

	fmt.Printf("medição fase 0%% (servidor vazio)\n")
	s.runPhase(perPhase, measureConc, 0, "0%")
	printPhaseTable(st_phases(s)[len(st_phases(s))-1])

	plan := chunkPlan{
		users:     users / chunks,
		messages:  messages / chunks,
		reactions: int(0.03 * float64(messages) / float64(chunks)),
		edits:     int(0.02 * float64(messages) / float64(chunks)),
		deletes:   int(0.01 * float64(messages) / float64(chunks)),
		pins:      2 * channelsN,
		emojis:    emojisN / chunks,
		roles:     1,
	}

	for n := 1; n <= chunks; n++ {
		p := plan
		if n == chunks {
			p.users = users - plan.users*(n-1)
			p.messages = messages - plan.messages*(n-1)
			p.emojis = emojisN - plan.emojis*(n-1)
		}
		fmt.Printf("\n--- chunk %d/%d: %d usuários, %d mensagens ---\n",
			n, chunks, p.users, p.messages)
		start := time.Now()
		firstErr, nErrs := s.seedChunk(n, p, userConc, msgConc)
		if firstErr != nil {
			fmt.Printf("aviso: chunk %d: %d erro(s), primeiro: %v\n", n, nErrs, firstErr)
			if nErrs > 20 {
				return fmt.Errorf("muitos erros no chunk %d (%d); abortando. Verifique o backend", n, nErrs)
			}
		}
		fmt.Printf("seeding do chunk em %.1fs\n", time.Since(start).Seconds())

		label := fmt.Sprintf("%d%%", n*100/chunks)
		fmt.Printf("medição fase %s\n", label)
		s.runPhase(perPhase, measureConc, n, label)
		printPhaseTable(st_phases(s)[len(st_phases(s))-1])
	}
	return nil
}

func st_phases(s *state) []*PhaseStats { return s.st.Phases() }

func runMeasureOnly(s *state, username, password string, perPhase, measureConc int) error {
	u := &User{Username: username, Password: password}
	if err := s.login(u); err != nil {
		return err
	}
	s.addUser(u)

	r := s.c.do(u, "GET", "/channels", nil, nil)
	if !r.ok() {
		return fmt.Errorf("GET /channels: %s", r.problem())
	}
	var cl struct {
		Channels []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
			Type string `json:"type"`
		} `json:"channels"`
	}
	if !jsonPtr(r.body, &cl) {
		return fmt.Errorf("GET /channels: resposta inesperada")
	}
	for _, ch := range cl.Channels {
		if ch.Type != "text" {
			continue
		}
		s.channels = append(s.channels, chanRef{ID: ch.ID, Name: ch.Name, Type: "text"})
		s.textChans = append(s.textChans, len(s.channels)-1)
		s.msgRings[ch.ID] = newMsgRing(2048)
	}
	if len(s.textChans) == 0 {
		return fmt.Errorf("nenhum canal de texto encontrado")
	}

	fmt.Println("preenchendo amostras com 100 mensagens...")
	if err := s.seedMessages(100, 8); err != nil {
		fmt.Printf("aviso: %v\n", err)
	}

	fmt.Println("medição (estado atual)...")
	s.runPhase(perPhase, measureConc, 0, "now")
	printPhaseTable(st_phases(s)[0])
	return nil
}
