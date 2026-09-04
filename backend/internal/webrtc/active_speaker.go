package webrtc

import (
	"strings"

	"github.com/pion/interceptor"
	"github.com/pion/rtp"
	"github.com/pion/sdp/v3"
)

// newAudioLevelFactory cria o interceptor de active speaker (RFC 6464):
// observa os pacotes de áudio recebidos, extrai o header extension
// ssrc-audio-level e reporta o nível (dBFS) para a sala do SSRC. O pion não
// tem este interceptor (seção 5.8 do plano).
func newAudioLevelFactory(m *Manager) interceptor.Factory {
	return &audioLevelFactory{m: m}
}

type audioLevelFactory struct {
	m *Manager
}

func (f *audioLevelFactory) NewInterceptor(id string) (interceptor.Interceptor, error) {
	return &audioLevelInterceptor{m: f.m}, nil
}

// audioLevelInterceptor implementa o interceptor (só observa o áudio
// recebido; os demais binders são pass-through).
type audioLevelInterceptor struct {
	m *Manager
}

func (i *audioLevelInterceptor) BindRTCPReader(reader interceptor.RTCPReader) interceptor.RTCPReader {
	return reader
}

func (i *audioLevelInterceptor) BindRTCPWriter(writer interceptor.RTCPWriter) interceptor.RTCPWriter {
	return writer
}

func (i *audioLevelInterceptor) BindLocalStream(info *interceptor.StreamInfo, writer interceptor.RTPWriter) interceptor.RTPWriter {
	return writer
}

func (i *audioLevelInterceptor) UnbindLocalStream(info *interceptor.StreamInfo) {}

func (i *audioLevelInterceptor) BindRemoteStream(info *interceptor.StreamInfo, reader interceptor.RTPReader) interceptor.RTPReader {
	if info == nil || !strings.HasPrefix(info.MimeType, "audio") {
		return reader
	}
	extID := audioLevelExtID(info)
	if extID == 0 {
		return reader
	}
	return &audioLevelReader{m: i.m, ssrc: info.SSRC, extID: extID, reader: reader}
}

func (i *audioLevelInterceptor) UnbindRemoteStream(info *interceptor.StreamInfo) {}

func (i *audioLevelInterceptor) Close() error { return nil }

// audioLevelExtID retorna o ID do header extension ssrc-audio-level (0 quando
// não negociado na m-line).
func audioLevelExtID(info *interceptor.StreamInfo) uint8 {
	for _, e := range info.RTPHeaderExtensions {
		if e.URI == sdp.AudioLevelURI {
			return uint8(e.ID)
		}
	}
	return 0
}

// audioLevelReader observa os pacotes de áudio e reporta o nível da m-line.
type audioLevelReader struct {
	m      *Manager
	ssrc   uint32
	extID  uint8
	reader interceptor.RTPReader
}

func (r *audioLevelReader) Read(b []byte, attrs interceptor.Attributes) (int, interceptor.Attributes, error) {
	n, attrs, err := r.reader.Read(b, attrs)
	if err != nil || n == 0 {
		return n, attrs, err
	}
	var header rtp.Header
	if _, uerr := header.Unmarshal(b[:n]); uerr != nil {
		return n, attrs, err
	}
	ext := header.GetExtension(r.extID)
	if len(ext) == 0 {
		return n, attrs, err
	}
	var al rtp.AudioLevelExtension
	if uerr := al.Unmarshal(ext); uerr != nil {
		return n, attrs, err
	}
	r.m.reportAudioLevel(r.ssrc, audioLevelToDBFS(al.Level))
	return n, attrs, err
}

// audioLevelToDBFS converte o nível da extensão ssrc-audio-level (RFC 6464)
// para dBFS. O valor 0-127 do RFC já é o dBFS negado: 0 = 0 dBFS (mais alto)
// e 127 = -127 dBFS (silêncio). A conversão anterior tratava 0 como silêncio
// e 127 como máximo (invertido).
func audioLevelToDBFS(level uint8) float64 {
	return -float64(level)
}
