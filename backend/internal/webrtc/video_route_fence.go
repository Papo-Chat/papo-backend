package webrtc

import (
	"encoding/binary"
	"errors"

	"github.com/pion/rtp"
	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v4"
)

const videoRouteExtensionURI = "urn:papo:rtp-hdrext:video-route-v1"

var (
	errVideoRouteIDsExhausted = errors.New("IDs de atribuição de vídeo esgotados")
	errVideoRouteUnavailable  = errors.New("extensão da rota de vídeo não negociada")
)

// Os parâmetros do sender do Pion agregam extensões por tipo de mídia.
// Confira também o offer e o answer do MID daquele sender: outro MID de
// vídeo não pode optar em nome deste slot.
func negotiatedVideoRouteExtensions(pc *webrtc.PeerConnection, offer, answer string) map[*webrtc.RTPSender]uint8 {
	receive := videoRouteExtMaps(offer, true)
	send := videoRouteExtMaps(answer, false)
	result := make(map[*webrtc.RTPSender]uint8)
	for _, tr := range pc.GetTransceivers() {
		sender := tr.Sender()
		id := send[tr.Mid()]
		if sender == nil || id == 0 || receive[tr.Mid()] != id {
			continue
		}
		for _, ext := range sender.GetParameters().HeaderExtensions {
			if ext.URI == videoRouteExtensionURI && ext.ID == int(id) {
				result[sender] = id
				break
			}
		}
	}
	return result
}

func videoRouteExtMaps(raw string, receive bool) map[string]uint8 {
	var desc sdp.SessionDescription
	if err := desc.Unmarshal([]byte(raw)); err != nil {
		return nil
	}
	result := make(map[string]uint8)
	for _, media := range desc.MediaDescriptions {
		if media.MediaName.Media != "video" || media.MediaName.Port.Value == 0 {
			continue
		}
		mid, ok := media.Attribute("mid")
		if !ok {
			continue
		}
		_, inactive := media.Attribute("inactive")
		_, sendOnly := media.Attribute("sendonly")
		_, recvOnly := media.Attribute("recvonly")
		if inactive || (receive && sendOnly) || (!receive && recvOnly) {
			continue
		}
		for _, attr := range media.Attributes {
			if attr.Key != "extmap" {
				continue
			}
			var ext sdp.ExtMap
			if err := ext.Unmarshal("extmap:" + attr.Value); err != nil || ext.URI.String() != videoRouteExtensionURI {
				continue
			}
			if ext.Value < 1 || ext.Value > 14 || ext.Direction == sdp.DirectionInactive ||
				(receive && ext.Direction == sdp.DirectionSendOnly) || (!receive && ext.Direction == sdp.DirectionRecvOnly) {
				continue
			}
			result[mid] = uint8(ext.Value)
		}
	}
	return result
}

func (p *Peer) updateVideoRouteExtensions(negotiated map[*webrtc.RTPSender]uint8) []any {
	p.mu.Lock()
	defer p.mu.Unlock()
	before := p.slotRoutesLocked()
	for _, s := range p.videoSlots {
		// Um slot legado pode ter RTP sem marca em trânsito. Não é seguro
		// promover esse slot no meio da chamada.
		if !s.routeRequired {
			continue
		}
		id := negotiated[s.sender]
		if id == s.routeExtID {
			continue
		}
		s.routeExtID = id
		if s.fwd != nil {
			s.assignWithFanout(s.owner, s.kind, s.src, s.fwd.fanout)
		}
	}
	return p.routeUpdatesLocked(before)
}

// Um pacote que já passou pelo tradutor pode sair após a troca de slot.
// A marca vem do forwarder antigo, nunca do ocupante atual do slot.
func (f *forwarder) writeRTP(pkt *rtp.Packet) error {
	if err := f.markVideoRoute(pkt); err != nil {
		return err
	}
	return f.slot.local.WriteRTP(pkt)
}

func (f *forwarder) markVideoRoute(pkt *rtp.Packet) error {
	if f.routeExtID == 0 {
		if f.routeRequired {
			// Após downgrade, o cliente ainda pode ter a rota anterior.
			// Nunca passe extensões arbitrárias do publisher para esse slot.
			return errVideoRouteUnavailable
		}
		return nil
	}
	if f.routeID == 0 {
		return errVideoRouteIDsExhausted
	}
	// Descarte o slice inteiro: IDs do publisher podem colidir com os do
	// subscriber, e outros forwarders ainda podem estar lendo o mesmo slice.
	pkt.Extensions = nil
	pkt.Extension = false
	var payload [8]byte
	binary.BigEndian.PutUint64(payload[:], f.routeID)
	return pkt.SetExtension(f.routeExtID, payload[:])
}
