package main

import (
	"bytes"
	"math/rand"
	"mime/multipart"
	"sync"
	"sync/atomic"
	"time"
)

// Labels de endpoint usados no relatório.
const (
	epListChannels       = "GET /channels"
	epListMessages       = "GET /channels/:id/messages"
	epPollMessages       = "GET /channels/:id/messages?since"
	epSendMessage        = "POST /messages"
	epEditMessage        = "PUT /messages/:id"
	epDeleteMessage      = "DELETE /messages/:id"
	epAddReaction        = "POST /messages/:id/reactions"
	epListReactions      = "GET /messages/:id/reactions"
	epRemoveReaction     = "DELETE /messages/:id/reactions"
	epPinMessage         = "POST /messages/:id/pin"
	epUnpinMessage       = "DELETE /messages/:id/pin"
	epListPinned         = "GET /channels/:id/pinned"
	epListUsers          = "GET /users"
	epGetProfile         = "GET /users/:id/profile"
	epProfileBatch       = "POST /users/profile_batch"
	epUpdateSettings     = "PUT /users/settings"
	epUpdateProfile      = "PUT /users/:id"
	epUpdateStatus       = "PUT /users/:id/status"
	epUpdateAvatar       = "PUT /users/:id/avatar"
	epUpdateBanner       = "PUT /users/:id/banner"
	epListEmojis         = "GET /emojis"
	epSearch             = "POST /search"
	epDownloadAttachment = "GET /attachments/:id"
	epDownloadMedia      = "GET /media/:sha"
	epGetServer          = "GET /server"
	epWhoami             = "GET /auth/whoami"
	epRefresh            = "POST /auth/refresh"
	epConnectedDevices   = "GET /auth/connected_devices"
)

// Kinds de tarefa. Tarefas compostas gravam mais de um endpoint
// (ex.: editar = enviar + PUT), exatamente como o frontend faz.
const (
	tListChannels = iota
	tListMessages
	tPollMessages
	tSendText
	tSendAttachment
	tEditMessage
	tDeleteMessage
	tAddReaction
	tListReactions
	tRemoveReaction
	tPinUnpin
	tListPinned
	tListUsers
	tGetProfile
	tProfileBatch
	tUpdateSettings
	tUpdateProfile
	tUpdateStatus
	tUpdateAvatar
	tUpdateBanner
	tListEmojis
	tSearch
	tDownloadAttachment
	tDownloadMedia
	tGetServer
	tWhoami
	tRefresh
	tConnectedDevices
)

// epWeights define a mistura de tarefas de cada fase (proporcional ao uso
// do frontend). A soma é 93.
var epWeights = []struct {
	kind int
	w    int
}{
	{tListChannels, 8},
	{tListMessages, 10},
	{tPollMessages, 10},
	{tSendText, 8},
	{tSendAttachment, 2},
	{tEditMessage, 2},
	{tDeleteMessage, 1},
	{tAddReaction, 4},
	{tListReactions, 2},
	{tRemoveReaction, 2},
	{tPinUnpin, 1},
	{tListPinned, 2},
	{tListUsers, 4},
	{tGetProfile, 4},
	{tProfileBatch, 2},
	{tUpdateSettings, 1},
	{tUpdateProfile, 1},
	{tUpdateStatus, 1},
	{tUpdateAvatar, 1},
	{tUpdateBanner, 1},
	{tListEmojis, 4},
	{tSearch, 4},
	{tDownloadAttachment, 4},
	{tDownloadMedia, 4},
	{tGetServer, 2},
	{tWhoami, 4},
	{tRefresh, 2},
	{tConnectedDevices, 2},
}

