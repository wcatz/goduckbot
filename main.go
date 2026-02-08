package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/blinklabs-io/gouroboros/ledger"
	koios "github.com/cardano-community/koios-go-client/v3"
	"github.com/gorilla/websocket"
	"github.com/michimani/gotwi"
	"github.com/michimani/gotwi/media/upload"
	upload_types "github.com/michimani/gotwi/media/upload/types"
	"github.com/michimani/gotwi/tweet/managetweet"
	"github.com/michimani/gotwi/tweet/managetweet/types"
	"github.com/spf13/viper"
	telebot "gopkg.in/tucnak/telebot.v2"
)

const (
	fullBlockSize       = 87.97
	EpochDurationInDays = 5
	SecondsInDay        = 24 * 60 * 60
	ShelleyEpochStart   = "2020-07-29T21:44:51Z"
	StartingEpoch       = 208
	maxRetryDuration    = time.Minute
)

// Channel to broadcast block events to connected clients
var clients = make(map[*websocket.Conn]bool) // connected clients
var broadcast = make(chan interface{}, 100)    // broadcast channel (buffered to prevent deadlock)
var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

// Singleton instance of the Indexer
var globalIndexer = &Indexer{}

// Indexer struct to manage chain sync and block events
type Indexer struct {
	bot             *telebot.Bot
	poolId          string
	telegramChannel string
	telegramToken   string
	image           string
	ticker          string
	koios           *koios.Client
	bech32PoolId    string
	epochBlocks     int
	nodeAddresses   []string
	totalBlocks     uint64
	poolName        string
	epoch           int
	networkMagic    int
	wg              sync.WaitGroup
	// Mode: "lite" (live tail + Koios) or "full" (historical sync + live tail)
	mode string
	// Social network toggles
	telegramEnabled    bool
	telegramGifEnabled bool
	twitterEnabled     bool
	twitterGifEnabled  bool
	twitterClient      *gotwi.Client
	// Bot command access control
	allowedUsers  map[int64]bool
	allowedGroups map[int64]bool
	nodeQuery     *NodeQueryClient
	prevBlockTime time.Time // tracks interval between live blocks
	// Leaderlog fields
	vrfKey           *VRFKey
	leaderlogEnabled bool
	leaderlogTZ      string
	store            Store
	nonceTracker     *NonceTracker
	leaderlogMu      sync.Mutex
	leaderlogCalcing map[int]bool      // epochs currently being calculated
	leaderlogFailed  map[int]time.Time // cooldown: epoch -> last failure time
	scheduleExists   map[int]bool      // cached: epoch -> schedule already in DB
	syncer           *ChainSyncer // nil in lite mode
}

// Function to calculate the current epoch number
func getCurrentEpoch() int {
	// Parse the Shelley epoch start time
	shelleyStartTime, err := time.Parse(time.RFC3339, ShelleyEpochStart)
	if err != nil {
		fmt.Println("Error parsing Shelley start time:", err)
		return -1
	}
	// Calculate the elapsed time since Shelley start in seconds
	elapsedSeconds := time.Since(shelleyStartTime).Seconds()
	// Calculate the number of epochs elapsed
	epochsElapsed := int(elapsedSeconds / (EpochDurationInDays * SecondsInDay))
	// Calculate the current epoch number
	currentEpoch := StartingEpoch + epochsElapsed

	return currentEpoch
}

// initStore creates the appropriate Store based on config.
func initStore() (Store, error) {
	driver := viper.GetString("database.driver")
	if driver == "" {
		driver = "sqlite"
	}

	switch driver {
	case "sqlite":
		path := viper.GetString("database.path")
		if path == "" {
			path = "./goduckbot.db"
		}
		return NewSqliteStore(path)

	case "postgres":
		dbHost := viper.GetString("database.host")
		dbPort := viper.GetInt("database.port")
		dbName := viper.GetString("database.name")
		dbUser := viper.GetString("database.user")
		dbPassword := os.Getenv("GODUCKBOT_DB_PASSWORD")
		if dbPassword == "" {
			dbPassword = viper.GetString("database.password")
			if dbPassword != "" {
				log.Println("WARNING: using database password from config file; prefer GODUCKBOT_DB_PASSWORD env var")
			}
		}

		connURL := &url.URL{
			Scheme:   "postgres",
			User:     url.UserPassword(dbUser, dbPassword),
			Host:     fmt.Sprintf("%s:%d", dbHost, dbPort),
			Path:     dbName,
			RawQuery: "sslmode=disable",
		}
		return NewPgStore(connURL.String())

	default:
		return nil, fmt.Errorf("unsupported database driver: %s (use 'sqlite' or 'postgres')", driver)
	}
}

