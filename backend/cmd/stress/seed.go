package main

import (
	"encoding/base64"
	"fmt"
	"math/rand"
	"net/http"
	"sync"
	"sync/atomic"
)

type chanRef struct {
	ID   string
	Name string
	Type string
}

type msgRef struct {
	ID     string
	Author *User
	atts   []string
}

// msgRing é um buffer circular limitado com acesso aleatório.
type msgRing struct {
	mu  sync.Mutex
	buf []msgRef
	pos int
	n   int
	cap int
}

func newMsgRing(cap int) *msgRing {
	return &msgRing{buf: make([]msgRef, 0, cap), cap: cap}
}

func (r *msgRing) add(m msgRef) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.buf) < r.cap {
		r.buf = append(r.buf, m)
	} else {
		r.buf[r.pos] = m
	}
	r.pos = (r.pos + 1) % r.cap
	if r.n < r.cap {
		r.n++
	}
}

func (r *msgRing) get(rng *rand.Rand) (msgRef, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.n == 0 {
		return msgRef{}, false
	}
	return r.buf[rng.Intn(r.n)], true
}

// strRing é o mesmo, para strings (attachment IDs, media hashes).
type strRing struct {
	mu  sync.Mutex
	buf []string
	pos int
	n   int
	cap int
}

func newStrRing(cap int) *strRing {
	return &strRing{buf: make([]string, 0, cap), cap: cap}
}

func (r *strRing) add(s string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.buf) < r.cap {
		r.buf = append(r.buf, s)
	} else {
		r.buf[r.pos] = s
	}
	r.pos = (r.pos + 1) % r.cap
	if r.n < r.cap {
		r.n++
	}
}

func (r *strRing) get(rng *rand.Rand) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.n == 0 {
		return "", false
	}
	return r.buf[rng.Intn(r.n)], true
}

func (r *strRing) len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.n
}

// errCollector guarda o primeiro erro e a contagem de um lote de goroutines.
type errCollector struct {
	once sync.Once
	mu   sync.Mutex
	err  error
	n    int
}

func (e *errCollector) add(err error) {
	if err == nil {
		return
	}
	e.mu.Lock()
	e.n++
	e.mu.Unlock()
	e.once.Do(func() { e.err = err })
}

func (e *errCollector) get() error { return e.err }

func (e *errCollector) count() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.n
}

// state é o estado compartilhado entre as fases de seeding e medição.
type state struct {
	c          *Client
	st         *Stats
	dg         *DataGen
	rng        *rand.Rand
	owner      *User
	memberRole string

	mu        sync.Mutex
	users     []*User
	channels  []chanRef
	textChans []int
	emojiIDs  []string
	roles     []string

	msgRings  map[string]*msgRing
	attRing   *strRing
	mediaRing *strRing

	delMu   sync.Mutex
	deleted map[string]struct{}

	nextUser atomic.Int64
}

// markDeleted registra uma mensagem removida (e os anexos dela, que caem em
// cascata), para que reply_to e download de anexo não as referenciem mais
// (os buffers circulares as manteriam por até 2048/4096 inserções).
func (s *state) markDeleted(id string, atts []string) {
	s.delMu.Lock()
	s.deleted[id] = struct{}{}
	for _, a := range atts {
		s.deleted[a] = struct{}{}
	}
	s.delMu.Unlock()
}

func (s *state) isDeleted(id string) bool {
	s.delMu.Lock()
	_, ok := s.deleted[id]
	s.delMu.Unlock()
	return ok
}

// getLiveMsg sorteia uma mensagem do canal que ainda existe, pulando IDs
// já excluídos (o buffer circular pode retê-los por até 2048 inserções).
func (s *state) getLiveMsg(chID string, rng *rand.Rand) (msgRef, bool) {
	for t := 0; t < 5; t++ {
		m, ok := s.msgRings[chID].get(rng)
		if !ok {
			return msgRef{}, false
		}
		if !s.isDeleted(m.ID) {
			return m, true
		}
	}
	return msgRef{}, false
}

// getLiveAtt sorteia um anexo do anel que ainda existe, pulando IDs de
// anexos cujas mensagens foram excluídas (cascata).
func (s *state) getLiveAtt(rng *rand.Rand) (string, bool) {
	for t := 0; t < 5; t++ {
		a, ok := s.attRing.get(rng)
		if !ok {
			return "", false
		}
		if !s.isDeleted(a) {
			return a, true
		}
	}
	return "", false
}

