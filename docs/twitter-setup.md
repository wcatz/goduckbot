# Twitter/X Setup Guide for goduckbot

This guide covers everything you need to enable Twitter/X notifications in goduckbot.

## Current Implementation Status

**Library**: [gotwi](https://github.com/michimani/gotwi) v0.18.1
**API Version**: Twitter API v2 with OAuth 1.0a User Context
**Status**: ✅ Fully implemented, tested, and ready to use
**Authentication Method**: OAuth 1.0a (4 credentials required)

## What Gets Posted

When your pool mints a block, goduckbot posts a **shortened tweet** (optimized for the 280-character limit):

```text
🦆 New Block!

Pool: DuckPool
Tx: 14 | Size: 42.69KB
Epoch: 7 | Lifetime: 1,337

pooltool.io/realtime/12345
```

The tweet format includes:
- Duck emoji
- Pool name
- Transaction count and block size
- Epoch blocks and lifetime blocks
- Link to PoolTool realtime view

**Note**: The Twitter message is intentionally shorter than the Telegram message due to the 280-character limit. Currently, image posting is NOT implemented (only text tweets).

## Required API Access

### Free Tier (Current as of Feb 2026)

The X API Free Tier provides:
- **500 Posts/month** (~16 posts/day)
- **100 Reads/month**
- **1 App environment**
- **Cost**: Free

**Source**: [X API Pricing Guide 2026](https://getlate.dev/blog/twitter-api-pricing)

### Is Free Tier Sufficient?

For most small-to-medium pools, **YES**:
- If you mint **1 block/day** → ~30 posts/month (well within 500 limit)
- If you mint **5 blocks/day** → ~150 posts/month (still within limit)
- If you mint **16 blocks/day** → ~500 posts/month (at the limit)

**If you exceed 500 posts/month**, you'll need to upgrade:
- **Basic Tier**: $200/month (10K posts/month, 2 apps)
- **Pro Tier**: $5K/month (1M posts/month, 3 apps)

**Source**: [X API Pricing Tiers 2025](https://twitterapi.io/blog/twitter-api-pricing-2025)

## Getting API Credentials

### Step 1: Create a Developer Account

1. Go to [developer.x.com](https://developer.x.com)
2. Sign in with your Twitter/X account (the account that will post tweets)
3. Apply for a developer account (if you don't have one)
4. Choose the **Free** tier

### Step 2: Create a New App

1. Navigate to the [Developer Portal](https://developer.x.com/en/portal/dashboard)
2. Click **+ Create Project** or **+ Add App**
3. Fill in app details:
   - **App name**: `goduckbot` (or your pool name)
   - **Description**: "Cardano stake pool block notifications"
   - **Use case**: "Making automated posts about blockchain events"

### Step 3: Get OAuth 1.0a Credentials

After creating your app, you need **4 credentials**:

1. **API Key** (Consumer Key)
2. **API Key Secret** (Consumer Secret)
3. **Access Token**
4. **Access Token Secret**

#### Where to Find Them

In your app's dashboard:
1. Go to **"Keys and tokens"** tab
2. Under **"Consumer Keys"**:
   - Copy **API Key**
   - Copy **API Key Secret**
3. Under **"Authentication Tokens"**:
   - Click **"Generate"** (if not already generated)
   - Copy **Access Token**
   - Copy **Access Token Secret**

**IMPORTANT**: Store these credentials securely. The secrets are only shown once!

### Step 4: Set App Permissions

1. In your app settings, go to **"User authentication settings"**
2. Click **"Set up"**
3. Set **App permissions** to **"Read and Write"** (required for posting tweets)
4. Save changes

You may need to **regenerate** your Access Token after changing permissions.

## Configuration

### 1. Set Environment Variables

goduckbot reads Twitter credentials from environment variables:

```bash
export TWITTER_API_KEY="your_api_key_here"
export TWITTER_API_KEY_SECRET="your_api_key_secret_here"
export TWITTER_ACCESS_TOKEN="your_access_token_here"
export TWITTER_ACCESS_TOKEN_SECRET="your_access_token_secret_here"
```

**Kubernetes/Helm**: Use a Secret to store these values (see helm chart section below).

### 2. Enable Twitter in config.yaml

Edit your `config.yaml`:

```yaml
twitter:
  enabled: true  # Change from false to true
```

That's it! No other config changes needed.

### 3. Restart goduckbot

```bash
./goduckbot
```

You should see in the logs:

```text
Twitter client initialized successfully
```

If Twitter is disabled, you'll see:

```text
Twitter credentials not provided, Twitter notifications disabled
```

or

```text
Twitter notifications disabled via config
```

## Helm Chart Deployment

If deploying via the goduckbot Helm chart:

### 1. Create a Kubernetes Secret

```bash
kubectl create secret generic goduckbot-twitter \
  --from-literal=TWITTER_API_KEY="your_api_key" \
  --from-literal=TWITTER_API_KEY_SECRET="your_api_key_secret" \
  --from-literal=TWITTER_ACCESS_TOKEN="your_access_token" \
  --from-literal=TWITTER_ACCESS_TOKEN_SECRET="your_access_token_secret" \
  -n duckbot
```

### 2. Update values.yaml

```yaml
config:
  twitter:
    enabled: true

env:
  - name: TWITTER_API_KEY
    valueFrom:
      secretKeyRef:
        name: goduckbot-twitter
        key: TWITTER_API_KEY
  - name: TWITTER_API_KEY_SECRET
    valueFrom:
      secretKeyRef:
        name: goduckbot-twitter
        key: TWITTER_API_KEY_SECRET
  - name: TWITTER_ACCESS_TOKEN
    valueFrom:
      secretKeyRef:
        name: goduckbot-twitter
        key: TWITTER_ACCESS_TOKEN
  - name: TWITTER_ACCESS_TOKEN_SECRET
    valueFrom:
      secretKeyRef:
        name: goduckbot-twitter
        key: TWITTER_ACCESS_TOKEN_SECRET
```

### 3. Deploy with Helm

```bash
helm upgrade --install goduckbot ./helm-chart \
  -f values.yaml \
  -n duckbot
```

## Testing Twitter Integration

### Quick Test

1. Set env vars and enable Twitter in config
2. Start goduckbot
3. Wait for your pool to mint a block (or trigger a test block event)
4. Check your Twitter feed — you should see the block notification tweet

### Manual Testing (Without Waiting for a Block)

You can't easily trigger a test tweet without modifying code, but you can verify the client initializes correctly by checking logs.

## Troubleshooting

### "Twitter credentials not provided"

**Cause**: Environment variables are not set or misspelled.

**Fix**:
- Verify all 4 env vars are set: `echo $TWITTER_API_KEY`
- Check for typos in env var names
- Ensure env vars are exported before running goduckbot

### "failed to initialize Twitter client"

**Possible causes**:
1. Invalid credentials (copy/paste error)
2. App permissions not set to "Read and Write"
3. Access tokens not regenerated after permission change

**Fix**:
- Double-check credentials in Developer Portal
- Verify app permissions are "Read and Write"
- Regenerate Access Token/Secret if needed

### "failed to post tweet: 403 Forbidden"

**Possible causes**:
1. App doesn't have write permissions
2. Access token expired or revoked
3. Free tier quota exceeded (500 posts/month)

**Fix**:
- Check app permissions in Developer Portal
- Regenerate Access Token/Secret
- Check your API usage in Developer Portal → Usage tab

### Tweets Not Posting (No Errors)

**Cause**: `twitter.enabled: false` in config.yaml

**Fix**: Set `twitter.enabled: true` and restart goduckbot

## Code Overview

The Twitter integration is fully implemented in `/home/wayne/git/goduckbot/main.go`:

### Initialization (lines 228-263)

```go
twitterConfigEnabled := viper.GetBool("twitter.enabled")
if !viper.IsSet("twitter.enabled") {
    // Backward compatibility: enable if env vars are present
    twitterConfigEnabled = true
}

if twitterConfigEnabled {
    twitterAPIKey := os.Getenv("TWITTER_API_KEY")
    twitterAPISecret := os.Getenv("TWITTER_API_KEY_SECRET")
    twitterAccessToken := os.Getenv("TWITTER_ACCESS_TOKEN")
    twitterAccessTokenSecret := os.Getenv("TWITTER_ACCESS_TOKEN_SECRET")

    if twitterAPIKey != "" && twitterAPISecret != "" &&
        twitterAccessToken != "" && twitterAccessTokenSecret != "" {
        in := &gotwi.NewClientInput{
            AuthenticationMethod: gotwi.AuthenMethodOAuth1UserContext,
            OAuthToken:           twitterAccessToken,
            OAuthTokenSecret:     twitterAccessTokenSecret,
            APIKey:               twitterAPIKey,
            APIKeySecret:         twitterAPISecret,
        }
        i.twitterClient, err = gotwi.NewClient(in)
        if err != nil {
            log.Printf("failed to initialize Twitter client: %s", err)
        } else {
            i.twitterEnabled = true
            log.Println("Twitter client initialized successfully")
        }
    }
}
```

### Posting Tweets (lines 653-669)

```go
if i.twitterEnabled {
    tweetMsg := fmt.Sprintf(
        "🦆 New Block!\n\n"+
            "Pool: %s\n"+
            "Tx: %d | Size: %.2fKB\n"+
            "Epoch: %d | Lifetime: %d\n\n"+
            "pooltool.io/realtime/%d",
        i.poolName, blockEvent.Payload.TransactionCount, blockSizeKB,
        i.epochBlocks, i.totalBlocks,
        blockEvent.Context.BlockNumber)

    err = i.sendTweet(tweetMsg, duckImageURL)
    if err != nil {
        log.Printf("failed to send tweet: %s", err)
    }
}
```

### Tweet Sending Function (lines 756-774)

```go
func (i *Indexer) sendTweet(text string, imageURL string) error {
    if !i.twitterEnabled || i.twitterClient == nil {
        return nil
    }

    input := &types.CreateInput{
        Text: gotwi.String(text),
    }

    _, err := managetweet.Create(context.Background(), i.twitterClient, input)
    if err != nil {
        return fmt.Errorf("failed to post tweet: %w", err)
    }

    log.Println("Tweet posted successfully")
    return nil
}
```

## Potential Code Improvements

While the current implementation is **production-ready**, here are some enhancements to consider:

### 1. Image Support

**Current**: `imageURL` parameter is passed but not used
**Why**: Twitter API v2 media upload requires additional API calls and complexity

**To implement**:
- Download image to bytes (already implemented: `downloadImage()` function)
- Upload media via Twitter API v2 media endpoint
- Attach media ID to tweet request

**Code changes needed**:
```go
// In sendTweet() function:
// 1. Download image
imageData, err := downloadImage(imageURL)
if err != nil {
    log.Printf("Failed to download image: %v", err)
    // Continue posting text-only tweet
}

// 2. Upload media (requires gotwi media upload support or manual API call)
// Note: This may require Basic tier or higher for media upload endpoint access

// 3. Attach media ID to CreateInput
input := &types.CreateInput{
    Text: gotwi.String(text),
    Media: &types.CreateInputMedia{
        MediaIDs: []string{mediaID},
    },
}
```

**Trade-offs**:
- Increases API complexity
- May not work with Free tier (needs verification)
- Adds failure points (image download, upload)

**Recommendation**: Current text-only implementation is sufficient for notifications. Image support is nice-to-have but not critical.

### 2. Better Error Handling

**Enhancement**: Retry logic for transient failures (rate limits, network issues)

```go
// Add exponential backoff retry wrapper
func (i *Indexer) sendTweetWithRetry(text string, imageURL string) error {
    maxRetries := 3
    for attempt := 1; attempt <= maxRetries; attempt++ {
        err := i.sendTweet(text, imageURL)
        if err == nil {
            return nil
        }
        if attempt < maxRetries {
            time.Sleep(time.Duration(attempt) * 5 * time.Second)
        }
    }
    return fmt.Errorf("failed after %d retries", maxRetries)
}
```

### 3. Rate Limit Tracking

**Enhancement**: Track daily/monthly post count to stay within Free tier limits

```go
// Add to Indexer struct:
twitterPostCount int
twitterPostLimit int // 500 for Free tier

// In sendTweet():
if i.twitterPostCount >= i.twitterPostLimit {
    log.Printf("Twitter post limit reached (%d/%d)", i.twitterPostCount, i.twitterPostLimit)
    return nil
}
i.twitterPostCount++
```

### 4. Configurable Tweet Format

**Enhancement**: Allow custom tweet templates in config.yaml

```yaml
twitter:
  enabled: true
  template: "🦆 {poolName} minted block #{lifetimeBlocks}! Tx: {txCount}, Size: {blockSize}"
```

**Trade-offs**: Adds complexity, but gives operators more control over branding.

## Library Assessment: Is gotwi Still the Right Choice?

### gotwi Status (Feb 2026)

**GitHub**: [michimani/gotwi](https://github.com/michimani/gotwi)
**Latest Release**: v0.18.1 (Jan 2026 per goduckbot's go.mod)
**Actively Maintained**: Yes (releases in 2026)
**API Support**: Twitter API v2 with OAuth 1.0a
**OAuth Methods**: OAuth 1.0a User Context, OAuth 2.0 Bearer Token

**Sources**:
- [gotwi GitHub](https://github.com/michimani/gotwi)
- [gotwi Releases](https://github.com/michimani/gotwi/releases)

### Pros of gotwi

✅ **Native Twitter API v2 support** (not deprecated v1.1)
✅ **OAuth 1.0a User Context** for posting tweets
✅ **Active maintenance** (v0.18.1 in 2026)
✅ **Simple API** (minimal code required)
✅ **Well-documented** authentication methods
✅ **Production-ready** (already working in goduckbot)

### Cons of gotwi

⚠️ **Limited media upload examples** (may need custom implementation)
⚠️ **Still marked as "under development"** on GitHub
⚠️ **Smaller community** compared to other Twitter libraries

### Alternative Libraries

#### 1. [anaconda](https://github.com/ChimeraCoder/anaconda)
- **Status**: Unmaintained since 2019
- **API**: Twitter API v1.1 (deprecated)
- **Recommendation**: ❌ Do NOT use

#### 2. [go-twitter](https://github.com/dghubble/go-twitter)
- **Status**: Maintained
- **API**: Twitter API v1.1 (deprecated) and some v2 support
- **Recommendation**: ⚠️ Partial v2 support, prefer gotwi

#### 3. [twitter-api-v2-go](https://github.com/g8rswimmer/go-twitter)
- **Status**: Maintained
- **API**: Twitter API v2
- **Recommendation**: ✅ Viable alternative to gotwi

### Verdict: Should We Switch?

**NO** — gotwi is the correct choice for goduckbot.

**Reasons**:
1. ✅ Already implemented and working
2. ✅ Supports Twitter API v2 (not deprecated)
3. ✅ Actively maintained (2026 releases)
4. ✅ OAuth 1.0a support is stable
5. ✅ Simple API for posting text tweets

**When to consider alternatives**:
- If gotwi stops being maintained (no releases for 12+ months)
- If you need advanced features not supported by gotwi
- If major bugs are discovered and not fixed

## X API v1.1 Deprecation Status

**Important Note**: Twitter API v1.1 was deprecated in June 2025 and is now **effectively end-of-life** as of Feb 2026. goduckbot correctly uses **Twitter API v2** via gotwi, so there's no migration needed.

**Sources**:
- [X API v1.1 to v2 Migration Guide](https://developer.twitter.com/en/docs/twitter-api/tweets/lookup/migrate/standard-to-twitter-api-v2)
- [OAuth 1.0a with API v2](https://developer.x.com/en/docs/tutorials/authenticating-with-twitter-api-for-enterprise/oauth1-0a-and-user-access-tokens)

## Summary

| Aspect | Status |
|--------|--------|
| **Implementation** | ✅ Complete and production-ready |
| **Library (gotwi)** | ✅ Maintained, supports API v2 |
| **Free Tier** | ✅ Sufficient for most pools (500 posts/month) |
| **Configuration** | ✅ Simple (4 env vars + 1 config flag) |
| **Tweet Format** | ✅ Optimized for 280 chars |
| **Image Support** | ❌ Not implemented (text-only) |
| **Code Quality** | ✅ Clean, minimal, well-structured |

**Recommendation**: The current Twitter integration is ready to use. No code changes needed. Just get your API credentials, set env vars, enable in config, and you're good to go.

---

## References

- [gotwi GitHub Repository](https://github.com/michimani/gotwi)
- [X API Pricing Guide 2026](https://getlate.dev/blog/twitter-api-pricing)
- [X API Pricing Tiers 2025](https://twitterapi.io/blog/twitter-api-pricing-2025)
- [OAuth 1.0a with Twitter API v2](https://developer.x.com/en/docs/tutorials/authenticating-with-twitter-api-for-enterprise/oauth1-0a-and-user-access-tokens)
- [X Developer Portal](https://developer.x.com/en/portal/dashboard)