// Start initializes config, connections, and begins chain sync.
func (i *Indexer) Start() error {
	// Increment the WaitGroup counter
	i.wg.Add(1)
	defer func() {
		// Decrement the WaitGroup counter when the function exits
		i.wg.Done()
		if r := recover(); r != nil {
			log.Println("Recovered in Start:", r)
		}
	}()

	viper.SetConfigName("config") // name of config file (without extension)
	viper.AddConfigPath(".")      // look for config in the working directory

	e := viper.ReadInConfig() // Find and read the config file
	if e != nil {             // Handle errors reading the config file
		log.Fatalf("Error while reading config file %s", e)
	}

	// Set the configuration values
	i.poolId = viper.GetString("poolId")
	i.ticker = viper.GetString("ticker")
	i.poolName = viper.GetString("poolName")
	i.telegramChannel = viper.GetString("telegram.channel")
	i.telegramToken = os.Getenv("TELEGRAM_TOKEN")
	i.image = viper.GetString("image")
	i.networkMagic = viper.GetInt("networkMagic")
	// Store the node addresses hosts into the array nodeAddresses in the Indexer
	i.nodeAddresses = viper.GetStringSlice("nodeAddress.host1")
	i.nodeAddresses = append(i.nodeAddresses, viper.GetStringSlice("nodeAddress.host2")...)

	// Mode: "lite" (default) or "full"
	i.mode = viper.GetString("mode")
	if i.mode == "" {
		i.mode = "lite"
	}
	log.Printf("Running in %s mode", i.mode)

	// Social network toggles
	i.telegramEnabled = viper.GetBool("telegram.enabled")
	// Default to true if not explicitly set (backward compatibility)
	if !viper.IsSet("telegram.enabled") {
		i.telegramEnabled = true
	}

	// GIF support toggle
	i.telegramGifEnabled = viper.GetBool("telegram.gifEnabled")
	// Default to true if not explicitly set
	if !viper.IsSet("telegram.gifEnabled") {
		i.telegramGifEnabled = true
	}

	// Initialize Telegram bot if enabled
	if i.telegramEnabled {
		var err error
		i.bot, err = telebot.NewBot(telebot.Settings{
			Token:  i.telegramToken,
			Poller: &telebot.LongPoller{Timeout: 10 * time.Second},
		})
		if err != nil {
			log.Fatalf("failed to start bot: %s", err)
		}
		log.Println("Telegram bot initialized")

		// Load allowed user IDs for bot commands (admin: all commands)
		i.allowedUsers = make(map[int64]bool)
		for _, id := range viper.GetIntSlice("telegram.allowedUsers") {
			i.allowedUsers[int64(id)] = true
		}
		if len(i.allowedUsers) > 0 {
			log.Printf("Bot commands enabled for %d admin user(s)", len(i.allowedUsers))
		}

		// Load allowed group IDs (group members: safe commands only)
		i.allowedGroups = make(map[int64]bool)
		for _, id := range viper.GetIntSlice("telegram.allowedGroups") {
			i.allowedGroups[int64(id)] = true
		}
		if len(i.allowedGroups) > 0 {
			log.Printf("Group commands enabled for %d group(s)", len(i.allowedGroups))
		}
	} else {
		log.Println("Telegram notifications disabled")
	}

	// Initialize node query client (NtC local state query via gouroboros).
	// Supports both TCP (socat bridge) and UNIX socket (direct) connections.
	ntcHost := viper.GetString("nodeAddress.ntcHost")
	if ntcHost == "" && len(i.nodeAddresses) > 0 {
		ntcHost = i.nodeAddresses[0] // fallback to NtN address
	}
	if ntcHost != "" {
		network, address := parseNodeAddress(ntcHost)
		i.nodeQuery = NewNodeQueryClient(ntcHost, i.networkMagic)
		log.Printf("Node query client initialized (NtC): %s://%s", network, address)
	}

	// Twitter toggle
	twitterConfigEnabled := viper.GetBool("twitter.enabled")
	if !viper.IsSet("twitter.enabled") {
		// Backward compatibility: enable if env vars are present
		twitterConfigEnabled = true
	}

	// Twitter GIF support toggle
	i.twitterGifEnabled = viper.GetBool("twitter.gifEnabled")
	// Default to true if not explicitly set
	if !viper.IsSet("twitter.gifEnabled") {
		i.twitterGifEnabled = true
	}

	if twitterConfigEnabled {
		twitterAPIKey := os.Getenv("TWITTER_API_KEY")
		twitterAPISecret := os.Getenv("TWITTER_API_KEY_SECRET")
		twitterAccessToken := os.Getenv("TWITTER_ACCESS_TOKEN")
		twitterAccessTokenSecret := os.Getenv("TWITTER_ACCESS_TOKEN_SECRET")

		if twitterAPIKey != "" && twitterAPISecret != "" &&
			twitterAccessToken != "" && twitterAccessTokenSecret != "" {
			var err error
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
		} else {
			log.Println("Twitter credentials not provided, Twitter notifications disabled")
		}
	} else {
		log.Println("Twitter notifications disabled via config")
	}

	/// Initialize the koios client based on networkMagic number
	if i.networkMagic == PreprodNetworkMagic {
		i.koios, e = koios.New(
			koios.Host(koios.PreProdHost),
			koios.APIVersion("v1"),
		)
		if e != nil {
			log.Fatal(e)
		}
	} else if i.networkMagic == PreviewNetworkMagic {
		i.koios, e = koios.New(
			koios.Host(koios.PreviewHost),
			koios.APIVersion("v1"),
		)
		if e != nil {
			log.Fatal(e)
		}
	} else {
		i.koios, e = koios.New(
			koios.Host(koios.MainnetHost),
			koios.APIVersion("v1"),
		)
		if e != nil {
			log.Fatal(e)
		}
	}

	i.epoch = getCurrentEpoch()
	log.Printf("Epoch: %d", i.epoch)

	// Initialize leaderlog if enabled
	i.leaderlogEnabled = viper.GetBool("leaderlog.enabled")
	i.leaderlogTZ = viper.GetString("leaderlog.timezone")
	if i.leaderlogTZ == "" {
		i.leaderlogTZ = "UTC"
	}

	if i.leaderlogEnabled {
		vrfKeyPath := viper.GetString("leaderlog.vrfKeyPath")
		vrfKey, vrfErr := ParseVRFKeyFile(vrfKeyPath)
		if vrfErr != nil {
			log.Fatalf("failed to parse VRF key from %s: %s", vrfKeyPath, vrfErr)
		}
		i.vrfKey = vrfKey
		log.Printf("Leaderlog enabled, VRF key loaded from %s", vrfKeyPath)

		// Initialize Store (SQLite or PostgreSQL)
		store, dbErr := initStore()
		if dbErr != nil {
			log.Fatalf("failed to initialize database: %s", dbErr)
		}
		i.store = store

		// Initialize nonce tracker
		fullMode := i.mode == "full"
		i.nonceTracker = NewNonceTracker(i.store, i.koios, i.epoch, i.networkMagic, fullMode)
		i.leaderlogCalcing = make(map[int]bool)
		i.leaderlogFailed = make(map[int]time.Time)
		i.scheduleExists = make(map[int]bool)
		log.Println("Nonce tracker initialized")

		// Backfill nonce history in background
		if fullMode {
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
				defer cancel()
				if err := i.nonceTracker.BackfillNonces(ctx); err != nil {
					log.Printf("Nonce backfill failed: %v", err)
				}
			}()
		}
	}

	// Convert the poolId to Bech32
	bech32PoolId, err := convertToBech32(i.poolId)
	if err != nil {
		log.Printf("failed to convert pool id to Bech32: %s", err)
	}

	// Set the bech32PoolId field in the Indexer
	i.bech32PoolId = bech32PoolId
	// Get lifetime blocks
	lifetimeBlocks, err := i.koios.GetPoolInfo(context.Background(), koios.PoolID(i.bech32PoolId), nil)
	if err != nil {
		log.Fatalf("failed to get pool lifetime blocks: %s", err)
	}

	// Get epoch blocks
	epoch := koios.EpochNo(i.epoch)
	epochBlocks, err := i.koios.GetPoolBlocks(context.Background(), koios.PoolID(i.bech32PoolId), &epoch, nil)

	if epochBlocks.Data != nil {
		i.epochBlocks = len(epochBlocks.Data)
	} else {
		log.Fatalf("failed to get pool epoch blocks: %s", err)
	}

	if lifetimeBlocks.Data != nil {
		i.totalBlocks = lifetimeBlocks.Data.BlockCount
	} else {
		log.Fatalf("failed to get pool lifetime blocks: %s", err)
	}
	log.Println("quack(duckBot initialized)")
	log.Printf("duckBot started: %s | Epoch: %d | Epoch Blocks: %d | Lifetime Blocks: %d | Mode: %s",
		i.poolName, i.epoch, i.epochBlocks, i.totalBlocks, i.mode)

	// Register bot commands and start polling
	if i.telegramEnabled && i.bot != nil && len(i.allowedUsers) > 0 {
		i.registerCommands()
		go i.bot.Start()
	}

	// Run unified chain sync (historical + live tail) with auto-reconnect
	return i.runChainTail()
}

