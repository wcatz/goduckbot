package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/blinklabs-io/adder/event"
	filter_event "github.com/blinklabs-io/adder/filter/event"
	"github.com/blinklabs-io/adder/input/chainsync"
	output_embedded "github.com/blinklabs-io/adder/output/embedded"
	"github.com/blinklabs-io/adder/pipeline"
	"github.com/blinklabs-io/gouroboros/ledger"
	"github.com/blinklabs-io/gouroboros/ledger/allegra"
	"github.com/blinklabs-io/gouroboros/ledger/alonzo"
	"github.com/blinklabs-io/gouroboros/ledger/babbage"
	"github.com/blinklabs-io/gouroboros/ledger/conway"
	"github.com/blinklabs-io/gouroboros/ledger/mary"
	"github.com/blinklabs-io/gouroboros/ledger/shelley"
	koios "github.com/cardano-community/koios-go-client/v3"
	"github.com/cenkalti/backoff/v4"
	"github.com/gorilla/websocket"
	"github.com/michimani/gotwi"
	"github.com/michimani/gotwi/media/upload"
	upload_types "github.com/michimani/gotwi/media/upload/types"
	"github.com/michimani/gotwi/tweet/managetweet"
	"github.com/michimani/gotwi/tweet/managetweet/types"
	"github.com/spf13/viper"
	telebot "gopkg.in/tucnak/telebot.v2"
)

