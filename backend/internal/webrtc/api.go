package webrtc

import (
	"fmt"

	"papo/internal/config"

	"github.com/pion/interceptor"
	"github.com/pion/interceptor/pkg/cc"
	"github.com/pion/interceptor/pkg/gcc"
	"github.com/pion/interceptor/pkg/pacing"
	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v4"
)

// webrtcAPI encapsula a API do pion compartilhada por todas as
// PeerConnections (MediaEngine com o codec único da sala + registry de
// interceptors). Compartilhada: o codec de vídeo é global (VOICE_VIDEO_CODEC)
// e o registry cria o interceptor por PC (Build(id)).
type webrtcAPI struct {
	*webrtc.API
	videoCodec webrtc.RTPCodecParameters
}

// videoCodecCapability mapeia o nome do codec (VOICE_VIDEO_CODEC) para a
// capacidade RTP. vp8 é o default (denominador comum de send+receive em
// Chrome/Firefox/Safari — D6).
func videoCodecCapability(name string) (webrtc.RTPCodecParameters, bool) {
	switch name {
	case "vp8":
		return webrtc.RTPCodecParameters{
			RTPCodecCapability: webrtc.RTPCodecCapability{
				MimeType:  webrtc.MimeTypeVP8,
				ClockRate: 90000,
				Channels:  0,
			},
		}, true
	case "vp9":
		return webrtc.RTPCodecParameters{
			RTPCodecCapability: webrtc.RTPCodecCapability{
				MimeType:  webrtc.MimeTypeVP9,
				ClockRate: 90000,
				Channels:  0,
			},
		}, true
	case "h264":
		return webrtc.RTPCodecParameters{
			RTPCodecCapability: webrtc.RTPCodecCapability{
				MimeType:  webrtc.MimeTypeH264,
				ClockRate: 90000,
				Channels:  0,
				SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;" +
					"profile-level-id=4d0032",
			},
		}, true
	default:
		return webrtc.RTPCodecParameters{}, false
	}
}

// newSharedAPI monta a API compartilhada (D6/D9):
//   - MediaEngine: opus (áudio) + codec de vídeo da sala (D6);
//   - header extensions: audio-level (RFC 6464, detecção de active speaker) e
//     abs-send-time (insumo do GCC); o TWCC já vem dos defaults;
//   - interceptors: NACK/RTX (ambos sentidos) + RTCP reports + TWCC + stats
//     (RegisterDefaultInterceptors), GCC (SendSideBWE) alimentando o pacer
//     (limita o bitrate enviado a cada subscriber) e o interceptor de
//     audio-level (RFC 6464, não existe no pion — active_speaker.go).
func newSharedAPI(cfg *config.Config, m *Manager) (*webrtcAPI, error) {
	videoCodec, ok := videoCodecCapability(cfg.VoiceVideoCodec)
	if !ok {
		return nil, fmt.Errorf("codec de vídeo %q não suportado (use vp8, vp9 ou h264)", cfg.VoiceVideoCodec)
	}

	mediaEngine := &webrtc.MediaEngine{}
	if err := mediaEngine.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:  webrtc.MimeTypeOpus,
			ClockRate: 48000,
			Channels:  2,
		},
	}, webrtc.RTPCodecTypeAudio); err != nil {
		return nil, err
	}
	if err := mediaEngine.RegisterCodec(videoCodec, webrtc.RTPCodecTypeVideo); err != nil {
		return nil, err
	}
	// Audio-level (RFC 6464) nas m-lines de áudio: alimenta a detecção de
	// active speaker (o browser envia na track do getUserMedia).
	if err := mediaEngine.RegisterHeaderExtension(
		webrtc.RTPHeaderExtensionCapability{URI: sdp.AudioLevelURI}, webrtc.RTPCodecTypeAudio,
	); err != nil {
		return nil, err
	}
	// abs-send-time nas m-lines de vídeo: insumo da estimativa de banda GCC (D9).
	if err := mediaEngine.RegisterHeaderExtension(
		webrtc.RTPHeaderExtensionCapability{URI: sdp.ABSSendTimeURI}, webrtc.RTPCodecTypeVideo,
	); err != nil {
		return nil, err
	}

	registry := &interceptor.Registry{}

	// GCC: estima a banda por PC a partir do feedback TWCC do subscriber e
	// alimenta o pacer (o pacer limita o bitrate efetivamente enviado).
	estimatorFactory, err := cc.NewInterceptor(func() (cc.BandwidthEstimator, error) {
		return gcc.NewSendSideBWE()
	})
	if err != nil {
		return nil, err
	}
	pacer := pacing.NewInterceptor()
	estimatorFactory.OnNewPeerConnection(func(id string, estimator cc.BandwidthEstimator) {
		estimator.OnTargetBitrateChange(func(bitrate int) {
			if bitrate > 0 {
				pacer.SetRate(id, bitrate)
			}
		})
	})
	registry.Add(estimatorFactory)

	// Defaults: NACK (send+recv), RTCP reports, simulcast ext headers, stats
	// e TWCC (header extension + feedback + sender).
	if err := webrtc.RegisterDefaultInterceptors(mediaEngine, registry); err != nil {
		return nil, err
	}

	// Pacer por último: innermost no caminho de saída (vê o stream final,
	// já com RTX do NACK) e aplica o limite de bitrate do GCC.
	registry.Add(pacer)

	// Audio-level (RFC 6464): observa os pacotes de áudio recebidos e
	// reporta o nível por SSRC (active_speaker.go).
	registry.Add(newAudioLevelFactory(m))

	return &webrtcAPI{
		API:        webrtc.NewAPI(webrtc.WithMediaEngine(mediaEngine), webrtc.WithInterceptorRegistry(registry)),
		videoCodec: videoCodec,
	}, nil
}

// iceServers monta os ICE servers do pion para as PeerConnections do
// servidor: STUN + TURN apenas como URLs (a credencial efêmera do usuário é
// do cliente, via GET /voice/ice-servers — D14).
func (m *Manager) iceServers() []webrtc.ICEServer {
	servers := make([]webrtc.ICEServer, 0, 2)
	if len(m.cfg.STUNURLs) > 0 {
		servers = append(servers, webrtc.ICEServer{URLs: m.cfg.STUNURLs})
	}
	if len(m.cfg.TURNURLs) > 0 {
		servers = append(servers, webrtc.ICEServer{URLs: m.cfg.TURNURLs})
	}
	return servers
}
