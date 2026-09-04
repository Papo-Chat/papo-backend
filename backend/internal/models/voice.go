package models

// VoiceState é o estado efêmero de um usuário dentro de uma sala de voz
// (o que a UI precisa para desenhar a call).
type VoiceState struct {
	UserID        string `json:"user_id"`
	Muted         bool   `json:"muted"`
	CameraOn      bool   `json:"camera_on"`
	ScreenSharing bool   `json:"screen_sharing"`
}

// ICEServer é o formato do browser (RTCIceServer): o frontend passa direto
// para new RTCPeerConnection({ iceServers }). Username/Credential só existem
// em servidores TURN (credencial efêmera RFC 5389, por usuário).
type ICEServer struct {
	URLs       []string `json:"urls"`
	Username   string   `json:"username,omitempty"`
	Credential string   `json:"credential,omitempty"`
}