// flushBlockBatch bulk-inserts blocks via CopyFrom and evolves nonce in-memory.
// Small batches (live tail) use individual ProcessBlock to avoid CopyFrom failures on rollbacks.
// Large batches (historical sync) use CopyFrom for throughput, with fallback on duplicate keys.
func (i *Indexer) flushBlockBatch(batch []BlockData) {
	// Small batches (live tail after rollback) — use individual inserts with dedup
	if len(batch) < 100 {
		for _, b := range batch {
			i.nonceTracker.ProcessBlock(b.Slot, b.Epoch, b.BlockHash, b.VrfOutput)
		}
		return
	}

	// Large batches (historical sync) — bulk insert via CopyFrom
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	err := i.store.InsertBlockBatch(ctx, batch)
	cancel()

	if err != nil {
		// CopyFrom fails on duplicate keys — fall back to individual inserts
		log.Printf("Batch insert failed (resume duplicates), falling back: %v", err)
		for _, b := range batch {
			i.nonceTracker.ProcessBlock(b.Slot, b.Epoch, b.BlockHash, b.VrfOutput)
		}
		return
	}

	// Blocks inserted via CopyFrom, evolve nonce in-memory with single DB persist
	i.nonceTracker.ProcessBatch(batch)
}

// runChainTail runs the unified chain sync (historical + live tail) with auto-reconnect.
// In full mode: syncs from last checkpoint, then continues as live tail.
// In lite mode: starts from tip, only receives new blocks.
// On any error (connection drop, watchdog timeout), waits and reconnects.
func (i *Indexer) runChainTail() error {
	hosts := i.nodeAddresses
	if len(hosts) == 0 {
		return fmt.Errorf("no node addresses configured")
	}
	fullMode := i.mode == "full" && i.leaderlogEnabled
	if i.mode == "full" && !i.leaderlogEnabled {
		log.Println("WARNING: full mode requires leaderlog.enabled; falling back to lite mode")
	}

	for {
		for _, host := range hosts {
			// Set up batch writer for nonce tracking (full mode only)
			var blockCh chan BlockData
			var writerDone chan struct{}
			if fullMode {
				blockCh = make(chan BlockData, 10000)
				writerDone = make(chan struct{})
				go func() {
					defer close(writerDone)
					batch := make([]BlockData, 0, 1000)
					ticker := time.NewTicker(2 * time.Second)
					defer ticker.Stop()
					for {
						select {
						case b, ok := <-blockCh:
							if !ok {
								if len(batch) > 0 {
									i.flushBlockBatch(batch)
								}
								return
							}
							batch = append(batch, b)
							if len(batch) >= 1000 {
								i.flushBlockBatch(batch)
								batch = batch[:0]
							}
						case <-ticker.C:
							if len(batch) > 0 {
								i.flushBlockBatch(batch)
								batch = batch[:0]
							}
						}
					}
				}()
			}

			// Build onBlock callback
			var onBlock func(slot uint64, epoch int, blockHash string, vrfOutput []byte)
			if fullMode {
				ch := blockCh // capture for closure
				onBlock = func(slot uint64, epoch int, blockHash string, vrfOutput []byte) {
					ch <- BlockData{Slot: slot, Epoch: epoch, BlockHash: blockHash, VrfOutput: vrfOutput}
				}
			}

			onCaughtUp := func() {
				log.Println("Chain sync caught up, live tail active")
			}

			syncer := NewChainSyncer(
				i.store, i.networkMagic, host,
				onBlock, i.handleLiveBlock, onCaughtUp,
				!fullMode, // startFromTip in lite mode
			)
			i.syncer = syncer

			err := syncer.Start(context.Background())

			// Clean up batch writer
			if fullMode {
				close(blockCh)
				<-writerDone
			}

			if err != nil {
				log.Printf("Chain sync error on %s: %s", host, err)
			}
		}

		log.Println("Chain sync disconnected, reconnecting in 5s...")
		time.Sleep(5 * time.Second)
	}
}