func newState(c *Client, st *Stats, dg *DataGen, seed int64) *state {
	return &state{
		c:         c,
		st:        st,
		dg:        dg,
		rng:       rand.New(rand.NewSource(seed)),
		msgRings:  map[string]*msgRing{},
		attRing:   newStrRing(4096),
		mediaRing: newStrRing(4096),
		deleted:   map[string]struct{}{},
	}
}

func (s *state) randomUser(rng *rand.Rand) *User {
	s.mu.Lock()
	var u *User
	if len(s.users) == 0 {
		u = s.owner
	} else {
		u = s.users[rng.Intn(len(s.users))]
	}
	s.mu.Unlock()
	return u
}

func (s *state) randomTextChannel(rng *rand.Rand) chanRef {
	s.mu.Lock()
	idx := s.textChans[rng.Intn(len(s.textChans))]
	ch := s.channels[idx]
	s.mu.Unlock()
	return ch
}

func (s *state) login(u *User) error {
	r := s.c.do(u, "POST", "/auth/login", map[string]string{"Content-Type": "application/json"},
		jsonBody(map[string]string{"username": u.Username, "password": u.Password}))
	s.c.seedOp(r)
	if !r.ok() {
		return fmt.Errorf("login %s: %s", u.Username, r.problem())
	}
	if u.Cookie() == "" {
		return fmt.Errorf("login %s: cookie Auth ausente", u.Username)
	}
	return nil
}

func (s *state) registerUser(i int) (*User, error) {
	u := &User{Username: s.dg.Username(i), Password: s.dg.Password(i)}
	r := s.c.do(nil, "POST", "/auth/register", map[string]string{"Content-Type": "application/json"},
		jsonBody(map[string]string{"username": u.Username, "password": u.Password}))
	s.c.seedOp(r)
	if !r.ok() {
		return nil, fmt.Errorf("register %s: %s", u.Username, r.problem())
	}
	var reg struct {
		ID string `json:"id"`
	}
	if !jsonPtr(r.body, &reg) || reg.ID == "" {
		return nil, fmt.Errorf("register %s: resposta inesperada", u.Username)
	}
	u.ID = reg.ID
	return u, nil
}

func (s *state) addUser(u *User) {
	s.mu.Lock()
	s.users = append(s.users, u)
	s.mu.Unlock()
}

// setup cria o servidor público, a role member e confere o estado inicial.
// Exige um banco vazio (sem servidor).
func (s *state) setup(serverName string) error {
	ct := map[string]string{"Content-Type": "application/json"}

	r := s.c.do(nil, "GET", "/health", nil, nil)
	if !r.ok() {
		return fmt.Errorf("health check falhou: %s", r.problem())
	}

	r = s.c.do(nil, "GET", "/server", nil, nil)
	if r.status == http.StatusOK {
		return fmt.Errorf("já existe um servidor neste banco; use um banco novo " +
			"(o servidor é singleton e não pode ser removido pela API)")
	}

	u := &User{Username: "stress_owner", Password: "Stress!owner"}
	r = s.c.do(nil, "POST", "/auth/register", ct,
		jsonBody(map[string]string{"username": u.Username, "password": u.Password}))
	if r.status == http.StatusConflict {
		return fmt.Errorf("usuário stress_owner já existe; use um banco novo")
	}
	if !r.ok() {
		return fmt.Errorf("register owner: %s", r.problem())
	}
	var reg struct {
		ID string `json:"id"`
	}
	jsonPtr(r.body, &reg)
	u.ID = reg.ID
	s.owner = u
	if err := s.login(u); err != nil {
		return err
	}

	icon := base64.StdEncoding.EncodeToString(s.dg.PNG(128, 128))
	r = s.c.do(u, "POST", "/server", ct,
		jsonBody(map[string]any{"name": serverName, "public": true, "icon_blob": icon, "icon_format": "PNG"}))
	s.c.seedOp(r)
	if !r.ok() {
		return fmt.Errorf("criar servidor: %s", r.problem())
	}

	r = s.c.do(u, "POST", "/roles", ct,
		jsonBody(map[string]any{
			"name":  "member",
			"color": "#4a90d9",
			"permissions": map[string]bool{
				"send_attachment": true,
				"pin_message":     true,
			},
		}))
	s.c.seedOp(r)
	if !r.ok() {
		return fmt.Errorf("criar role member: %s", r.problem())
	}
	var role struct {
		ID string `json:"id"`
	}
	if !jsonPtr(r.body, &role) || role.ID == "" {
		return fmt.Errorf("criar role member: resposta inesperada")
	}
	s.memberRole = role.ID
	return nil
}

