package main

import (
        "context"
        "encoding/hex"
        "encoding/json"
        "errors"
        "fmt"
        "io"
        "log"
        "net/http"
        "os"
        "strconv"
        "sync"
        "time"

        "github.com/blinklabs-io/adder/event"
        filter_event "github.com/blinklabs-io/adder/filter/event"
        "github.com/blinklabs-io/adder/input/chainsync"
        output_embedded "github.com/blinklabs-io/adder/output/embedded"
        "github.com/blinklabs-io/adder/pipeline"
        "github.com/blinklabs-io/gouroboros/ledger/babbage"
        "github.com/blinklabs-io/gouroboros/ledger/conway"
        "github.com/blinklabs-io/gouroboros/ledger/shelley"
        "github.com/btcsuite/btcutil/bech32"
        koios "github.com/cardano-community/koios-go-client/v3"
        "github.com/cenkalti/backoff/v4"
        "github.com/gorilla/websocket"
        "github.com/jackc/pgx/v5/pgxpool"
        "github.com/michimani/gotwi"
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

// Define a variable to store the timestamp of the previous block event
var prevBlockTimestamp time.Time
var timeDiffString string

// Channel to broadcast block events to connected clients
var clients = make(map[*websocket.Conn]bool) // connected clients
var broadcast = make(chan interface{})       // broadcast channel
var upgrader = websocket.Upgrader{
        CheckOrigin: func(r *http.Request) bool {
                return true
        },
}

// Singleton instance of the Indexer
var globalIndexer = &Indexer{}

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
        // Twitter fields
        twitterClient  *gotwi.Client
        twitterEnabled bool
        // Leaderlog fields
        vrfKey            *VRFKey
        leaderlogEnabled  bool
        leaderlogTZ       string
        db                *pgxpool.Pool
        nonceTracker      *NonceTracker
        leaderlogMu       sync.Mutex
        leaderlogCalcing  map[int]bool // epochs currently being calculated
}