// handleLiveBlock processes a live block from the ChainSyncer after caught up.
// Replaces the old adder-based handleEvent with direct field access from NtN block headers.
func (i *Indexer) handleLiveBlock(info LiveBlockInfo) {
	now := time.Now()

	// Calculate block interval
	var interval string
	if i.prevBlockTime.IsZero() {
		interval = "first"
	} else {
		diff := now.Sub(i.prevBlockTime)
		if diff.Seconds() < 60 {
			interval = fmt.Sprintf("%.0fs", diff.Seconds())
		} else {
			minutes := int(diff.Minutes())
			seconds := int(diff.Seconds()) - (minutes * 60)
			interval = fmt.Sprintf("%dm%02ds", minutes, seconds)
		}
	}
	i.prevBlockTime = now

	// Log block line
	vrfHex := ""
	if info.VrfOutput != nil {
		vrfHex = truncHash(hex.EncodeToString(info.VrfOutput), 16)
	}
	log.Printf("[block] slot %d | hash %s | nonce %s | interval %s",
		info.Slot, truncHash(info.BlockHash, 16), vrfHex, interval)

	// Check if the epoch has changed
	currentEpoch := getCurrentEpoch()
	if currentEpoch != i.epoch {
		i.epoch = currentEpoch
		i.epochBlocks = 0
	}

	// Track VRF data for nonce evolution (use slot-derived epoch from sync.go,
	// not wall-clock i.epoch, to stay consistent with the historical-sync path)
	if i.leaderlogEnabled && info.VrfOutput != nil {
		i.nonceTracker.ProcessBlock(info.Slot, info.Epoch, info.BlockHash, info.VrfOutput)
		i.checkLeaderlogTrigger(info.Slot)
	}

	// Customize links based on network
	var cexplorerLink string
	switch i.networkMagic {
	case PreprodNetworkMagic:
		cexplorerLink = "https://preprod.cexplorer.io/block/"
	case PreviewNetworkMagic:
		cexplorerLink = "https://preview.cexplorer.io/block/"
	default:
		cexplorerLink = "https://cexplorer.io/block/"
	}

	// Pool block detection: compare 28-byte pool ID hash
	if info.IssuerVkeyHash == i.poolId {
		i.epochBlocks++
		i.totalBlocks++

		blockSizeKB := float64(info.BlockBodySize) / 1024
		sizePercentage := (blockSizeKB / fullBlockSize) * 100

		log.Printf("[MINTED] slot %d | hash %s | %.1fKB (%.0f%%) | epoch %d | lifetime %d",
			info.Slot, truncHash(info.BlockHash, 16),
			blockSizeKB, sizePercentage,
			i.epochBlocks, i.totalBlocks)

		msg := fmt.Sprintf(
			"Quack!(attention) \U0001F986\nduckBot notification!\n\n"+
				"%s\n"+"\U0001F4A5 New Block!\n\n"+
				"Block Size: %.2f KB\n"+
				"%.2f%% Full\n"+
				"Interval: %s\n\n"+
				"Epoch Blocks: %d\n"+
				"Lifetime Blocks: %d\n\n"+
				"Pooltool: https://pooltool.io/realtime/%d\n\n"+
				"Cexplorer: "+cexplorerLink+"%s",
			i.poolName, blockSizeKB, sizePercentage,
			interval, i.epochBlocks, i.totalBlocks,
			info.BlockNumber, info.BlockHash)

		// Get duck image URL for Telegram photo fallback
		duckImageURL, duckErr := getDuckImage()
		if duckErr != nil {
			log.Printf("failed to fetch duck image: %v", duckErr)
		}

		// Send Telegram notification if enabled
		if i.telegramEnabled && i.bot != nil {
			channelID, err := strconv.ParseInt(i.telegramChannel, 10, 64)
			if err != nil {
				log.Printf("failed to parse telegram channel ID: %s", err)
			} else {
				chat := &telebot.Chat{ID: channelID}
				sentSuccessfully := false

				if i.telegramGifEnabled {
					gifURL, gifErr := fetchRandomDuckGIF()
					if gifErr == nil && gifURL != "" {
						animation := &telebot.Animation{
							File:    telebot.FromURL(gifURL),
							Caption: msg,
						}
						_, err = i.bot.Send(chat, animation)
						if err != nil {
							log.Printf("failed to send Telegram GIF, falling back to photo: %s", err)
						} else {
							sentSuccessfully = true
						}
					} else {
						log.Printf("failed to fetch duck GIF, falling back to photo: %v", gifErr)
					}
				}

				if !sentSuccessfully {
					if duckImageURL != "" {
						photo := &telebot.Photo{File: telebot.FromURL(duckImageURL), Caption: msg}
						_, err = i.bot.Send(chat, photo)
						if err != nil {
							log.Printf("failed to send Telegram photo, sending text only: %s", err)
							i.bot.Send(chat, msg)
						}
					} else {
						i.bot.Send(chat, msg)
					}
				}
			}
		}

		// Send tweet if Twitter is enabled
		if i.twitterEnabled {
			tweetMsg := fmt.Sprintf(
				"\U0001F986 New Block!\n\n"+
					"Pool: %s\n"+
					"Size: %.2fKB (%.0f%% full)\n"+
					"Epoch: %d | Lifetime: %d\n\n"+
					"pooltool.io/realtime/%d",
				i.poolName, blockSizeKB, sizePercentage,
				i.epochBlocks, i.totalBlocks,
				info.BlockNumber)

			var gifURL string
			if i.twitterGifEnabled {
				fetchedGIF, gifErr := fetchRandomDuckGIF()
				if gifErr != nil {
					log.Printf("failed to fetch duck GIF for Twitter, posting text-only: %v", gifErr)
				} else {
					gifURL = fetchedGIF
				}
			}

			if err := i.sendTweet(tweetMsg, gifURL); err != nil {
				log.Printf("failed to send tweet: %s", err)
			}
		}
	}

	// Broadcast to WebSocket clients
	select {
	case broadcast <- map[string]interface{}{
		"type":        "chainsync.block",
		"slot":        info.Slot,
		"hash":        info.BlockHash,
		"blockNumber": info.BlockNumber,
		"bodySize":    info.BlockBodySize,
		"timestamp":   now.Format(time.RFC3339),
	}:
	default:
	}
}

