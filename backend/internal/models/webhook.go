package models

import "time"

// Webhook is an independently authenticated sender, limited to a single channel.
// Its secret hash is never serialized to clients.
type Webhook struct {
    ID string `json:"id"`
    ChannelID string `json:"channel_id"`
    Name string `json:"name"`
    CreatedBy string `json:"created_by"`
    CreatedAt time.Time `json:"created_at"`
    RevokedAt *time.Time `json:"revoked_at,omitempty"`
}

// ExternalAuthor is a display-only identity attributed to an incoming webhook.
// It is NOT a Papo user and never determines authorization.
type ExternalAuthor struct {
    Source string `json:"source"`
    ID string `json:"id"`
    Name string `json:"name"`
    AvatarURL string `json:"avatar_url,omitempty"`
    ProfileURL string `json:"profile_url,omitempty"`
}
