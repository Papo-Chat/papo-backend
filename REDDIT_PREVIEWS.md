# Reddit previews

Reddit links use a dedicated provider built around the HTML served by Reddit's
own embed renderer (`https://embed.reddit.com/<post path>`). The provider never
executes scripts, sends account cookies or scrapes the main website for post
content. Reddit's normal website can return a JavaScript challenge with HTTP
200; such a response must not become a cached card titled "Reddit".

## Request flow

1. Accept only approved HTTPS Reddit post/comment, gallery and share URLs.
   Reject credentials, nonstandard ports, spoofed hosts and JSON endpoints.
2. Resolve opaque `/r/<community>/s/<token>` links using redirect headers only;
   never read the main website's response body. `redd.it/<id>` and gallery URLs
   already contain the post ID and need no preliminary request.
3. Read the corresponding first-party embed document with a four-second budget,
   preserving the existing safe transport, total timeout and outbound semaphore.
   Every redirect must remain an embed for the same post/comment. Do not follow
   links, scripts, trackers, iframes or login/challenge redirects from the HTML.
4. Stream the post title, author, body and preview image from recognized DOM
   elements. Require a matching post/comment identity before accepting content.
   Select an available H.264 packaged MP4 rendition, preferring up to 720p.
   Unlike guessed `DASH_*.mp4` URLs, the packaged rendition contains video and
   audio together. HLS/DASH manifests are not passed to the direct-video relay.
5. If the document is blocked, unavailable or unrecognized, request the
   [official oEmbed endpoint](https://publish.reddit.com/assets/Reddit_Embed_Documentation.pdf)
   once for title/author metadata. Discard its executable HTML. Respect 404,
   410 and 429 responses, cancellation and rejected redirects without retrying
   through other endpoints.
6. Download a recognized Reddit preview image through the normal thumbnail
   processing, robots checks, byte limit and content-addressed storage. Restrict
   image redirects to approved Reddit media hosts. A missing image does not
   discard valid post text or video metadata.

The original normalized link remains the cache key; the rendered card links to
the canonical post/comment. Other link providers and the frontend `Embed` API
remain unchanged. No generic Reddit robots exception is introduced: the provider
uses the first-party embed renderer and oEmbed API, and ordinary image robots
checks still apply.

## Cache, media and limits

- Share the existing URL cache and rate limits. Ordinary cache hits make no
  network requests. Text-only oEmbed fallbacks and old invalid "Reddit" cards
  may be retried on a later share after five minutes, rather than every message.
- Signed `packaged-media.redd.it` MP4 URLs can expire before the normal cache
  TTL. Refresh entries within five minutes of expiry, including before video
  playback, after the existing channel-read authorization check. Fresh range
  requests reuse the renewed URL. The video still travels through the existing
  authenticated, MIME-checked, range-capable relay.
- HTML and images retain the 5 MiB input limits; oEmbed is limited to 1 MiB and
  packaged video metadata to 64 KiB. Titles/descriptions retain the shared text
  limits, and the streaming parser bounds retained text while preserving UTF-8.
- The message crawler allows at least 20 seconds for Reddit document/fallback,
  thumbnail processing and persistence. Longer configured budgets are retained,
  including the 30-second X proxy animation budget in mixed-link messages.
- One preview image is exposed. Complete gallery browsing, live vote counts and
  fetching entire comment threads are not implemented. Comment previews require
  their own recognized content; the parent post is not substituted silently.
- If neither official source is accessible from the deployment host, no preview
  can be generated. A metadata fallback cannot invent missing media. Private,
  deleted and login-gated content is not accessed with a user's session.

## Validation

Use a disposable PostgreSQL instance: the package test harness otherwise skips
database-dependent suites when no database is available.

```sh
export TEST_DATABASE_URL='postgres://USER:PASSWORD@127.0.0.1:PORT/TEST_DB?sslmode=disable'
cd backend
go test -race ./internal/services -run '^TestReddit' -count=1 -v
go test ./... -count=1
go build ./...
go vet ./...
```

An optional public-network test exercises the actual provider, thumbnail
download and temporary PostgreSQL persistence for the supplied Factorio share
link, a Reddit announcement text post and an image post:

```sh
PAPO_REDDIT_LIVE=1 go test ./internal/services -run '^TestRedditLivePublicPreviews$' -count=1 -v
```

It uses no Reddit account, production database, browser cookies or Papo session.
Production network access can differ from a development host; run this opt-in
check on the deployment host before deployment. Deployment remains separate
from opening or merging the source PR.