func handleConnections(w http.ResponseWriter, r *http.Request) {
	// Upgrade initial GET request to a websocket
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Fatal(err)
	}
	// Make sure we close the connection when the function returns
	defer ws.Close()
	// Register our new client
	clients[ws] = true
	for {
		var msg interface{}
		// Read in a new message as JSON and map it to a Message object
		err := ws.ReadJSON(&msg)
		if err != nil {
			log.Printf("error: %v", err)
			delete(clients, ws)
			break
		}
		// Send the newly received message to the broadcast channel
		broadcast <- msg
	}
}

// Handle broadcasting the messages to clients
func handleMessages() {
	for {
		// Grab the next message from the broadcast channel
		msg := <-broadcast
		// Send it out to every client that is currently connected
		for client := range clients {
			err := client.WriteJSON(msg)
			if err != nil {
				log.Printf("error: %v", err)
				client.Close()
				delete(clients, client)
			}
		}
	}
}

// Get random duck image URL with timeout and safe error handling
func getDuckImage() (string, error) {
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get("https://random-d.uk/api/v2/random")
	if err != nil {
		return "", fmt.Errorf("failed to fetch duck image: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("duck API returned status %d", resp.StatusCode)
	}
	var result map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("failed to decode duck response: %w", err)
	}
	imageURL, ok := result["url"].(string)
	if !ok || imageURL == "" {
		return "", fmt.Errorf("no URL in duck API response")
	}
	return imageURL, nil
}

