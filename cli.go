package main

import (
	"context"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	koios "github.com/cardano-community/koios-go-client/v3"
	"github.com/spf13/viper"
)

// cliContext holds the shared state needed by CLI subcommands.
type cliContext struct {
	networkMagic int
	poolId       string
	bech32PoolId string
	poolName     string
	vrfKey       *VRFKey
	store        Store
	koios        *koios.Client
	nonceTracker *NonceTracker
	leaderlogTZ  string
	timeFormat   string
	slotToTime   func(uint64) time.Time
}

// runCLI dispatches CLI subcommands. Returns an exit code.
func runCLI(args []string) int {
	switch args[0] {
	case "version":
		return cmdVersion()
	case "leaderlog":
		return cmdCLILeaderlog(args[1:])
	case "nonce":
		return cmdCLINonce(args[1:])
	case "help", "--help", "-h":
		printCLIHelp()
		return 0
	default:
		fmt.Fprintf(os.Stderr, "Unknown command %q\n\nRun 'goduckbot help' for usage.\n", args[0])
		return 1
	}
}

func printCLIHelp() {
	fmt.Printf(`goduckbot - Cardano stake pool companion

Usage:
  goduckbot                       Start the daemon (default)
  goduckbot version               Show version information
  goduckbot leaderlog <epoch>     Calculate leaderlog for a single epoch
  goduckbot leaderlog <N>-<M>     Calculate leaderlog for epoch range (max 10)
  goduckbot nonce <epoch>         Show epoch nonce
  goduckbot help                  Show this help

Config is loaded from config.yaml in the current directory.
`)
}

func cmdVersion() int {
	sha := commitSHA
	if len(sha) > 8 {
		sha = sha[:8]
	}
	fmt.Printf("goduckbot %s (%s) built %s\n", version, sha, buildDate)
	return 0
}

// cliInit sets up the minimal state needed by CLI subcommands.
func cliInit() (*cliContext, func(), error) {
	viper.SetConfigName("config")
	viper.AddConfigPath(".")
	if err := viper.ReadInConfig(); err != nil {
		return nil, nil, fmt.Errorf("config: %w", err)
	}

	cc := &cliContext{
		networkMagic: viper.GetInt("networkMagic"),
		poolId:       viper.GetString("poolId"),
		poolName:     viper.GetString("poolName"),
		leaderlogTZ:  viper.GetString("leaderlog.timezone"),
		timeFormat:   viper.GetString("leaderlog.timeFormat"),
	}
	if cc.leaderlogTZ == "" {
		cc.leaderlogTZ = "UTC"
	}
	if cc.timeFormat != "24h" {
		cc.timeFormat = "12h"
	}

	// VRF key
	if v := viper.GetString("leaderlog.vrfKeyValue"); v != "" {
		key, err := ParseVRFKeyCborHex(v)
		if err != nil {
			return nil, nil, fmt.Errorf("vrfKeyValue: %w", err)
		}
		cc.vrfKey = key
	} else if p := viper.GetString("leaderlog.vrfKeyPath"); p != "" {
		key, err := ParseVRFKeyFile(p)
		if err != nil {
			return nil, nil, fmt.Errorf("vrfKeyPath %s: %w", p, err)
		}
		cc.vrfKey = key
	}

	// Store
	store, err := initStore()
	if err != nil {
		if cc.vrfKey != nil {
			cc.vrfKey.Close()
		}
		return nil, nil, fmt.Errorf("database: %w", err)
	}
	cc.store = store

	// Koios client
	switch cc.networkMagic {
	case PreprodNetworkMagic:
		cc.koios, err = koios.New(koios.Host(koios.PreProdHost), koios.APIVersion("v1"))
	case PreviewNetworkMagic:
		cc.koios, err = koios.New(koios.Host(koios.PreviewHost), koios.APIVersion("v1"))
	default:
		cc.koios, err = koios.New(koios.Host(koios.MainnetHost), koios.APIVersion("v1"))
	}
	if err != nil {
		store.Close()
		if cc.vrfKey != nil {
			cc.vrfKey.Close()
		}
		return nil, nil, fmt.Errorf("koios: %w", err)
	}

	// Bech32 pool ID
	bech32, err := convertToBech32(cc.poolId)
	if err != nil {
		store.Close()
		if cc.vrfKey != nil {
			cc.vrfKey.Close()
		}
		return nil, nil, fmt.Errorf("pool ID to bech32: %w", err)
	}
	cc.bech32PoolId = bech32

	// Nonce tracker (lite mode — uses DB cache + Koios fallback)
	currentEpoch := calcCurrentEpoch(cc.networkMagic)
	cc.nonceTracker = NewNonceTracker(store, cc.koios, currentEpoch, cc.networkMagic, false)

	cc.slotToTime = makeSlotToTime(cc.networkMagic)

	cleanup := func() {
		store.Close()
		if cc.vrfKey != nil {
			cc.vrfKey.Close()
		}
	}
	return cc, cleanup, nil
}