type BlockEvent struct {
        Type      string              `json:"type"`
        Timestamp string              `json:"timestamp"`
        Context   event.BlockContext  `json:"context"`
        Payload   event.BlockEvent    `json:"payload"`
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

// Start the adder pipeline and handle block events
func (i *Indexer) Start() error {
        // Increment the WaitGroup counter
        i.wg.Add(1)
        defer func() {
                // Decrement the WaitGroup counter when the function exits
                i.wg.Done()
                if r := recover(); r != nil {
                        log.Println("Recovered in handleEvent", r)
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

        // Initialize the bot
        var err error
        i.bot, err = telebot.NewBot(telebot.Settings{
                Token:  i.telegramToken,
                Poller: &telebot.LongPoller{Timeout: 10 * time.Second},
        })
        if err != nil {
                log.Fatalf("failed to start bot: %s", err)
        }

        // Initialize Twitter client if credentials are provided
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
        } else {
                log.Println("Twitter credentials not provided, Twitter notifications disabled")
        }

        /// Initialize the kois client based on networkMagic number
        if i.networkMagic == 1 {
                i.koios, e = koios.New(
                        koios.Host(koios.PreProdHost),
                        koios.APIVersion("v1"),
                )
                if e != nil {
                        log.Fatal(e)
                }
        } else if i.networkMagic == 2 {
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
        fmt.Println("Epoch: ", i.epoch)

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

                // Connect to PostgreSQL
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

                connString := fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=disable",
                        dbUser, dbPassword, dbHost, dbPort, dbName)

                dbPool, dbErr := InitDB(connString)
                if dbErr != nil {
                        log.Fatalf("failed to connect to database: %s", dbErr)
                }
                i.db = dbPool

                // Initialize nonce tracker
                i.nonceTracker = NewNonceTracker(i.db, i.koios, i.epoch, i.networkMagic)
                i.leaderlogCalcing = make(map[int]bool)
                log.Println("Nonce tracker initialized")
        }

        // Convert the poolId to Bech32
        bech32PoolId, err := convertToBech32(i.poolId)
        if err != nil {
                log.Printf("failed to convert pool id to Bech32: %s", err)
                fmt.Println("PoolId: ", bech32PoolId)
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
                fmt.Println("Epoch Blocks: ", i.epochBlocks)
        } else {
                log.Fatalf("failed to get pool epoch blocks: %s", err)
        }

        if lifetimeBlocks.Data != nil {
                i.totalBlocks = lifetimeBlocks.Data.BlockCount
                fmt.Println("Total Blocks: ", i.totalBlocks)
        } else {
                log.Fatalf("failed to get pool lifetime blocks: %s", err)
        }
        log.Printf("duckBot started: %s | Epoch: %d | Epoch Blocks: %d | Lifetime Blocks: %d",
                i.poolName, i.epoch, i.epochBlocks, i.totalBlocks)

        // Try each host with exponential backoff
        hosts := i.nodeAddresses
        for _, host := range hosts {
                // Initialize the backoff strategy for each host attempt
                bo := backoff.NewExponentialBackOff()
                bo.MaxElapsedTime = maxRetryDuration

                // Wrap the pipeline start in a function for the backoff operation
                startPipelineFunc := func() error {
                        // Use the host to connect to the Cardano node
                        node := chainsync.WithAddress(host)
                        inputOpts := []chainsync.ChainSyncOptionFunc{
                                node,
                                chainsync.WithNetworkMagic(uint32(i.networkMagic)),
                                chainsync.WithIntersectTip(true),
                                chainsync.WithAutoReconnect(true),
                                chainsync.WithIncludeCbor(false),
                        }

                        i.pipeline = pipeline.New()
                        // Configure ChainSync input
                        input_chainsync := chainsync.New(inputOpts...)
                        i.pipeline.AddInput(input_chainsync)

                        // Configure filter to handle events
                        filterEvent := filter_event.New(filter_event.WithTypes([]string{"chainsync.block"}))
                        i.pipeline.AddFilter(filterEvent)

                        // Configure embedded output with callback function
                        output := output_embedded.New(output_embedded.WithCallbackFunc(i.handleEvent))
                        i.pipeline.AddOutput(output)

                        err := i.pipeline.Start()
                        if err != nil {
                                log.Printf("Failed to start pipeline on %s: %s. Retrying...", host, err)
                                return err
                        }

                        return nil
                }

                // Use exponential backoff retry
                err := backoff.Retry(startPipelineFunc, bo)
                if err == nil {
                        log.Printf("Successfully connected to node at %s", host)
                        return nil
                }
                log.Printf("Failed to connect to node at %s after retries: %s", host, err)
        }

        log.Fatalf("Failed to start pipeline after trying all hosts")
        return errors.New("Failed to start pipeline after trying all hosts")

}

func (i *Indexer) handleEvent(evt event.Event) error {
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
                return fmt.Errorf("error marshalling event: %v", err)
        }

        // Unmarshal the event to get the block event details
        var blockEvent BlockEvent
        err = json.Unmarshal(data, &blockEvent)
        if err != nil {
                return fmt.Errorf("error unmarshalling block event: %v", err)
        }

        // Convert the block event timestamp to time.Time
        blockEventTime, err := time.Parse(time.RFC3339, blockEvent.Timestamp)
        if err != nil {
                return fmt.Errorf("error parsing block event timestamp: %v", err)
        }

        // Calculate the time difference between the current block event and the previous one
        timeDiff := blockEventTime.Sub(prevBlockTimestamp)
        if timeDiff.Seconds() < 60 {
                timeDiffString = fmt.Sprintf("%.0f seconds", timeDiff.Seconds())
        } else {
                minutes := int(timeDiff.Minutes())
                seconds := int(timeDiff.Seconds()) - (minutes * 60)
                timeDiffString = fmt.Sprintf("%d minute %02d seconds", minutes, seconds)
        }

        // Print the time difference to the terminal
        fmt.Println("Time Difference:", timeDiffString)

        // Update the previous block event timestamp with the current one
        prevBlockTimestamp = blockEventTime

        // Convert the block event timestamp to local time
        localTime := blockEventTime.In(time.Local)
        blockEvent.Timestamp = localTime.Format("January 2, 2006 15:04:05 MST")
        fmt.Printf("Local Time: %s\n", blockEvent.Timestamp)

        // Convert issuer Vkey to Bech32
        bech32PoolID, err := convertToBech32(blockEvent.Payload.IssuerVkey)
        if err != nil {
                log.Printf("failed to convert issuer vkey to Bech32: %s", err)
        } else {
                fmt.Printf("Bech32 poolID: %s\n", bech32PoolID)
        }

        // Customize links based on the network magic number
        var cexplorerLink string
        switch i.networkMagic {
        case 1:
                cexplorerLink = "https://preprod.cexplorer.io/block/"
        case 2:
                cexplorerLink = "https://preview.cexplorer.io/block/"
        default:
                cexplorerLink = "https://cexplorer.io/block/"
        }

        // Check if the epoch has changed
        currentEpoch := getCurrentEpoch()
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

                msg := fmt.Sprintf(
                        "Quack!(attention) 🦆\nduckBot notification!\n\n"+
                                "%s\n"+"💥 New Block!\n\n"+
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

                // Get duck image URL for both Telegram and Twitter
                duckImageURL := getDuckImage()

                // Send the message to the appropriate channel
                channelID, err := strconv.ParseInt(i.telegramChannel, 10, 64)
                if err != nil {
                        log.Fatalf("failed to parse telegram channel ID: %s", err)
                }
                photo := &telebot.Photo{File: telebot.FromURL(duckImageURL), Caption: msg}
                _, err = i.bot.Send(&telebot.Chat{ID: channelID}, photo)
                if err != nil {
                        log.Printf("failed to send Telegram message: %s", err)
                }

                // Send tweet if Twitter is enabled (shorter format for 280 char limit)
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
        }

        // Print the received event information
        fmt.Printf("Received Event: %+v\n", blockEvent)

        // Send the block event to the WebSocket clients
        broadcast <- blockEvent

        return nil
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

// Get random duck
func getDuckImage() string {
        resp, err := http.Get("https://random-d.uk/api/v2/random")
        if err != nil {
                log.Fatal(err)
        }
        defer resp.Body.Close()
        var result map[string]interface{}
        json.NewDecoder(resp.Body).Decode(&result)
        imageURL := result["url"].(string)
        return imageURL
}

// Download image from URL to bytes
func downloadImage(imageURL string) ([]byte, error) {
        resp, err := http.Get(imageURL)
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

// Send tweet with optional image
func (i *Indexer) sendTweet(text string, imageURL string) error {
        if !i.twitterEnabled || i.twitterClient == nil {
                return nil
        }

        // Create tweet request
        input := &types.CreateInput{
                Text: gotwi.String(text),
        }

        // Post the tweet
        _, err := managetweet.Create(context.Background(), i.twitterClient, input)
        if err != nil {
                return fmt.Errorf("failed to post tweet: %w", err)
        }

        log.Println("Tweet posted successfully")
        return nil
}

// Convert to bech32 poolID
func convertToBech32(hash string) (string, error) {
        bytes, err := hex.DecodeString(hash)
        if err != nil {
                return "", err
        }
        fiveBitWords, err := bech32.ConvertBits(bytes, 8, 5, true)
        if err != nil {
                return "", err
        }
        bech32Str, err := bech32.Encode("pool", fiveBitWords)
        if err != nil {
                return "", err
        }
        return bech32Str, nil
}

// extractVrfOutput extracts the VRF output from a block header.
// Returns the raw VRF output bytes, or nil if extraction fails.
func extractVrfOutput(header interface{}) []byte {
        switch h := header.(type) {
        case *conway.ConwayBlockHeader:
                return h.Body.VrfResult.Output
        case *babbage.BabbageBlockHeader:
                return h.Body.VrfResult.Output
        case *shelley.ShelleyBlockHeader:
                // Pre-Babbage eras have separate nonce VRF
                return h.Body.NonceVrf.Output
        default:
                log.Printf("Unsupported block header type for VRF extraction: %T", header)
                return nil
        }
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
// Triggers at >=70% epoch progress (stability window passed).
func (i *Indexer) checkLeaderlogTrigger(slot uint64) {
        progress := i.getEpochProgress(slot)

        // Freeze candidate nonce at stability window
        if progress >= 70.0 {
                i.nonceTracker.FreezeCandidate(i.epoch)
        }

        // Calculate leader schedule for next epoch after stability window
        nextEpoch := i.epoch + 1
        if progress >= 70.0 {
                i.leaderlogMu.Lock()
                if i.leaderlogCalcing[nextEpoch] {
                        i.leaderlogMu.Unlock()
                        return
                }
                if IsSchedulePosted(context.Background(), i.db, nextEpoch) {
                        i.leaderlogMu.Unlock()
                        return
                }
                i.leaderlogCalcing[nextEpoch] = true
                i.leaderlogMu.Unlock()

                go func() {
                        i.calculateAndPostLeaderlog(nextEpoch)
                        i.leaderlogMu.Lock()
                        delete(i.leaderlogCalcing, nextEpoch)
                        i.leaderlogMu.Unlock()
                }()
        }
}

// calculateAndPostLeaderlog calculates the leader schedule and posts to Telegram.
func (i *Indexer) calculateAndPostLeaderlog(epoch int) {
        log.Printf("Calculating leader schedule for epoch %d...", epoch)

        // Wait a bit for data to settle
        time.Sleep(30 * time.Second)

        ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
        defer cancel()

        // Get epoch nonce (local first, Koios fallback)
        epochNonce, err := i.nonceTracker.GetNonceForEpoch(epoch)
        if err != nil {
                log.Printf("Failed to get nonce for epoch %d: %v", epoch, err)
                return
        }

        // Get pool stake from Koios
        poolInfo, err := i.koios.GetPoolInfo(ctx, koios.PoolID(i.bech32PoolId), nil)
        if err != nil {
                log.Printf("Failed to get pool info for leaderlog: %v", err)
                return
        }
        if poolInfo.Data == nil {
                log.Printf("No pool info returned for leaderlog")
                return
        }
        poolStake := uint64(poolInfo.Data.ActiveStake.IntPart())

        // Get total active stake from Koios
        epochNo := koios.EpochNo(epoch)
        epochInfo, err := i.koios.GetEpochInfo(ctx, &epochNo, nil)
        if err != nil {
                log.Printf("Failed to get epoch info for leaderlog: %v", err)
                return
        }
        if len(epochInfo.Data) == 0 {
                log.Printf("No epoch info returned for epoch %d", epoch)
                return
        }
        totalStake := uint64(epochInfo.Data[0].ActiveStake.IntPart())

        if poolStake == 0 || totalStake == 0 {
                log.Printf("Invalid stake values: pool=%d, total=%d", poolStake, totalStake)
                return
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
                return
        }

        log.Printf("Epoch %d: %d slots assigned (expected %.2f)",
                epoch, len(schedule.AssignedSlots), schedule.IdealSlots)

        // Store in database
        if err := InsertLeaderSchedule(ctx, i.db, schedule); err != nil {
                log.Printf("Failed to store leader schedule: %v", err)
        }

        // Format and post to Telegram
        msg := FormatScheduleForTelegram(schedule, i.poolName, i.leaderlogTZ)

        channelID, err := strconv.ParseInt(i.telegramChannel, 10, 64)
        if err != nil {
                log.Printf("Failed to parse telegram channel ID: %v", err)
                return
        }

        _, err = i.bot.Send(&telebot.Chat{ID: channelID}, msg)
        if err != nil {
                log.Printf("Failed to send leaderlog to Telegram: %v", err)
                return
        }

        // Mark as posted
        if err := MarkSchedulePosted(ctx, i.db, epoch); err != nil {
                log.Printf("Failed to mark schedule as posted: %v", err)
        }

        log.Printf("Leader schedule for epoch %d posted to Telegram", epoch)
}

// makeSlotToTime returns a function that converts a slot number to a time.Time.
func makeSlotToTime(networkMagic int) func(uint64) time.Time {
        shelleyStart, _ := time.Parse(time.RFC3339, ShelleyEpochStart)

        if networkMagic == 764824073 { // mainnet
                // Shelley started at slot 4492800
                return func(slot uint64) time.Time {
                        return shelleyStart.Add(time.Duration(int64(slot)-4492800) * time.Second)
                }
        }

        // Preview/preprod — slot 0 starts at network genesis
        // Preview: 2022-11-01T00:00:00Z, Preprod: 2022-04-01T00:00:00Z
        var genesis time.Time
        switch networkMagic {
        case 2: // preview
                genesis, _ = time.Parse(time.RFC3339, "2022-11-01T00:00:00Z")
        case 1: // preprod
                genesis, _ = time.Parse(time.RFC3339, "2022-04-01T00:00:00Z")
        default:
                genesis = shelleyStart
        }

        return func(slot uint64) time.Time {
                return genesis.Add(time.Duration(slot) * time.Second)
        }
}

// formatNumber formats an integer with comma separators (e.g., 1234567 -> "1,234,567").
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

// Main function to start the adder pipeline
func main() {

        // Start the adder pipeline
        if err := globalIndexer.Start(); err != nil {
                log.Fatalf("failed to start adder: %s", err)
        }

        // Configure websocket route
        http.HandleFunc("/ws", handleConnections)

        // Start listening for incoming chat messages
        go handleMessages()

        // Wait for all goroutines to finish before exiting
        globalIndexer.wg.Wait()

        // Start the server on localhost port 8080 and log any errors
        log.Println("http server started on :8081")
        err := http.ListenAndServe("localhost:8081", nil)
        if err != nil {
                log.Fatal("ListenAndServe: ", err)
        }
}