// fetchRandomDuckGIF fetches a random duck GIF from random-d.uk API with a timeout.
// Returns the GIF URL or an error if the fetch fails.
func fetchRandomDuckGIF() (string, error) {
	client := &http.Client{
		Timeout: 5 * time.Second,
	}

	resp, err := client.Get("https://random-d.uk/api/random?type=gif")
	if err != nil {
		return "", fmt.Errorf("failed to fetch duck GIF: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("duck GIF API returned status %d", resp.StatusCode)
	}

	var result map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("failed to decode duck GIF response: %w", err)
	}

	gifURL, ok := result["url"].(string)
	if !ok || gifURL == "" {
		return "", fmt.Errorf("no GIF URL in response")
	}

	return gifURL, nil
}

// Download image from URL to bytes
func downloadImage(imageURL string) ([]byte, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(imageURL)
	if err != nil {
		return nil, fmt.Errorf("failed to download image: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to download image: status %d", resp.StatusCode)
	}

	imageData, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read image data: %w", err)
	}

	return imageData, nil
}

// uploadMediaToTwitter uploads media to Twitter using the chunked upload API.
// Returns the media_id string on success, or an error.
func (i *Indexer) uploadMediaToTwitter(mediaBytes []byte, mediaType upload_types.MediaType, mediaCategory upload_types.MediaCategory) (string, error) {
	if !i.twitterEnabled || i.twitterClient == nil {
		return "", fmt.Errorf("twitter client not initialized")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Step 1: Initialize upload
	initInput := &upload_types.InitializeInput{
		MediaType:     mediaType,
		MediaCategory: mediaCategory,
		TotalBytes:    len(mediaBytes),
	}

	initOutput, err := upload.Initialize(ctx, i.twitterClient, initInput)
	if err != nil {
		return "", fmt.Errorf("failed to initialize media upload: %w", err)
	}

	if initOutput.Data.MediaID == "" {
		return "", fmt.Errorf("initialize returned no media ID")
	}

	mediaID := initOutput.Data.MediaID

	// Step 2: Append media data (single chunk for images/GIFs)
	appendInput := &upload_types.AppendInput{
		MediaID:      mediaID,
		Media:        bytes.NewReader(mediaBytes),
		SegmentIndex: 0,
	}

	_, err = upload.Append(ctx, i.twitterClient, appendInput)
	if err != nil {
		return "", fmt.Errorf("failed to append media data: %w", err)
	}

	// Step 3: Finalize upload
	finalizeInput := &upload_types.FinalizeInput{
		MediaID: mediaID,
	}

	finalizeOutput, err := upload.Finalize(ctx, i.twitterClient, finalizeInput)
	if err != nil {
		return "", fmt.Errorf("failed to finalize media upload: %w", err)
	}

	if finalizeOutput.Data.MediaID == "" {
		return "", fmt.Errorf("finalize returned no media ID")
	}

	return finalizeOutput.Data.MediaID, nil
}

// Send tweet with optional GIF
func (i *Indexer) sendTweet(text string, gifURL string) error {
	if !i.twitterEnabled || i.twitterClient == nil {
		return nil
	}

	// Create tweet request
	input := &types.CreateInput{
		Text: gotwi.String(text),
	}

	// If GIF URL is provided, download and upload it
	if gifURL != "" {
		gifBytes, err := downloadImage(gifURL)
		if err != nil {
			log.Printf("failed to download GIF, posting text-only tweet: %v", err)
		} else {
			// Upload GIF to Twitter
			mediaID, uploadErr := i.uploadMediaToTwitter(
				gifBytes,
				upload_types.MediaTypeGIF,
				upload_types.MediaCategoryTweetGIF,
			)
			if uploadErr != nil {
				log.Printf("failed to upload GIF to Twitter, posting text-only tweet: %v", uploadErr)
			} else {
				// Attach media to tweet
				input.Media = &types.CreateInputMedia{
					MediaIDs: []string{mediaID},
				}
				log.Printf("GIF uploaded successfully, media_id: %s", mediaID)
			}
		}
	}

	// Post the tweet (with or without media)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := managetweet.Create(ctx, i.twitterClient, input)
	if err != nil {
		return fmt.Errorf("failed to post tweet: %w", err)
	}

	log.Println("Tweet posted successfully")
	return nil
}

// convertToBech32 converts a hex pool ID to bech32 format using gouroboros.
func convertToBech32(hash string) (string, error) {
	bytes, err := hex.DecodeString(hash)
	if err != nil {
		return "", err
	}
	poolId := ledger.NewBlake2b224(bytes)
	return poolId.Bech32("pool"), nil
}

// getEpochProgress returns the percentage progress through the current epoch based on slot position.
func (i *Indexer) getEpochProgress(slot uint64) float64 {
	epochStartSlot := GetEpochStartSlot(i.epoch, i.networkMagic)
	epochLen := GetEpochLength(i.networkMagic)
	if epochLen == 0 || slot < epochStartSlot {
		return 0
	}
	return float64(slot-epochStartSlot) / float64(epochLen) * 100
}

// checkLeaderlogTrigger checks if we should calculate the leader schedule.
// Triggers when slot passes the stability window (60% of epoch = 259200 slots on mainnet).
func (i *Indexer) checkLeaderlogTrigger(slot uint64) {
	epochStartSlot := GetEpochStartSlot(i.epoch, i.networkMagic)
	stabilitySlot := epochStartSlot + StabilityWindowSlots(i.networkMagic)
	pastStability := slot >= stabilitySlot

	// Freeze candidate nonce at stability window
	if pastStability {
		i.nonceTracker.FreezeCandidate(i.epoch)
	}

	// Calculate leader schedule for next epoch after stability window
	nextEpoch := i.epoch + 1
	if pastStability {
		i.leaderlogMu.Lock()
		if i.leaderlogCalcing[nextEpoch] {
			i.leaderlogMu.Unlock()
			return
		}
		// Skip if schedule already cached as existing (no DB hit per block)
		if i.scheduleExists[nextEpoch] {
			i.leaderlogMu.Unlock()
			return
		}
		// Check DB once per epoch (with timeout), then cache the result
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		existing, err := i.store.GetLeaderSchedule(ctx, nextEpoch)
		cancel()
		if err == nil && existing != nil {
			i.scheduleExists[nextEpoch] = true
			i.leaderlogMu.Unlock()
			return
		}
		// Cooldown: don't retry for 15 minutes after a failed attempt
		if failedAt, ok := i.leaderlogFailed[nextEpoch]; ok && time.Since(failedAt) < 15*time.Minute {
			i.leaderlogMu.Unlock()
			return
		}
		i.leaderlogCalcing[nextEpoch] = true
		i.leaderlogMu.Unlock()

		go func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("leaderlog goroutine panic for epoch %d: %v", nextEpoch, r)
					i.leaderlogMu.Lock()
					delete(i.leaderlogCalcing, nextEpoch)
					i.leaderlogFailed[nextEpoch] = time.Now()
					i.leaderlogMu.Unlock()
				}
			}()
			success := i.calculateAndPostLeaderlog(nextEpoch)
			i.leaderlogMu.Lock()
			delete(i.leaderlogCalcing, nextEpoch)
			if !success {
				i.leaderlogFailed[nextEpoch] = time.Now()
			} else {
				delete(i.leaderlogFailed, nextEpoch)
				i.scheduleExists[nextEpoch] = true
			}
			i.leaderlogMu.Unlock()
		}()
	}
}

