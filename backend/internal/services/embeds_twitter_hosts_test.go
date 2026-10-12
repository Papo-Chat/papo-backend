package services

import (
    "net/url"
    "testing"
)

func TestTwitterStatusIDHandlesKnownEmbedMirrors(t *testing.T) {
    const statusID = "2109376514688966914"
    cases := []struct {
        raw string
        want string
        ok bool
    }{
        {"https://vxtwitter.com/Lord_PSMG/status/" + statusID + "?s=20", statusID, true},
        {"https://www.vxtwitter.com/Lord_PSMG/status/" + statusID, statusID, true},
        {"https://fixvx.com/Lord_PSMG/status/" + statusID, statusID, true},
        {"https://x.com/Lord_PSMG/status/" + statusID, statusID, true},
        {"https://fxtwitter.com/Lord_PSMG/status/" + statusID, statusID, true},
        {"https://not-vxtwitter.com/Lord_PSMG/status/" + statusID, "", false},
        {"https://vxtwitter.com/Lord_PSMG/status/not-a-number", "", false},
        {"https://vxtwitter.com/Lord_PSMG/not-a-status/" + statusID, "", false},
    }
    for _, tt := range cases {
        t.Run(tt.raw, func(t *testing.T) {
            u, err := url.Parse(tt.raw)
            if err != nil { t.Fatal(err) }
            id, ok := twitterStatusID(u)
            if id != tt.want || ok != tt.ok {
                t.Fatalf("twitterStatusID(%q) = %q, %v; want %q, %v", tt.raw, id, ok, tt.want, tt.ok)
            }
        })
    }
}