// parseEpochArg parses "612" into [612] or "500-510" into [500..510].
func parseEpochArg(arg string) ([]int, error) {
	if idx := strings.Index(arg, "-"); idx > 0 {
		start, err1 := strconv.Atoi(arg[:idx])
		end, err2 := strconv.Atoi(arg[idx+1:])
		if err1 != nil || err2 != nil {
			return nil, fmt.Errorf("invalid epoch range %q", arg)
		}
		if start > end {
			return nil, fmt.Errorf("start epoch must be <= end epoch")
		}
		if end-start+1 > 10 {
			return nil, fmt.Errorf("max 10 epochs at once (got %d)", end-start+1)
		}
		epochs := make([]int, 0, end-start+1)
		for e := start; e <= end; e++ {
			epochs = append(epochs, e)
		}
		return epochs, nil
	}
	n, err := strconv.Atoi(arg)
	if err != nil {
		return nil, fmt.Errorf("invalid epoch %q", arg)
	}
	if n < 0 {
		return nil, fmt.Errorf("epoch must be non-negative, got %d", n)
	}
	return []int{n}, nil
}

func cmdCLILeaderlog(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "Usage: goduckbot leaderlog <epoch>|<start>-<end>")
		return 1
	}

	epochs, err := parseEpochArg(args[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}

	cc, cleanup, err := cliInit()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Init failed: %v\n", err)
		return 1
	}
	defer cleanup()

	if cc.vrfKey == nil {
		fmt.Fprintln(os.Stderr, "No VRF key configured (set leaderlog.vrfKeyValue or leaderlog.vrfKeyPath)")
		return 1
	}

	exitCode := 0
	for _, epoch := range epochs {
		if err := runLeaderlogForEpoch(cc, epoch); err != nil {
			fmt.Fprintf(os.Stderr, "Epoch %d: %v\n", epoch, err)
			exitCode = 1
		}
	}
	return exitCode
}