// calculateAndPostLeaderlog calculates the leader schedule and posts to Telegram.
// Returns true on success, false on failure (used for cooldown tracking).
func (i *Indexer) calculateAndPostLeaderlog(epoch int) bool {
	log.Printf("Calculating leader schedule for epoch %d...", epoch)

	// Wait a bit for data to settle
	time.Sleep(30 * time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	// Get epoch nonce (local first, Koios fallback)
	epochNonce, err := i.nonceTracker.GetNonceForEpoch(epoch)
	if err != nil {
		log.Printf("Failed to get nonce for epoch %d: %v", epoch, err)
		return false
	}

	// Get pool + network stake (NtC mark snapshot first, 60s timeout, Koios fallback)
	var poolStake, totalStake uint64
	if i.nodeQuery != nil {
		ntcCtx, ntcCancel := context.WithTimeout(ctx, 60*time.Second)
		snapshots, snapErr := i.nodeQuery.QueryPoolStakeSnapshots(ntcCtx, i.bech32PoolId)
		ntcCancel()
		if snapErr != nil {
			log.Printf("NtC stake query failed for auto-leaderlog (60s timeout), trying Koios: %v", snapErr)
		} else {
			poolStake = snapshots.PoolStakeMark
			totalStake = snapshots.TotalStakeMark
		}
	}
	if poolStake == 0 && i.koios != nil {
		// Koios fallback: try recent completed epochs (Koios may lag 1-2 epochs behind)
		for _, tryEpoch := range []int{epoch - 1, epoch - 2, epoch - 3} {
			epochNo := koios.EpochNo(tryEpoch)
			poolHist, histErr := i.koios.GetPoolHistory(ctx, koios.PoolID(i.bech32PoolId), &epochNo, nil)
			if histErr != nil {
				log.Printf("Koios GetPoolHistory(%d) error: %v", tryEpoch, histErr)
				continue
			}
			if len(poolHist.Data) == 0 {
				log.Printf("Koios GetPoolHistory(%d) returned empty data", tryEpoch)
				continue
			}
			poolStake = uint64(poolHist.Data[0].ActiveStake.IntPart())
			epochInfo, infoErr := i.koios.GetEpochInfo(ctx, &epochNo, nil)
			if infoErr != nil || len(epochInfo.Data) == 0 {
				log.Printf("Koios GetEpochInfo(%d) failed: %v", tryEpoch, infoErr)
				poolStake = 0
				continue
			}
			totalStake = uint64(epochInfo.Data[0].ActiveStake.IntPart())
			log.Printf("Using Koios stake fallback for epoch %d leaderlog (from epoch %d)", epoch, tryEpoch)
			break
		}
	}
	if poolStake == 0 || totalStake == 0 {
		log.Printf("No stake data available for epoch %d (pool=%d, total=%d)", epoch, poolStake, totalStake)
		return false
	}

	// Calculate schedule
	epochLength := GetEpochLength(i.networkMagic)
	epochStartSlot := GetEpochStartSlot(epoch, i.networkMagic)
	slotToTimeFn := makeSlotToTime(i.networkMagic)

	schedule, err := CalculateLeaderSchedule(
		epoch, epochNonce, i.vrfKey,
		poolStake, totalStake,
		epochLength, epochStartSlot, slotToTimeFn,
	)
	if err != nil {
		log.Printf("Failed to calculate leader schedule for epoch %d: %v", epoch, err)
		return false
	}

	log.Printf("Epoch %d: %d slots assigned (expected %.2f)",
		epoch, len(schedule.AssignedSlots), schedule.IdealSlots)

	// Store in database
	if err := i.store.InsertLeaderSchedule(ctx, schedule); err != nil {
		log.Printf("Failed to store leader schedule: %v", err)
	}

	// Post to Telegram if enabled
	if i.telegramEnabled && i.bot != nil {
		msg := FormatScheduleForTelegram(schedule, i.poolName, i.leaderlogTZ)

		channelID, err := strconv.ParseInt(i.telegramChannel, 10, 64)
		if err != nil {
			log.Printf("Failed to parse telegram channel ID: %v", err)
			return false
		}

		chat := &telebot.Chat{ID: channelID}
		// Telegram messages are limited to 4096 characters
		for len(msg) > 0 {
			chunk := msg
			if len(chunk) > 4096 {
				// Split at last newline before 4096
				cut := 4096
				for cut > 0 && chunk[cut-1] != '\n' {
					cut--
				}
				if cut == 0 {
					cut = 4096
				}
				chunk = msg[:cut]
				msg = msg[cut:]
			} else {
				msg = ""
			}
			if _, sendErr := i.bot.Send(chat, chunk); sendErr != nil {
				log.Printf("Failed to send leaderlog to Telegram: %v", sendErr)
				return false
			}
		}
	}

	// Mark as posted
	if err := i.store.MarkSchedulePosted(ctx, epoch); err != nil {
		log.Printf("Failed to mark schedule as posted: %v", err)
	}

	log.Printf("Leader schedule for epoch %d posted", epoch)
	return true
}

// makeSlotToTime returns a function that converts a slot number to a time.Time.
func makeSlotToTime(networkMagic int) func(uint64) time.Time {
	shelleyStart, _ := time.Parse(time.RFC3339, ShelleyEpochStart)

	// Byron-Shelley boundary slot count
	byronSlots := uint64(ShelleyStartEpoch) * ByronEpochLength // mainnet: 4492800

	switch networkMagic {
	case MainnetNetworkMagic:
		return func(slot uint64) time.Time {
			if slot < byronSlots {
				return shelleyStart.Add(-time.Duration(byronSlots-slot) * 20 * time.Second)
			}
			return shelleyStart.Add(time.Duration(slot-byronSlots) * time.Second)
		}

	case PreprodNetworkMagic:
		genesis, _ := time.Parse(time.RFC3339, "2022-06-01T00:00:00Z")
		preprodByronSlots := uint64(PreprodShelleyStartEpoch) * ByronEpochLength // 86400
		return func(slot uint64) time.Time {
			if slot < preprodByronSlots {
				return genesis.Add(time.Duration(slot) * 20 * time.Second)
			}
			byronDuration := time.Duration(preprodByronSlots) * 20 * time.Second
			shelleySlots := slot - preprodByronSlots
			return genesis.Add(byronDuration + time.Duration(shelleySlots)*time.Second)
		}

	case PreviewNetworkMagic:
		genesis, _ := time.Parse(time.RFC3339, "2022-11-01T00:00:00Z")
		return func(slot uint64) time.Time {
			return genesis.Add(time.Duration(slot) * time.Second)
		}

	default:
		return func(slot uint64) time.Time {
			return shelleyStart.Add(time.Duration(slot) * time.Second)
		}
	}
}

// formatNumber formats an integer with comma separators (e.g., 1234567 -> "1,234,567").
// truncHash returns the first n characters of a hex string.
func truncHash(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func formatNumber(n int64) string {
	if n < 0 {
		return "-" + formatNumber(-n)
	}
	s := strconv.FormatInt(n, 10)
	if len(s) <= 3 {
		return s
	}

	result := make([]byte, 0, len(s)+(len(s)-1)/3)
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			result = append(result, ',')
		}
		result = append(result, byte(c))
	}
	return string(result)
}

// Main function to start goduckbot
func main() {

	// Configure websocket route and start broadcast handler BEFORE Start()
	// (Start blocks in full mode; broadcast channel must be drained)
	http.HandleFunc("/ws", handleConnections)
	go handleMessages()
	go func() {
		log.Println("http server started on :8081")
		if err := http.ListenAndServe("localhost:8081", nil); err != nil {
			log.Printf("ListenAndServe: %s", err)
		}
	}()

	// Start the indexer (blocks in full mode until sync + live tail complete)
	if err := globalIndexer.Start(); err != nil {
		log.Fatalf("failed to start: %s", err)
	}

	// Wait for all goroutines to finish before exiting
	globalIndexer.wg.Wait()
	select {}
}
