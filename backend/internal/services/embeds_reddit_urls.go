package services

import (
	"net/url"
	"strings"
)

type redditTarget struct {
	url                    *url.URL
	id, comment, community string
	share                  bool
}

func redditHTTPS(u *url.URL) bool {
	return u != nil && u.Scheme == "https" && u.User == nil && (u.Port() == "" || u.Port() == "443")
}

func redditToken(value string, max int) bool {
	if len(value) == 0 || len(value) > max {
		return false
	}
	for _, c := range value {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

func redditPostTarget(u *url.URL) (redditTarget, bool) {
	if !redditHTTPS(u) {
		return redditTarget{}, false
	}
	host := strings.ToLower(u.Hostname())
	switch host {
	case "reddit.com", "www.reddit.com", "old.reddit.com", "new.reddit.com", "m.reddit.com", "sh.reddit.com", "np.reddit.com", "redd.it", "www.redd.it":
	default:
		return redditTarget{}, false
	}
	parts := strings.Split(strings.Trim(u.EscapedPath(), "/"), "/")
	for _, part := range parts {
		if strings.HasSuffix(strings.ToLower(part), ".json") {
			return redditTarget{}, false
		}
	}
	canonical := *u
	canonical.Host, canonical.RawQuery, canonical.Fragment = "www.reddit.com", "", ""
	result := redditTarget{url: &canonical}
	var tail []string
	switch {
	case (host == "redd.it" || host == "www.redd.it") && len(parts) == 1 && redditToken(parts[0], 16):
		result.id = parts[0]
		canonical.Path, canonical.RawPath = "/comments/"+result.id+"/", ""
	case (host == "redd.it" || host == "www.redd.it"):
		return redditTarget{}, false
	case len(parts) == 2 && parts[0] == "gallery" && redditToken(parts[1], 16):
		result.id = parts[1]
		canonical.Path, canonical.RawPath = "/comments/"+result.id+"/", ""
	case len(parts) >= 2 && parts[0] == "comments" && redditToken(parts[1], 16):
		result.id, tail = parts[1], parts[2:]
	case len(parts) == 4 && parts[0] == "r" && redditToken(parts[1], 128) && parts[2] == "s" && redditToken(parts[3], 128):
		result.community, result.share = parts[1], true
	case len(parts) >= 4 && (parts[0] == "r" || parts[0] == "user" || parts[0] == "u") && redditToken(parts[1], 128) && parts[2] == "comments" && redditToken(parts[3], 16):
		result.id, tail = parts[3], parts[4:]
		if parts[0] == "r" {
			result.community = parts[1]
		}
	default:
		return redditTarget{}, false
	}
	if len(tail) > 2 || len(tail) == 2 && !redditToken(tail[1], 16) {
		return redditTarget{}, false
	}
	if len(tail) == 2 {
		result.comment = tail[1]
	}
	return result, true
}

func isRedditPostURL(u *url.URL) bool {
	_, valid := redditPostTarget(u)
	return valid
}

func redditImageURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || !redditHTTPS(u) || len(raw) > embedURLMax {
		return ""
	}
	switch strings.ToLower(u.Hostname()) {
	case "i.redd.it", "preview.redd.it", "cf.preview.redd.it", "external-preview.redd.it", "a.thumbs.redditmedia.com", "b.thumbs.redditmedia.com":
		return u.String()
	}
	return ""
}

func redditVideoURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || !redditHTTPS(u) || len(raw) > embedURLMax || !strings.HasSuffix(strings.ToLower(u.Path), ".mp4") {
		return ""
	}
	if u.Hostname() == "packaged-media.redd.it" || u.Hostname() == "v.redd.it" {
		return u.String()
	}
	return ""
}

func redditTitle(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "reddit", "reddit - dive into anything", "just a moment...", "blocked", "whoa there, pardner!":
		return false
	}
	return true
}