// createChannels cria 5 categorias e n canais de texto.
func (s *state) createChannels(n int) error {
	ct := map[string]string{"Content-Type": "application/json"}
	for i := 0; i < 5; i++ {
		r := s.c.do(s.owner, "POST", "/channels", ct,
			jsonBody(map[string]string{"name": fmt.Sprintf("cat-%02d", i+1), "type": "category"}))
		s.c.seedOp(r)
		if !r.ok() {
			return fmt.Errorf("criar categoria %d: %s", i+1, r.problem())
		}
	}
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("canal-%02d", i+1)
		if i == 0 {
			name = "geral"
		}
		r := s.c.do(s.owner, "POST", "/channels", ct,
			jsonBody(map[string]string{"name": name, "type": "text", "topic": s.dg.Words(4)}))
		s.c.seedOp(r)
		if !r.ok() {
			return fmt.Errorf("criar canal %s: %s", name, r.problem())
		}
		var ch struct {
			ID string `json:"id"`
		}
		if !jsonPtr(r.body, &ch) || ch.ID == "" {
			return fmt.Errorf("criar canal %s: resposta inesperada", name)
		}
		s.mu.Lock()
		s.channels = append(s.channels, chanRef{ID: ch.ID, Name: name, Type: "text"})
		s.textChans = append(s.textChans, len(s.channels)-1)
		s.mu.Unlock()
		s.msgRings[ch.ID] = newMsgRing(2048)
	}
	return nil
}

// seedUserFull aplica a role member e preenche perfil, status, avatar,
// banner e configurações de um usuário.
func (s *state) seedUserFull(u *User, i int) error {
	ct := map[string]string{"Content-Type": "application/json"}

	r := s.c.do(s.owner, "POST", "/users/"+u.ID+"/roles", ct,
		jsonBody(map[string]string{"role_id": s.memberRole}))
	s.c.seedOp(r)
	if !r.ok() {
		return fmt.Errorf("%s: atribuir role: %s", u.Username, r.problem())
	}

	r = s.c.do(u, "PUT", "/users/"+u.ID, ct,
		jsonBody(map[string]string{
			"nickname":    s.dg.Nickname(i),
			"status":      s.dg.Words(2),
			"description": s.dg.Words(8),
		}))
	s.c.seedOp(r)
	if !r.ok() {
		return fmt.Errorf("%s: atualizar perfil: %s", u.Username, r.problem())
	}

	r = s.c.do(u, "PUT", "/users/"+u.ID+"/status", ct,
		jsonBody(map[string]string{"status_message": s.dg.Words(2)}))
	s.c.seedOp(r)
	if !r.ok() {
		return fmt.Errorf("%s: atualizar status: %s", u.Username, r.problem())
	}

	r = s.c.do(u, "PUT", "/users/"+u.ID+"/avatar", ct,
		jsonBody(map[string]string{"image_blob": base64.StdEncoding.EncodeToString(s.dg.PNG(128, 128))}))
	s.c.seedOp(r)
	if !r.ok() {
		return fmt.Errorf("%s: atualizar avatar: %s", u.Username, r.problem())
	}

	r = s.c.do(u, "PUT", "/users/"+u.ID+"/banner", ct,
		jsonBody(map[string]string{"image_blob": base64.StdEncoding.EncodeToString(s.dg.PNG(512, 128))}))
	s.c.seedOp(r)
	if !r.ok() {
		return fmt.Errorf("%s: atualizar banner: %s", u.Username, r.problem())
	}

	r = s.c.do(u, "PUT", "/users/settings", ct,
		jsonBody(map[string]any{"config": s.dg.Settings()}))
	s.c.seedOp(r)
	if !r.ok() {
		return fmt.Errorf("%s: atualizar settings: %s", u.Username, r.problem())
	}

	r = s.c.do(u, "GET", "/users/"+u.ID+"/profile", nil, nil)
	s.c.seedOp(r)
	if r.ok() {
		var p struct {
			BannerMedia *string `json:"banner_media"`
		}
		if jsonPtr(r.body, &p) && p.BannerMedia != nil && *p.BannerMedia != "" {
			s.mediaRing.add(*p.BannerMedia)
		}
	}
	return nil
}

