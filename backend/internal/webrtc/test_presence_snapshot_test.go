package webrtc

import (
	"reflect"
	"testing"
)

func TestUserVoiceChannelsSnapshot(t *testing.T) {
	m := &Manager{
		userRooms: map[string]map[string]struct{}{
			"user": {"channel-b": {}, "channel-a": {}},
		},
	}
	got := m.UserVoiceChannels("user")
	want := []string{"channel-a", "channel-b"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("UserVoiceChannels = %v, esperado %v", got, want)
	}
	empty := m.UserVoiceChannels("missing")
	if empty == nil || len(empty) != 0 {
		t.Fatalf("usuário sem voice deve retornar [], obtive %#v", empty)
	}
}
