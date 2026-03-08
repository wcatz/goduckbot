# Twitter/X Setup

goduckbot posts block notifications and leader schedule announcements to Twitter/X using OAuth 1.0a.

## What's supported

- Text tweets for every block notification
- GIF or image media attached automatically when `duck.media` is `"gif"`, `"image"`, or `"both"`
- Media upload uses the chunked Twitter v1.1 upload API via gotwi, posted via raw OAuth1 HMAC-SHA256 against `api.x.com`
- GIFs use `tweet_gif` category; images use `tweet_image` category (JPEG)
- Falls back to text-only if media download or upload fails — the tweet is never dropped

## Prerequisites

You need a Twitter Developer account with an app that has **Read and Write** permissions and OAuth 1.0a credentials.

1. Go to [developer.twitter.com](https://developer.twitter.com) and create a project + app
2. Set app permissions to **Read and Write**
3. Generate **Access Token and Secret** (under "Keys and Tokens")
4. Copy all four credential values

## Configuration

```yaml
twitter:
  enabled: true
  apiKey: "YOUR_API_KEY"
  apiKeySecret: "YOUR_API_KEY_SECRET"
  accessToken: "YOUR_ACCESS_TOKEN"
  accessTokenSecret: "YOUR_ACCESS_TOKEN_SECRET"
```

All four values can also be set via environment variables, which take precedence over config.yaml:

```text
TWITTER_API_KEY
TWITTER_API_KEY_SECRET
TWITTER_ACCESS_TOKEN
TWITTER_ACCESS_TOKEN_SECRET
```

## Duck media on tweets

Controlled by the top-level `duck.media` config key — same setting as Telegram:

```yaml
duck:
  media: "gif"    # "gif", "image", or "both"
```

- `"gif"` — attaches a GIF from random-d.uk
- `"image"` — attaches a JPEG image
- `"both"` — randomly picks GIF or image per notification

## Disabling

```yaml
twitter:
  enabled: false
```

Setting `enabled: false` is the only guaranteed hard-disable. If `twitter.enabled` is omitted
from config entirely, the bot treats it as enabled for backward compatibility — meaning Twitter
will post if the `TWITTER_*` environment variables are set.