// sendOne cria uma mensagem (opcionalmente com anexos) e devolve o ID.
func (s *state) sendOne(u *User, ch chanRef, content, replyTo string, atts [][2]string) (string, error) {
	id, r := s.doSend(u, ch, content, replyTo, atts)
	s.c.seedOp(r)
	if !r.ok() {
		return "", fmt.Errorf("POST /messages: %s", r.problem())
	}
	return id, nil
}

// chunkPlan descreve o volume de uma fatia de 10%.
type chunkPlan struct {
	users     int
	messages  int
	reactions int
	edits     int
	deletes   int
	pins      int
	emojis    int
	roles     int
}

func (s *state) seedUsers(p int, concurrency int) *errCollector {
	ec := &errCollector{}
	var wg sync.WaitGroup
	sem := make(chan struct{}, concurrency)
	for i := 0; i < p; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			idx := int(s.nextUser.Add(1))
			u, err := s.registerUser(idx)
			if err != nil {
				ec.add(err)
				return
			}
			if err := s.login(u); err != nil {
				ec.add(err)
				return
			}
			s.addUser(u)
			if err := s.seedUserFull(u, idx); err != nil {
				ec.add(err)
			}
		}()
	}
	wg.Wait()
	return ec
}

func (s *state) seedMessages(p int, concurrency int) *errCollector {
	ec := &errCollector{}
	var wg sync.WaitGroup
	sem := make(chan struct{}, concurrency)
	for i := 0; i < p; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			rng := rand.New(rand.NewSource(rand.Int63()))
			u := s.randomUser(rng)
			ch := s.randomTextChannel(rng)

			content := s.dg.Content()
			if rng.Float64() < 0.02 {
				content += " @" + s.randomUser(rng).ID
			}
			replyTo := ""
			if rng.Float64() < 0.05 {
				if m, ok := s.getLiveMsg(ch.ID, rng); ok {
					replyTo = m.ID
				}
			}
			var atts [][2]string
			if rng.Float64() < 0.10 {
				for f := 0; f < 1+rng.Intn(3); f++ {
					if rng.Float64() < 0.85 {
						name, data := s.dg.TextFile(1 + rng.Intn(20))
						atts = append(atts, [2]string{name, string(data)})
					} else {
						name := fmt.Sprintf("img_%06d.png", rng.Intn(1<<24))
						atts = append(atts, [2]string{name, string(s.dg.PNG(64, 64))})
					}
				}
			}
			if _, err := s.sendOne(u, ch, content, replyTo, atts); err != nil {
				ec.add(err)
			}
		}()
	}
	wg.Wait()
	return ec
}

func (s *state) seedReactions(p int, concurrency int) *errCollector {
	ec := &errCollector{}
	var wg sync.WaitGroup
	sem := make(chan struct{}, concurrency)
	for i := 0; i < p; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			rng := rand.New(rand.NewSource(rand.Int63()))
			ch := s.randomTextChannel(rng)
			m, ok := s.getLiveMsg(ch.ID, rng)
			if !ok {
				return
			}
			u := s.randomUser(rng)
			var body map[string]any
			s.mu.Lock()
			emojiCount := len(s.emojiIDs)
			var emojiID string
			if emojiCount > 0 {
				emojiID = s.emojiIDs[rng.Intn(emojiCount)]
			}
			s.mu.Unlock()
			if emojiID != "" && rng.Float64() < 0.3 {
				body = map[string]any{"emoji_id": emojiID}
			} else {
				body = map[string]any{"unicode": unicodeEmojis[rng.Intn(len(unicodeEmojis))]}
			}
			r := s.c.do(u, "POST", "/channels/"+ch.ID+"/messages/"+m.ID+"/reactions",
				map[string]string{"Content-Type": "application/json"}, jsonBody(body))
			s.c.seedOp(r)
			if !r.ok() && r.status != http.StatusConflict {
				ec.add(fmt.Errorf("POST reactions: %s", r.problem()))
			}
		}()
	}
	wg.Wait()
	return ec
}