// doSend envia uma mensagem e atualiza os anéis de amostra.
func (s *state) doSend(u *User, ch chanRef, content, replyTo string, atts [][2]string) (string, resp) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("channel_id", ch.ID)
	if content != "" {
		_ = mw.WriteField("content", content)
	}
	if replyTo != "" {
		_ = mw.WriteField("reply_to", replyTo)
	}
	for _, a := range atts {
		fw, err := mw.CreateFormFile("attachments", a[0])
		if err != nil {
			return "", resp{err: err}
		}
		if _, err := fw.Write([]byte(a[1])); err != nil {
			return "", resp{err: err}
		}
	}
	if err := mw.Close(); err != nil {
		return "", resp{err: err}
	}
	r := s.c.do(u, "POST", "/messages", map[string]string{"Content-Type": mw.FormDataContentType()}, buf.Bytes())
	if !r.ok() {
		return "", r
	}
	var m struct {
		ID          string `json:"id"`
		Attachments []struct {
			ID string `json:"id"`
		} `json:"attachments"`
	}
	if !jsonPtr(r.body, &m) || m.ID == "" {
		return "", r
	}
	var attIDs []string
	for _, a := range m.Attachments {
		if a.ID != "" {
			attIDs = append(attIDs, a.ID)
			s.attRing.add(a.ID)
		}
	}
	s.msgRings[ch.ID].add(msgRef{ID: m.ID, Author: u, atts: attIDs})
	return m.ID, r
}

// taskSend envia uma mensagem gravando o endpoint POST /messages.
func (s *state) taskSend(u *User, ch chanRef, content string, atts [][2]string) string {
	id, r := s.doSend(u, ch, content, "", atts)
	s.st.Record(epSendMessage, r.dur, r.status)
	if !r.ok() {
		return ""
	}
	return id
}

func (s *state) reactionBody(rng *rand.Rand) map[string]any {
	s.mu.Lock()
	var emojiID string
	if len(s.emojiIDs) > 0 {
		emojiID = s.emojiIDs[rng.Intn(len(s.emojiIDs))]
	}
	s.mu.Unlock()
	if emojiID != "" && rng.Float64() < 0.3 {
		return map[string]any{"emoji_id": emojiID}
	}
	return map[string]any{"unicode": unicodeEmojis[rng.Intn(len(unicodeEmojis))]}
}