// Build-time version metadata (set via -ldflags)
var (
	version   = "dev"
	commitSHA = "unknown"
	buildDate = "unknown"
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

// Block interval tracking for adder live tail
var prevBlockTimestamp time.Time
var timeDiffString string

// Tracks goroutines leaked when pipeline.Stop() times out
var abandonedPipelines int64 // atomic

// Indexer struct to manage the adder pipeline and block events
type Indexer struct {
	pipeline        *pipeline.Pipeline
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
	// Mode: "lite" (adder tail + Koios) or "full" (historical sync + adder tail)
	mode string
	// Duck media settings
	duckMedia     string // "gif", "image", or "both" (default)
	duckCustomUrl string // custom override URL
	// Social network toggles
	telegramEnabled bool
	twitterEnabled  bool
	twitterClient   *gotwi.Client
	// Bot command access control
	allowedUsers  map[int64]bool
	allowedGroups map[int64]bool
	nodeQuery     *NodeQueryClient
	// Leaderlog fields
	vrfKey              *VRFKey
	leaderlogEnabled    bool
	leaderlogTZ         string
	leaderlogTimeFormat string
	store            Store
	nonceTracker     *NonceTracker
	leaderlogMu      sync.Mutex
	leaderlogCalcing map[int]bool      // epochs currently being calculated
	leaderlogFailed  map[int]time.Time // cooldown: epoch -> last failure time
	scheduleExists   map[int]bool      // cached: epoch -> schedule already in DB
	syncer           *ChainSyncer // nil in lite mode
	lastBlockTime    int64        // atomic: unix timestamp of last block received
}

type BlockEvent struct {
	Type      string             `json:"type"`
	Timestamp string             `json:"timestamp"`
	Context   event.BlockContext `json:"context"`
	Payload   event.BlockEvent   `json:"payload"`
}

// getCurrentEpoch calculates the current epoch number based on networkMagic.
func (i *Indexer) getCurrentEpoch() int {
	now := time.Now().UTC()

	switch i.networkMagic {
	case PreprodNetworkMagic:
		genesis, _ := time.Parse(time.RFC3339, "2022-06-01T00:00:00Z")
		elapsed := now.Sub(genesis).Seconds()
		byronDuration := float64(PreprodShelleyStartEpoch) * float64(ByronEpochLength) * 20 // 20s Byron slots
		if elapsed < byronDuration {
			return int(elapsed / (float64(ByronEpochLength) * 20))
		}
		shelleySeconds := elapsed - byronDuration
		return PreprodShelleyStartEpoch + int(shelleySeconds/float64(MainnetEpochLength))

	case PreviewNetworkMagic:
		genesis, _ := time.Parse(time.RFC3339, "2022-11-01T00:00:00Z")
		elapsed := now.Sub(genesis).Seconds()
		return int(elapsed / float64(PreviewEpochLength))

	default: // mainnet
		shelleyStartTime, _ := time.Parse(time.RFC3339, ShelleyEpochStart)
		elapsedSeconds := now.Sub(shelleyStartTime).Seconds()
		epochsElapsed := int(elapsedSeconds / (EpochDurationInDays * SecondsInDay))
		return StartingEpoch + epochsElapsed
	}
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
	i.duckMedia = viper.GetString("duck.media")
	if i.duckMedia == "" {
		i.duckMedia = "both"
	}
	i.duckCustomUrl = viper.GetString("duck.customUrl")
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
		ntcQueryTimeout := viper.GetDuration("leaderlog.ntcQueryTimeout")
		i.nodeQuery = NewNodeQueryClient(ntcHost, i.networkMagic, ntcQueryTimeout)
		log.Printf("Node query client initialized (NtC): %s://%s (timeout: %v)", network, address, i.nodeQuery.queryTimeout)
	}

	// Twitter toggle
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

	i.epoch = i.getCurrentEpoch()
	log.Printf("Epoch: %d", i.epoch)

	// Initialize leaderlog
	i.leaderlogEnabled = viper.GetBool("leaderlog.enabled")
	i.leaderlogTZ = viper.GetString("leaderlog.timezone")
	if i.leaderlogTZ == "" {
		i.leaderlogTZ = "UTC"
	}
	i.leaderlogTimeFormat = viper.GetString("leaderlog.timeFormat")
	if i.leaderlogTimeFormat != "24h" {
		i.leaderlogTimeFormat = "12h"
	}

	// Load VRF key: prefer vrfKeyValue (inline CBOR hex), fall back to vrfKeyPath (file)
	vrfKeyValue := viper.GetString("leaderlog.vrfKeyValue")
	if vrfKeyValue != "" {
		vrfKey, vrfErr := ParseVRFKeyCborHex(vrfKeyValue)
		if vrfErr != nil {
			log.Fatalf("failed to parse leaderlog.vrfKeyValue: %s", vrfErr)
		}
		i.vrfKey = vrfKey
		i.leaderlogEnabled = true
		log.Println("Leaderlog enabled, VRF key loaded from vrfKeyValue")
	} else if i.leaderlogEnabled {
		vrfKeyPath := viper.GetString("leaderlog.vrfKeyPath")
		vrfKey, vrfErr := ParseVRFKeyFile(vrfKeyPath)
		if vrfErr != nil {
			log.Fatalf("failed to parse VRF key from %s: %s", vrfKeyPath, vrfErr)
		}
		i.vrfKey = vrfKey
		log.Printf("Leaderlog enabled, VRF key loaded from %s", vrfKeyPath)
	}

	if i.leaderlogEnabled {
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

		// Nonce backfill runs after historical sync (see runChainTail)
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

// runChainTail runs historical sync (full mode) then starts adder pipeline for live tail.
// Full mode: gouroboros historical sync → caught up → adder pipeline (live tail)
// Lite mode: adder pipeline only (intersect at tip)
func (i *Indexer) runChainTail() error {
	fullMode := i.mode == "full" && i.leaderlogEnabled
	if i.mode == "full" && !i.leaderlogEnabled {
		log.Println("WARNING: full mode requires leaderlog.enabled; falling back to lite mode")
	}

	// Full mode: run historical sync before starting adder pipeline
	if fullMode && len(i.nodeAddresses) > 0 {
		log.Println("Starting historical chain sync...")
		syncCtx, syncCancel := context.WithCancel(context.Background())
		defer syncCancel()

		// Buffered channel decouples fast chain sync from slower DB writes
		blockCh := make(chan BlockData, 10000)

		onCaughtUp := func() {
			log.Println("Historical sync caught up, stopping ChainSyncer...")
			syncCancel() // stop ChainSyncer so Start() returns and adder takes over
		}

		// DB writer goroutine — drains channel in batches for throughput
		writerDone := make(chan struct{})
		go func() {
			defer close(writerDone)
			batch := make([]BlockData, 0, 1000)
			ticker := time.NewTicker(2 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case b, ok := <-blockCh:
					if !ok {
						// Channel closed — flush remaining
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

		// Retry loop — on keep-alive timeout, reconnect and resume from GetLastSyncedSlot
		maxRetries := 10
		for attempt := 1; attempt <= maxRetries; attempt++ {
			i.syncer = NewChainSyncer(
				i.store,
				i.networkMagic,
				i.nodeAddresses[0],
				func(slot uint64, epoch int, blockHash string, vrfOutput []byte) {
					blockCh <- BlockData{Slot: slot, Epoch: epoch, BlockHash: blockHash, VrfOutput: vrfOutput, NetworkMagic: i.networkMagic}
				},
				onCaughtUp,
			)

			if err := i.syncer.Start(syncCtx); err != nil {
				if syncCtx.Err() != nil {
					// Context canceled by onCaughtUp — sync is done
					break
				}
				log.Printf("Historical sync error (attempt %d/%d): %s", attempt, maxRetries, err)
				if attempt < maxRetries {
					// Drain channel buffer — old syncer is dead so no new sends.
					// Wait for writer goroutine to flush all buffered blocks to DB.
					for len(blockCh) > 0 {
						time.Sleep(100 * time.Millisecond)
					}
					// Give writer time to finish flushing the current in-flight batch
					time.Sleep(3 * time.Second)

					// Resync NonceTracker from DB so in-memory state matches persisted state.
					// Without this, the evolving nonce diverges when buffered blocks from
					// the dead connection overlap with blocks from the new connection.
					i.nonceTracker.ResyncFromDB()

					time.Sleep(time.Duration(attempt) * 5 * time.Second)
					continue
				}
				log.Printf("Historical sync failed after %d attempts, falling through to adder", maxRetries)
			}
			break
		}

		close(blockCh)
		<-writerDone // wait for DB writer to flush
		log.Println("Historical sync complete, transitioning to live tail...")

		// Run nonce backfill after sync so blocks table has data
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
			defer cancel()

			if err := i.nonceTracker.BackfillNonces(ctx); err != nil {
				log.Printf("Nonce backfill failed: %v", err)
				return
			}

			if viper.GetBool("leaderlog.nonceIntegrityCheck") {
				report, err := i.nonceTracker.NonceIntegrityCheck(ctx)
				if err != nil {
					log.Printf("Nonce integrity check failed: %v", err)
				} else if report.KoiosMismatched > 0 {
					log.Printf("WARNING: %d epoch nonce mismatches detected!", report.KoiosMismatched)
				}
			}

			if viper.GetBool("leaderlog.backfillSchedules") {
				if err := i.backfillSchedules(ctx); err != nil {
					log.Printf("Schedule backfill failed: %v", err)
				}
			}
		}()
	}

	// Start adder pipeline for live chain tail (both full and lite mode)
	return i.startAdderPipeline()
}

// startAdderPipeline starts the adder pipeline for live chain tail.
// It runs an infinite restart loop: on pipeline error or stall, it stops, waits, and reconnects.
// Auto-reconnect is disabled because adder orphans the event channel after reconnect.
// A stall detector goroutine monitors lastBlockTime and forces a restart if no blocks
// arrive for 2 minutes (catches zombie state where pipeline.Stop() previously hung).
func (i *Indexer) startAdderPipeline() error {
	hosts := i.nodeAddresses

	for {
		connected := false
		for _, host := range hosts {
			bo := backoff.NewExponentialBackOff()
			bo.MaxElapsedTime = maxRetryDuration

			startPipelineFunc := func() error {
				node := chainsync.WithAddress(host)
				inputOpts := []chainsync.ChainSyncOptionFunc{
					node,
					chainsync.WithNetworkMagic(uint32(i.networkMagic)),
					chainsync.WithIntersectTip(true),
					chainsync.WithAutoReconnect(false),
					chainsync.WithIncludeCbor(false),
				}

				i.pipeline = pipeline.New()
				input_chainsync := chainsync.New(inputOpts...)
				i.pipeline.AddInput(input_chainsync)

				filterEvent := filter_event.New(filter_event.WithTypes([]string{"chainsync.block"}))
				i.pipeline.AddFilter(filterEvent)

				output := output_embedded.New(output_embedded.WithCallbackFunc(i.handleEvent))
				i.pipeline.AddOutput(output)

				// Reset interval tracking before Start() spawns event goroutines
				prevBlockTimestamp = time.Time{}

				err := i.pipeline.Start()
				if err != nil {
					log.Printf("Failed to start pipeline on %s: %s. Retrying...", host, err)
					return err
				}

				return nil
			}

			err := backoff.Retry(startPipelineFunc, bo)
			if err != nil {
				log.Printf("Failed to connect to node at %s after retries: %s", host, err)
				continue
			}

			log.Printf("Pipeline connected to node at %s", host)
			connected = true
			atomic.StoreInt64(&i.lastBlockTime, time.Now().Unix())

			// Start stall detector — forces restart if no blocks for 2 minutes.
			// This catches the zombie state where pipeline.Stop() hangs or the
			// pipeline silently stops delivering blocks after a reconnect.
			stallCh := make(chan struct{})
			stallDone := make(chan struct{})
			go func() {
				defer close(stallDone)
				ticker := time.NewTicker(30 * time.Second)
				defer ticker.Stop()
				for {
					select {
					case <-ticker.C:
						lastSeen := atomic.LoadInt64(&i.lastBlockTime)
						if lastSeen > 0 {
							stale := time.Since(time.Unix(lastSeen, 0))
							if stale > 2*time.Minute {
								log.Printf("Pipeline stall detected (no blocks for %s), forcing restart",
									stale.Round(time.Second))
								close(stallCh)
								return
							}
						}
					case <-stallCh:
						return
					}
				}
			}()

			// Block until pipeline error OR stall detected
			select {
			case pipelineErr := <-i.pipeline.ErrorChan():
				log.Printf("Pipeline error: %s", pipelineErr)
			case <-stallCh:
				// Stall detector already logged
			}

			// Signal stall detector to stop (no-op if it already closed stallCh)
			select {
			case <-stallCh:
				// Already closed
			default:
				close(stallCh)
			}
			<-stallDone

			// Stop the dead pipeline with timeout to prevent hanging forever.
			// pipeline.Stop() can block indefinitely if the underlying connection
			// is in a broken state (the root cause of the zombie bug).
			stopDone := make(chan struct{})
			go func() {
				if stopErr := i.pipeline.Stop(); stopErr != nil {
					log.Printf("Pipeline stop error: %s", stopErr)
				}
				close(stopDone)
			}()
			select {
			case <-stopDone:
				// Clean shutdown
			case <-time.After(5 * time.Second):
				n := atomic.AddInt64(&abandonedPipelines, 1)
				log.Printf("Pipeline stop timed out, abandoning old pipeline (leaked: %d)", n)
			}

			break // break inner host loop, restart from outer loop
		}

		if !connected {
			log.Println("Failed to connect to any host, retrying all in 30s...")
			time.Sleep(30 * time.Second)
		} else {
			log.Println("Pipeline died, restarting in 5s...")
			time.Sleep(5 * time.Second)
		}
	}
}

// handleEvent processes a block event from the adder pipeline (live tail).
func (i *Indexer) handleEvent(evt event.Event) error {
	// Update liveness tracker for stall detection
	atomic.StoreInt64(&i.lastBlockTime, time.Now().Unix())

	// Extract VRF output before JSON marshal (Block field is json:"-")
	var vrfOutput []byte
	if i.leaderlogEnabled {
		if be, ok := evt.Payload.(event.BlockEvent); ok && be.Block != nil {
			vrfOutput = extractVrfOutput(be.Block.Header())
		}
	}

	// Marshal the event to JSON
	data, err := json.Marshal(evt)
	if err != nil {
		log.Printf("error marshalling event, skipping: %v", err)
		return nil
	}

	// Unmarshal the event to get the block event details
	var blockEvent BlockEvent
	err = json.Unmarshal(data, &blockEvent)
	if err != nil {
		log.Printf("error unmarshalling block event, skipping: %v", err)
		return nil
	}

	// Convert the block event timestamp to time.Time
	blockEventTime, err := time.Parse(time.RFC3339, blockEvent.Timestamp)
	if err != nil {
		log.Printf("error parsing block event timestamp, skipping: %v", err)
		return nil
	}

	// Calculate the time difference between the current block event and the previous one
	if prevBlockTimestamp.IsZero() {
		timeDiffString = "first"
	} else {
		timeDiff := blockEventTime.Sub(prevBlockTimestamp)
		if timeDiff.Seconds() < 60 {
			timeDiffString = fmt.Sprintf("%.0fs", timeDiff.Seconds())
		} else {
			minutes := int(timeDiff.Minutes())
			seconds := int(timeDiff.Seconds()) - (minutes * 60)
			timeDiffString = fmt.Sprintf("%dm%02ds", minutes, seconds)
		}
	}

	// Update the previous block event timestamp with the current one
	prevBlockTimestamp = blockEventTime

	// Log clean block line: slot, hash, nonce (VRF output)
	vrfHex := ""
	if vrfOutput != nil {
		vrfHex = hex.EncodeToString(vrfOutput)
	}
	log.Printf("[block] slot %d | hash %s | nonce %s | interval %s",
		blockEvent.Context.SlotNumber,
		blockEvent.Payload.BlockHash,
		vrfHex, timeDiffString)

	// Customize links based on the network magic number
	var cexplorerLink string
	switch i.networkMagic {
	case PreprodNetworkMagic:
		cexplorerLink = "https://preprod.cexplorer.io/block/"
	case PreviewNetworkMagic:
		cexplorerLink = "https://preview.cexplorer.io/block/"
	default:
		cexplorerLink = "https://cexplorer.io/block/"
	}

	// Check if the epoch has changed
	currentEpoch := i.getCurrentEpoch()
	if currentEpoch != i.epoch {
		i.epoch = currentEpoch
		i.epochBlocks = 0
	}

	// Track VRF data for nonce evolution
	if i.leaderlogEnabled && vrfOutput != nil {
		i.nonceTracker.ProcessBlock(
			blockEvent.Context.SlotNumber,
			i.epoch,
			blockEvent.Payload.BlockHash,
			vrfOutput,
		)
		i.checkLeaderlogTrigger(blockEvent.Context.SlotNumber)
	}

	// If the block event is from the pool, process it
	if blockEvent.Payload.IssuerVkey == i.poolId {
		i.epochBlocks++
		i.totalBlocks++

		blockSizeKB := float64(blockEvent.Payload.BlockBodySize) / 1024
		sizePercentage := (blockSizeKB / fullBlockSize) * 100

		log.Printf("[MINTED] slot %d | hash %s | txs %d | %.1fKB (%.0f%%) | epoch %d | lifetime %d",
			blockEvent.Context.SlotNumber,
			truncHash(blockEvent.Payload.BlockHash, 16),
			blockEvent.Payload.TransactionCount,
			blockSizeKB, sizePercentage,
			i.epochBlocks, i.totalBlocks)

		msg := fmt.Sprintf(
			"Quack!(attention) \U0001F986\nduckBot notification!\n\n"+
				"%s\n"+"\U0001F4A5 New Block!\n\n"+
				"Tx Count: %d\n"+
				"Block Size: %.2f KB\n"+
				"%.2f%% Full\n"+
				"Interval: %s\n\n"+
				"Epoch Blocks: %d\n"+
				"Lifetime Blocks: %d\n\n"+
				"Pooltool: https://pooltool.io/realtime/%d\n\n"+
				"Cexplorer: "+cexplorerLink+"%s",
			i.poolName, blockEvent.Payload.TransactionCount, blockSizeKB, sizePercentage,
			timeDiffString, i.epochBlocks, i.totalBlocks,
			blockEvent.Context.BlockNumber, blockEvent.Payload.BlockHash)

		// Get duck media for notifications
		mediaURL, isGif, mediaErr := i.getDuckMedia()
		if mediaErr != nil {
			log.Printf("failed to fetch duck media: %v", mediaErr)
		}

		// Send Telegram notification if enabled
		if i.telegramEnabled && i.bot != nil {
			channelID, err := strconv.ParseInt(i.telegramChannel, 10, 64)
			if err != nil {
				log.Printf("failed to parse telegram channel ID: %s", err)
			} else {
				chat := &telebot.Chat{ID: channelID}
				sent := false
				if mediaURL != "" {
					if isGif {
						animation := &telebot.Animation{File: telebot.FromURL(mediaURL), Caption: msg}
						if _, err = i.bot.Send(chat, animation); err != nil {
							log.Printf("failed to send Telegram GIF: %s", err)
						} else {
							sent = true
						}
					} else {
						photo := &telebot.Photo{File: telebot.FromURL(mediaURL), Caption: msg}
						if _, err = i.bot.Send(chat, photo); err != nil {
							log.Printf("failed to send Telegram photo: %s", err)
						} else {
							sent = true
						}
					}
				}
				if !sent {
					i.bot.Send(chat, msg)
				}
			}
		}

		// Send tweet if Twitter is enabled (same format as Telegram minus links)
		if i.twitterEnabled {
			tweetMsg := fmt.Sprintf(
				"Quack!(attention) \U0001F986\nduckBot notification!\n\n"+
					"%s\n"+"\U0001F4A5 New Block!\n\n"+
					"Tx Count: %d\n"+
					"Block Size: %.2f KB\n"+
					"%.2f%% Full\n"+
					"Interval: %s\n\n"+
					"Epoch Blocks: %d\n"+
					"Lifetime Blocks: %d",
				i.poolName, blockEvent.Payload.TransactionCount, blockSizeKB, sizePercentage,
				timeDiffString, i.epochBlocks, i.totalBlocks)

			if err := i.sendTweet(tweetMsg, mediaURL, isGif); err != nil {
				log.Printf("failed to send tweet: %s", err)
			}
		}
	}

	// Send the block event to the WebSocket clients (non-blocking)
	select {
	case broadcast <- blockEvent:
	default:
	}

	return nil
}

// extractVrfOutput extracts VRF output from a ledger.BlockHeader (adder live tail).
func extractVrfOutput(header ledger.BlockHeader) []byte {
	switch h := header.(type) {
	case *conway.ConwayBlockHeader:
		return h.Body.VrfResult.Output
	case *babbage.BabbageBlockHeader:
		return h.Body.VrfResult.Output
	case *alonzo.AlonzoBlockHeader:
		return h.Body.NonceVrf.Output
	case *mary.MaryBlockHeader:
		return h.Body.NonceVrf.Output
	case *allegra.AllegraBlockHeader:
		return h.Body.NonceVrf.Output
	case *shelley.ShelleyBlockHeader:
		return h.Body.NonceVrf.Output
	default:
		log.Printf("Could not extract VRF from header type %T", header)
		return nil
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

// getDuckMedia returns a duck media URL and whether it's a GIF.
// Uses customUrl if set, otherwise fetches from random-d.uk based on media setting.
func (i *Indexer) getDuckMedia() (string, bool, error) {
	if i.duckCustomUrl != "" {
		isGif := strings.HasSuffix(strings.ToLower(i.duckCustomUrl), ".gif")
		return i.duckCustomUrl, isGif, nil
	}

	endpoint := "https://random-d.uk/api/v2/random"
	switch i.duckMedia {
	case "gif":
		endpoint += "?type=gif"
	case "image":
		endpoint += "?type=jpg"
	default: // "both" — random-d.uk picks randomly, but we can also randomize
		if rand.Intn(2) == 0 {
			endpoint += "?type=gif"
		} else {
			endpoint += "?type=jpg"
		}
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(endpoint)
	if err != nil {
		return "", false, fmt.Errorf("failed to fetch duck media: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", false, fmt.Errorf("duck API returned status %d", resp.StatusCode)
	}
	var result map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", false, fmt.Errorf("failed to decode duck response: %w", err)
	}
	mediaURL, ok := result["url"].(string)
	if !ok || mediaURL == "" {
		return "", false, fmt.Errorf("no URL in duck API response")
	}
	isGif := strings.HasSuffix(strings.ToLower(mediaURL), ".gif")
	return mediaURL, isGif, nil
}

// fetchDuckByType fetches a duck with an explicit type ("gif" or "jpg"), ignoring config.
func (i *Indexer) fetchDuckByType(mediaType string) (string, bool, error) {
	endpoint := fmt.Sprintf("https://random-d.uk/api/v2/random?type=%s", mediaType)
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(endpoint)
	if err != nil {
		return "", false, fmt.Errorf("failed to fetch duck media: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", false, fmt.Errorf("duck API returned status %d", resp.StatusCode)
	}
	var result map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", false, fmt.Errorf("failed to decode duck response: %w", err)
	}
	mediaURL, ok := result["url"].(string)
	if !ok || mediaURL == "" {
		return "", false, fmt.Errorf("no URL in duck API response")
	}
	isGif := strings.HasSuffix(strings.ToLower(mediaURL), ".gif")
	return mediaURL, isGif, nil
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

// Send tweet with optional media (GIF or image)
func (i *Indexer) sendTweet(text string, mediaURL string, isGif bool) error {
	if !i.twitterEnabled || i.twitterClient == nil {
		return nil
	}

	// Create tweet request
	input := &types.CreateInput{
		Text: gotwi.String(text),
	}

	// If media URL is provided, download and upload it
	if mediaURL != "" {
		mediaBytes, err := downloadImage(mediaURL)
		if err != nil {
			log.Printf("failed to download media, posting text-only tweet: %v", err)
		} else {
			mediaType := upload_types.MediaTypeJPEG
			mediaCategory := upload_types.MediaCategoryTweetImage
			if isGif {
				mediaType = upload_types.MediaTypeGIF
				mediaCategory = upload_types.MediaCategoryTweetGIF
			}
			mediaID, uploadErr := i.uploadMediaToTwitter(mediaBytes, mediaType, mediaCategory)
			if uploadErr != nil {
				log.Printf("failed to upload media to Twitter, posting text-only tweet: %v", uploadErr)
			} else {
				input.Media = &types.CreateInputMedia{
					MediaIDs: []string{mediaID},
				}
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

	// Get pool + network stake (NtC mark snapshot first, 5min timeout, Koios fallback)
	var poolStake, totalStake uint64
	if i.nodeQuery != nil {
		ntcCtx, ntcCancel := context.WithTimeout(ctx, 5*time.Minute)
		snapshots, snapErr := i.nodeQuery.QueryPoolStakeSnapshots(ntcCtx, i.bech32PoolId)
		ntcCancel()
		if snapErr != nil {
			log.Printf("NtC stake query failed for auto-leaderlog, trying Koios: %v", snapErr)
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
		msg := FormatScheduleForTelegram(schedule, i.poolName, i.leaderlogTZ, i.leaderlogTimeFormat)

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

// backfillSchedules calculates leader schedules for all historical epochs
// that have a computed nonce but no schedule yet. Uses Koios for historical
// stake data. This is a long-running one-shot operation.
func (i *Indexer) backfillSchedules(ctx context.Context) error {
	start := time.Now()
	log.Println("╔══════════════════════════════════════════════════════════════╗")
	log.Println("║              SCHEDULE HISTORY BACKFILL                       ║")
	log.Println("╚══════════════════════════════════════════════════════════════╝")

	epochLength := GetEpochLength(i.networkMagic)
	slotToTimeFn := makeSlotToTime(i.networkMagic)

	shelleyStart := ShelleyStartEpoch
	if i.networkMagic == PreprodNetworkMagic {
		shelleyStart = PreprodShelleyStartEpoch
	}

	// Find the range of epochs with nonces
	computed := 0
	skipped := 0
	failed := 0

	for epoch := shelleyStart + 1; epoch <= i.epoch; epoch++ {
		// Check if schedule already exists
		existing, _ := i.store.GetLeaderSchedule(ctx, epoch)
		if existing != nil {
			skipped++
			continue
		}

		// Get nonce from DB
		nonce, err := i.store.GetFinalNonce(ctx, epoch)
		if err != nil || nonce == nil {
			continue // no nonce available for this epoch
		}

		// Get historical stake from Koios pool_history
		var poolStake, totalStake uint64
		epochNo := koios.EpochNo(epoch)
		poolHist, histErr := i.koios.GetPoolHistory(ctx, koios.PoolID(i.bech32PoolId), &epochNo, nil)
		if histErr != nil || len(poolHist.Data) == 0 {
			// Try adjacent epochs
			for _, tryEpoch := range []int{epoch - 1, epoch + 1} {
				tryNo := koios.EpochNo(tryEpoch)
				ph, e := i.koios.GetPoolHistory(ctx, koios.PoolID(i.bech32PoolId), &tryNo, nil)
				if e == nil && len(ph.Data) > 0 {
					poolStake = uint64(ph.Data[0].ActiveStake.IntPart())
					break
				}
			}
		} else {
			poolStake = uint64(poolHist.Data[0].ActiveStake.IntPart())
		}

		if poolStake == 0 {
			failed++
			continue
		}

		// Get total active stake from epoch_info
		epochInfo, infoErr := i.koios.GetEpochInfo(ctx, &epochNo, nil)
		if infoErr != nil || len(epochInfo.Data) == 0 {
			failed++
			continue
		}
		totalStake = uint64(epochInfo.Data[0].ActiveStake.IntPart())
		if totalStake == 0 {
			failed++
			continue
		}

		// Calculate schedule
		epochStartSlot := GetEpochStartSlot(epoch, i.networkMagic)
		schedule, calcErr := CalculateLeaderSchedule(
			epoch, nonce, i.vrfKey,
			poolStake, totalStake,
			epochLength, epochStartSlot, slotToTimeFn,
		)
		if calcErr != nil {
			log.Printf("Schedule backfill: epoch %d calc failed: %v", epoch, calcErr)
			failed++
			continue
		}

		// Compute performance: actual blocks forged / assigned slots
		forgedSlots, _ := i.store.GetForgedSlots(ctx, epoch)
		if len(schedule.AssignedSlots) > 0 {
			schedule.Performance = float64(len(forgedSlots)) / float64(len(schedule.AssignedSlots)) * 100
		}

		if err := i.store.InsertLeaderSchedule(ctx, schedule); err != nil {
			log.Printf("Schedule backfill: epoch %d store failed: %v", epoch, err)
			failed++
			continue
		}

		computed++
		log.Printf("Schedule backfill: epoch %d — %d assigned, %d forged (%.0f%% perf)",
			epoch, len(schedule.AssignedSlots), len(forgedSlots), schedule.Performance)

		// Rate limit Koios calls
		time.Sleep(100 * time.Millisecond)
	}

	log.Printf("Schedule backfill complete in %v: %d computed, %d skipped, %d failed",
		time.Since(start).Round(time.Second), computed, skipped, failed)
	return nil
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