func (s *state) seedEditsDeletes(pEdits, pDeletes int, concurrency int) *errCollector {
	ec := &errCollector{}
	var wg sync.WaitGroup
	sem := make(chan struct{}, concurrency)
	total := pEdits + pDeletes
	for i := 0; i < total; i++ {
		isEdit := i < pEdits
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			rng := rand.New(rand.NewSource(rand.Int63()))
			ch := s.randomTextChannel(rng)
			m, ok := s.getLiveMsg(ch.ID, rng)
			if !ok || m.Author == nil {
				return
			}
			if isEdit {
				r := s.c.do(m.Author, "PUT", "/messages/"+m.ID,
					map[string]string{"Content-Type": "application/json"},
					jsonBody(map[string]string{"content": s.dg.Content()}))
				s.c.seedOp(r)
				if !r.ok() && r.status != http.StatusNotFound {
					ec.add(fmt.Errorf("PUT /messages: %s", r.problem()))
				}
			} else {
				r := s.c.do(m.Author, "DELETE", "/messages/"+m.ID, nil, nil)
				s.c.seedOp(r)
				if r.ok() {
					s.markDeleted(m.ID, m.atts)
				} else if r.status != http.StatusNotFound {
					ec.add(fmt.Errorf("DELETE /messages: %s", r.problem()))
				}
			}
		}()
	}
	wg.Wait()
	return ec
}

// seedPins fixa mensagens recém-criadas e depois desfixa (saldo zero,
// respeitando o limite de 100 pins por canal).
func (s *state) seedPins(p int, concurrency int) *errCollector {
	ec := &errCollector{}
	var wg sync.WaitGroup
	sem := make(chan struct{}, concurrency)
	for i := 0; i < p; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			rng := rand.New(rand.NewSource(rand.Int63()))
			u := s.randomUser(rng)
			ch := s.randomTextChannel(rng)
			id, err := s.sendOne(u, ch, s.dg.Content(), "", nil)
			if err != nil {
				ec.add(err)
				return
			}
			r := s.c.do(u, "POST", "/channels/"+ch.ID+"/messages/"+id+"/pin", nil, nil)
			s.c.seedOp(r)
			if !r.ok() && r.status != http.StatusConflict {
				ec.add(fmt.Errorf("pin: %s", r.problem()))
				return
			}
			r = s.c.do(u, "DELETE", "/channels/"+ch.ID+"/messages/"+id+"/pin", nil, nil)
			s.c.seedOp(r)
			if !r.ok() && r.status != http.StatusNotFound {
				ec.add(fmt.Errorf("unpin: %s", r.problem()))
			}
		}()
	}
	wg.Wait()
	return ec
}

func (s *state) seedEmojis(p int, base int) error {
	ct := map[string]string{"Content-Type": "application/json"}
	for i := 0; i < p; i++ {
		name := fmt.Sprintf("st_%04d", base+i+1)
		r := s.c.do(s.owner, "POST", "/emojis", ct,
			jsonBody(map[string]any{
				"name":       name,
				"format":     "PNG",
				"image_blob": base64.StdEncoding.EncodeToString(s.dg.PNG(64, 64)),
			}))
		s.c.seedOp(r)
		if !r.ok() {
			return fmt.Errorf("criar emoji %s: %s", name, r.problem())
		}
		var e struct {
			ID string `json:"id"`
		}
		if !jsonPtr(r.body, &e) || e.ID == "" {
			return fmt.Errorf("criar emoji %s: resposta inesperada", name)
		}
		s.mu.Lock()
		s.emojiIDs = append(s.emojiIDs, e.ID)
		s.mu.Unlock()
	}
	return nil
}

func (s *state) seedRoles(p int, base int) error {
	ct := map[string]string{"Content-Type": "application/json"}
	for i := 0; i < p; i++ {
		name := fmt.Sprintf("st_role_%02d", base+i+1)
		r := s.c.do(s.owner, "POST", "/roles", ct,
			jsonBody(map[string]any{
				"name":  name,
				"color": fmt.Sprintf("#%06x", s.rng.Intn(1<<24)),
				"permissions": map[string]bool{
					"send_attachment":  s.rng.Intn(2) == 1,
					"pin_message":      s.rng.Intn(2) == 1,
					"everyone_message": s.rng.Intn(2) == 1,
				},
			}))
		s.c.seedOp(r)
		if !r.ok() {
			return fmt.Errorf("criar role %s: %s", name, r.problem())
		}
		var role struct {
			ID string `json:"id"`
		}
		if !jsonPtr(r.body, &role) || role.ID == "" {
			return fmt.Errorf("criar role %s: resposta inesperada", name)
		}
		s.mu.Lock()
		s.roles = append(s.roles, role.ID)
		s.mu.Unlock()

		rng := rand.New(rand.NewSource(rand.Int63()))
		for k := 0; k < 20; k++ {
			u := s.randomUser(rng)
			r := s.c.do(s.owner, "POST", "/users/"+u.ID+"/roles", ct,
				jsonBody(map[string]string{"role_id": role.ID}))
			s.c.seedOp(r)
		}
	}
	return nil
}