func runLeaderlogForEpoch(cc *cliContext, epoch int) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	// Check DB cache first
	stored, err := cc.store.GetLeaderSchedule(ctx, epoch)
	if err == nil && stored != nil {
		fmt.Println(formatScheduleForCLI(stored, cc.poolName, cc.leaderlogTZ, cc.timeFormat))
		return nil
	}

	log.Printf("Fetching nonce for epoch %d...", epoch)
	epochNonce, err := cc.nonceTracker.GetNonceForEpoch(epoch)
	if err != nil {
		return fmt.Errorf("nonce: %w", err)
	}

	// Get stake data from Koios (same fallback pattern as commands.go)
	var poolStake, totalStake uint64
	curEpoch := calcCurrentEpoch(cc.networkMagic)
	for _, tryEpoch := range []int{epoch, curEpoch, curEpoch - 1, curEpoch - 2} {
		epochNo := koios.EpochNo(tryEpoch)
		poolHist, histErr := cc.koios.GetPoolHistory(ctx, koios.PoolID(cc.bech32PoolId), &epochNo, nil)
		if histErr != nil || len(poolHist.Data) == 0 {
			continue
		}
		poolStake = uint64(poolHist.Data[0].ActiveStake.IntPart())
		epochInfo, infoErr := cc.koios.GetEpochInfo(ctx, &epochNo, nil)
		if infoErr != nil || len(epochInfo.Data) == 0 {
			poolStake = 0
			continue
		}
		totalStake = uint64(epochInfo.Data[0].ActiveStake.IntPart())
		if tryEpoch != epoch {
			log.Printf("Using stake from epoch %d for epoch %d calculation", tryEpoch, epoch)
		}
		break
	}

	if poolStake == 0 || totalStake == 0 {
		return fmt.Errorf("could not get stake data from Koios")
	}

	epochLength := GetEpochLength(cc.networkMagic)
	epochStartSlot := GetEpochStartSlot(epoch, cc.networkMagic)

	log.Printf("Calculating leader schedule for epoch %d...", epoch)
	schedule, err := CalculateLeaderSchedule(
		epoch, epochNonce, cc.vrfKey,
		poolStake, totalStake,
		epochLength, epochStartSlot, cc.slotToTime,
	)
	if err != nil {
		return fmt.Errorf("calculation: %w", err)
	}

	// Store in DB
	if storeErr := cc.store.InsertLeaderSchedule(ctx, schedule); storeErr != nil {
		log.Printf("Warning: failed to store schedule: %v", storeErr)
	}

	fmt.Println(formatScheduleForCLI(schedule, cc.poolName, cc.leaderlogTZ, cc.timeFormat))
	return nil
}

func cmdCLINonce(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "Usage: goduckbot nonce <epoch>")
		return 1
	}

	epoch, err := strconv.Atoi(args[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "Invalid epoch %q\n", args[0])
		return 1
	}

	cc, cleanup, err := cliInit()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Init failed: %v\n", err)
		return 1
	}
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// Try DB first
	nonce, dbErr := cc.store.GetFinalNonce(ctx, epoch)
	source := "db"
	if dbErr != nil || nonce == nil {
		nonce, err = cc.nonceTracker.GetNonceForEpoch(epoch)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			return 1
		}
		source = "computed"
	}

	fmt.Printf("Epoch:  %d\nNonce:  %s\nSource: %s\n", epoch, hex.EncodeToString(nonce), source)
	return 0
}

// formatScheduleForCLI formats a leader schedule for terminal output.
func formatScheduleForCLI(schedule *LeaderSchedule, poolName, timezone, timeFormat string) string {
	loc, err := time.LoadLocation(timezone)
	if err != nil {
		loc = time.UTC
	}

	timeFmt := "01/02/2006 03:04:05 PM"
	fmtLabel := "12-hour"
	if timeFormat == "24h" {
		timeFmt = "01/02/2006 15:04:05"
		fmtLabel = "24-hour"
	}

	performance := 0.0
	if schedule.IdealSlots > 0 {
		performance = float64(len(schedule.AssignedSlots)) / schedule.IdealSlots * 100
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "Epoch: %d\n", schedule.Epoch)
	fmt.Fprintf(&sb, "Nonce: %s\n", schedule.EpochNonce)
	fmt.Fprintf(&sb, "Pool Active Stake:    %s\n", formatNumber(int64(schedule.PoolStake)))
	fmt.Fprintf(&sb, "Network Active Stake: %s\n", formatNumber(int64(schedule.TotalStake)))
	fmt.Fprintf(&sb, "Ideal Blocks: %.2f\n", schedule.IdealSlots)

	if len(schedule.AssignedSlots) > 0 {
		fmt.Fprintf(&sb, "\nAssigned Slots (%s, %s):\n", fmtLabel, timezone)
		for _, slot := range schedule.AssignedSlots {
			localTime := slot.At.In(loc)
			fmt.Fprintf(&sb, "  %s - Slot: %-6d - B: %d\n",
				localTime.Format(timeFmt), slot.SlotInEpoch, slot.No)
		}
	} else {
		sb.WriteString("\nNo slots assigned this epoch.\n")
	}

	fmt.Fprintf(&sb, "\nTotal Scheduled Blocks: %d\n", len(schedule.AssignedSlots))
	fmt.Fprintf(&sb, "Performance: %.2f%%\n", performance)
	return sb.String()
}