// execTask executa uma tarefa de medição.
func (s *state) execTask(kind int, rng *rand.Rand) {
	ct := map[string]string{"Content-Type": "application/json"}
	switch kind {
	case tListChannels:
		u := s.randomUser(rng)
		r := s.c.do(u, "GET", "/channels", nil, nil)
		s.st.Record(epListChannels, r.dur, r.status)

	case tListMessages:
		u := s.randomUser(rng)
		ch := s.randomTextChannel(rng)
		r := s.c.do(u, "GET", "/channels/"+ch.ID+"/messages", nil, nil)
		s.st.Record(epListMessages, r.dur, r.status)

	case tPollMessages:
		u := s.randomUser(rng)
		ch := s.randomTextChannel(rng)
		since := time.Now().Add(-15 * time.Second).Format(time.RFC3339)
		r := s.c.do(u, "GET", "/channels/"+ch.ID+"/messages?since="+since, nil, nil)
		s.st.Record(epPollMessages, r.dur, r.status)

	case tSendText:
		u := s.randomUser(rng)
		ch := s.randomTextChannel(rng)
		s.taskSend(u, ch, s.dg.Content(), nil)

	case tSendAttachment:
		u := s.randomUser(rng)
		ch := s.randomTextChannel(rng)
		name, data := s.dg.TextFile(2)
		s.taskSend(u, ch, s.dg.Content(), [][2]string{{name, string(data)}})

	case tEditMessage:
		u := s.randomUser(rng)
		ch := s.randomTextChannel(rng)
		id := s.taskSend(u, ch, s.dg.Content(), nil)
		if id == "" {
			return
		}
		r := s.c.do(u, "PUT", "/messages/"+id, ct,
			jsonBody(map[string]string{"content": s.dg.Content()}))
		s.st.Record(epEditMessage, r.dur, r.status)

	case tDeleteMessage:
		u := s.randomUser(rng)
		ch := s.randomTextChannel(rng)
		id := s.taskSend(u, ch, s.dg.Content(), nil)
		if id == "" {
			return
		}
		r := s.c.do(u, "DELETE", "/messages/"+id, nil, nil)
		s.st.Record(epDeleteMessage, r.dur, r.status)
		if r.ok() {
			s.markDeleted(id, nil)
		}

	case tAddReaction:
		ch := s.randomTextChannel(rng)
		m, ok := s.getLiveMsg(ch.ID, rng)
		if !ok {
			return
		}
		u := s.randomUser(rng)
		r := s.c.do(u, "POST", "/channels/"+ch.ID+"/messages/"+m.ID+"/reactions", ct,
			jsonBody(s.reactionBody(rng)))
		s.st.Record(epAddReaction, r.dur, r.status)

	case tListReactions:
		ch := s.randomTextChannel(rng)
		m, ok := s.getLiveMsg(ch.ID, rng)
		if !ok {
			return
		}
		u := s.randomUser(rng)
		r := s.c.do(u, "GET", "/channels/"+ch.ID+"/messages/"+m.ID+"/reactions", nil, nil)
		s.st.Record(epListReactions, r.dur, r.status)

	case tRemoveReaction:
		ch := s.randomTextChannel(rng)
		m, ok := s.getLiveMsg(ch.ID, rng)
		if !ok {
			return
		}
		u := s.randomUser(rng)
		body := s.reactionBody(rng)
		r := s.c.do(u, "POST", "/channels/"+ch.ID+"/messages/"+m.ID+"/reactions", ct, jsonBody(body))
		s.st.Record(epAddReaction, r.dur, r.status)
		r = s.c.do(u, "DELETE", "/channels/"+ch.ID+"/messages/"+m.ID+"/reactions", ct, jsonBody(body))
		s.st.Record(epRemoveReaction, r.dur, r.status)

	case tPinUnpin:
		u := s.randomUser(rng)
		ch := s.randomTextChannel(rng)
		id := s.taskSend(u, ch, s.dg.Content(), nil)
		if id == "" {
			return
		}
		r := s.c.do(u, "POST", "/channels/"+ch.ID+"/messages/"+id+"/pin", nil, nil)
		s.st.Record(epPinMessage, r.dur, r.status)
		r = s.c.do(u, "DELETE", "/channels/"+ch.ID+"/messages/"+id+"/pin", nil, nil)
		s.st.Record(epUnpinMessage, r.dur, r.status)

	case tListPinned:
		u := s.randomUser(rng)
		ch := s.randomTextChannel(rng)
		r := s.c.do(u, "GET", "/channels/"+ch.ID+"/pinned", nil, nil)
		s.st.Record(epListPinned, r.dur, r.status)

	case tListUsers:
		u := s.randomUser(rng)
		r := s.c.do(u, "GET", "/users", nil, nil)
		s.st.Record(epListUsers, r.dur, r.status)

	case tGetProfile:
		u := s.randomUser(rng)
		target := s.randomUser(rng)
		r := s.c.do(u, "GET", "/users/"+target.ID+"/profile", nil, nil)
		s.st.Record(epGetProfile, r.dur, r.status)
		if r.ok() {
			var p struct {
				BannerMedia *string `json:"banner_media"`
			}
			if jsonPtr(r.body, &p) && p.BannerMedia != nil && *p.BannerMedia != "" {
				s.mediaRing.add(*p.BannerMedia)
			}
		}

	case tProfileBatch:
		u := s.randomUser(rng)
		ids := make([]string, 0, 10)
		for i := 0; i < 10; i++ {
			ids = append(ids, s.randomUser(rng).ID)
		}
		r := s.c.do(u, "POST", "/users/profile_batch", ct, jsonBody(map[string]any{"ids": ids}))
		s.st.Record(epProfileBatch, r.dur, r.status)

	case tUpdateSettings:
		u := s.randomUser(rng)
		r := s.c.do(u, "PUT", "/users/settings", ct,
			jsonBody(map[string]any{"config": s.dg.Settings()}))
		s.st.Record(epUpdateSettings, r.dur, r.status)

	case tUpdateProfile:
		u := s.randomUser(rng)
		r := s.c.do(u, "PUT", "/users/"+u.ID, ct,
			jsonBody(map[string]string{
				"nickname":    u.Username + " nick",
				"status":      s.dg.Words(2),
				"description": s.dg.Words(8),
			}))
		s.st.Record(epUpdateProfile, r.dur, r.status)

	case tUpdateStatus:
		u := s.randomUser(rng)
		r := s.c.do(u, "PUT", "/users/"+u.ID+"/status", ct,
			jsonBody(map[string]string{"status_message": s.dg.Words(2)}))
		s.st.Record(epUpdateStatus, r.dur, r.status)

	case tUpdateAvatar:
		u := s.randomUser(rng)
		r := s.c.do(u, "PUT", "/users/"+u.ID+"/avatar", ct,
			jsonBody(map[string]string{"image_blob": b64(s.dg.PNG(128, 128))}))
		s.st.Record(epUpdateAvatar, r.dur, r.status)

	case tUpdateBanner:
		u := s.randomUser(rng)
		r := s.c.do(u, "PUT", "/users/"+u.ID+"/banner", ct,
			jsonBody(map[string]string{"image_blob": b64(s.dg.PNG(512, 128))}))
		s.st.Record(epUpdateBanner, r.dur, r.status)

	case tListEmojis:
		u := s.randomUser(rng)
		r := s.c.do(u, "GET", "/emojis", nil, nil)
		s.st.Record(epListEmojis, r.dur, r.status)

	case tSearch:
		u := s.randomUser(rng)
		r := s.c.do(u, "POST", "/search", ct,
			jsonBody(map[string]string{"text": words[rng.Intn(len(words))], "order": "desc"}))
		s.st.Record(epSearch, r.dur, r.status)

	case tDownloadAttachment:
		att, ok := s.getLiveAtt(rng)
		if !ok {
			return
		}
		u := s.randomUser(rng)
		r := s.c.do(u, "GET", "/attachments/"+att, nil, nil)
		s.st.Record(epDownloadAttachment, r.dur, r.status)

	case tDownloadMedia:
		sha, ok := s.mediaRing.get(rng)
		if !ok {
			return
		}
		u := s.randomUser(rng)
		r := s.c.do(u, "GET", "/media/"+sha, nil, nil)
		s.st.Record(epDownloadMedia, r.dur, r.status)

	case tGetServer:
		u := s.randomUser(rng)
		r := s.c.do(u, "GET", "/server", nil, nil)
		s.st.Record(epGetServer, r.dur, r.status)

	case tWhoami:
		u := s.randomUser(rng)
		r := s.c.do(u, "GET", "/auth/whoami", nil, nil)
		s.st.Record(epWhoami, r.dur, r.status)

	case tRefresh:
		u := s.randomUser(rng)
		r := s.c.do(u, "POST", "/auth/refresh", nil, nil)
		s.st.Record(epRefresh, r.dur, r.status)

	case tConnectedDevices:
		u := s.randomUser(rng)
		r := s.c.do(u, "GET", "/auth/connected_devices", nil, nil)
		s.st.Record(epConnectedDevices, r.dur, r.status)
	}
}

// runPhase executa a passada de medição com a mesma mistura de tarefas.
func (s *state) runPhase(total, concurrency, phaseIdx int, label string) {
	s.st.BeginPhase(phaseIdx, label)
	defer s.st.EndPhase()

	sumW := 0
	for _, e := range epWeights {
		sumW += e.w
	}
	tasks := make([]int, 0, total)
	for _, e := range epWeights {
		count := int(float64(e.w)/float64(sumW)*float64(total) + 0.5)
		for i := 0; i < count; i++ {
			tasks = append(tasks, e.kind)
		}
	}
	rng := rand.New(rand.NewSource(int64(phaseIdx) * 2654435761))
	rng.Shuffle(len(tasks), func(i, j int) { tasks[i], tasks[j] = tasks[j], tasks[i] })

	var next atomic.Int64
	var wg sync.WaitGroup
	for w := 0; w < concurrency; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			r := rand.New(rand.NewSource(int64(phaseIdx)*7919 + int64(w)*104729 + 12345))
			for {
				i := int(next.Add(1)) - 1
				if i >= len(tasks) {
					return
				}
				s.execTask(tasks[i], r)
			}
		}(w)
	}
	wg.Wait()
}