func (s *state) seedChannelUpdates() error {
	if len(s.textChans) < 3 {
		return nil
	}
	ct := map[string]string{"Content-Type": "application/json"}
	rng := rand.New(rand.NewSource(rand.Int63()))

	for i := 0; i < 2; i++ {
		idx := s.textChans[rng.Intn(len(s.textChans))]
		ch := s.channels[idx]
		r := s.c.do(s.owner, "PUT", "/channels/"+ch.ID, ct,
			jsonBody(map[string]string{"name": fmt.Sprintf("%s-v2", ch.Name), "topic": s.dg.Words(4)}))
		s.c.seedOp(r)
		if !r.ok() {
			return fmt.Errorf("renomear canal %s: %s", ch.Name, r.problem())
		}
	}

	r := s.c.do(s.owner, "GET", "/channels", nil, nil)
	s.c.seedOp(r)
	if !r.ok() {
		return fmt.Errorf("listar canais: %s", r.problem())
	}
	var list struct {
		Channels []struct {
			ID       string `json:"id"`
			Position int    `json:"position"`
		} `json:"channels"`
	}
	if !jsonPtr(r.body, &list) {
		return fmt.Errorf("listar canais: resposta inesperada")
	}
	pos := make(map[string]int, len(list.Channels))
	for _, ch := range list.Channels {
		pos[ch.ID] = ch.Position
	}
	n := len(list.Channels)
	for i := 0; i < 2; i++ {
		idx := s.textChans[rng.Intn(len(s.textChans))]
		ch := s.channels[idx]
		old := pos[ch.ID]
		new := 1 + rng.Intn(n)
		if new == old {
			new = (new % n) + 1
		}
		r = s.c.do(s.owner, "PUT", "/channels/"+ch.ID+"/change_position", ct,
			jsonBody(map[string]int{"old_position": old, "new_position": new}))
		s.c.seedOp(r)
		if !r.ok() {
			return fmt.Errorf("reposicionar canal %s: %s", ch.Name, r.problem())
		}
		// Acompanha o deslocamento feito pelo backend no mapa local.
		if new > old {
			for id, p := range pos {
				if id != ch.ID && p > old && p <= new {
					pos[id] = p - 1
				}
			}
		} else {
			for id, p := range pos {
				if id != ch.ID && p >= new && p < old {
					pos[id] = p + 1
				}
			}
		}
		pos[ch.ID] = new
	}
	return nil
}

// seedChunk executa uma fatia de 10% do volume total e devolve o primeiro
// erro (se houver) e a contagem total de erros.
func (s *state) seedChunk(n int, p chunkPlan, userConc, msgConc int) (error, int) {
	var firstErr error
	totalErrs := 0
	consider := func(ec *errCollector, step string) {
		if ec.err != nil && firstErr == nil {
			firstErr = fmt.Errorf("%s: %w", step, ec.err)
		}
		totalErrs += ec.count()
	}
	if p.users > 0 {
		consider(s.seedUsers(p.users, userConc), "usuários")
	}
	if p.messages > 0 {
		consider(s.seedMessages(p.messages, msgConc), "mensagens")
	}
	if p.reactions > 0 {
		consider(s.seedReactions(p.reactions, msgConc), "reações")
	}
	if p.edits > 0 || p.deletes > 0 {
		consider(s.seedEditsDeletes(p.edits, p.deletes, msgConc), "edições/exclusões")
	}
	if p.pins > 0 {
		consider(s.seedPins(p.pins, msgConc), "pins")
	}
	if p.emojis > 0 {
		if err := s.seedEmojis(p.emojis, (n-1)*p.emojis); err != nil {
			return fmt.Errorf("emojis: %w", err), totalErrs
		}
	}
	if p.roles > 0 {
		if err := s.seedRoles(p.roles, n); err != nil {
			return fmt.Errorf("roles: %w", err), totalErrs
		}
	}
	if err := s.seedChannelUpdates(); err != nil {
		return fmt.Errorf("canais: %w", err), totalErrs
	}
	return firstErr, totalErrs
}
