package main

import (
	"context"
	"encoding/hex"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	koios "github.com/cardano-community/koios-go-client/v3"
	telebot "gopkg.in/tucnak/telebot.v2"
)

// registerCommands registers all Telegram bot command handlers and sets the command menu.
func (i *Indexer) registerCommands() {
	i.bot.Handle("/help", i.cmdHelp)
	i.bot.Handle("/status", i.cmdStatus)
	i.bot.Handle("/tip", i.cmdTip)
	i.bot.Handle("/epoch", i.cmdEpoch)
	i.bot.Handle("/leaderlog", i.cmdLeaderlog)
	i.bot.Handle("/nonce", i.cmdNonce)
	i.bot.Handle("/validate", i.cmdValidate)
	i.bot.Handle("/stake", i.cmdStake)
	i.bot.Handle("/blocks", i.cmdBlocks)
	i.bot.Handle("/ping", i.cmdPing)
	i.bot.Handle("/duck", i.cmdDuck)
	i.bot.Handle("/nextblock", i.cmdNextBlock)
	i.bot.Handle("/version", i.cmdVersion)

	// Register command menu with Telegram so users see / autocomplete
	err := i.bot.SetCommands([]telebot.Command{
		{Text: "help", Description: "Show available commands"},
		{Text: "status", Description: "DB sync status"},
		{Text: "tip", Description: "Current chain tip"},
		{Text: "epoch", Description: "Current epoch info"},
		{Text: "leaderlog", Description: "Leader schedule (next/current/epoch)"},
		{Text: "nextblock", Description: "Next scheduled block"},
		{Text: "nonce", Description: "Epoch nonce (next/current)"},
		{Text: "validate", Description: "Check block hash on-chain"},
		{Text: "stake", Description: "Pool & network stake"},
		{Text: "blocks", Description: "Pool block count"},
		{Text: "ping", Description: "Node connectivity check"},
		{Text: "duck", Description: "Random duck pic"},
		{Text: "version", Description: "Bot version info"},
	})
	if err != nil {
		log.Printf("Failed to set bot command menu: %v", err)
	}

	log.Println("Bot commands registered")
}

// isAllowed checks if the sender is an admin user (has access to all commands).
func (i *Indexer) isAllowed(m *telebot.Message) bool {
	if m.Sender == nil {
		return false
	}
	_, ok := i.allowedUsers[int64(m.Sender.ID)]
	return ok
}

// isGroupAllowed checks if the message came from an allowed group or an admin user.
// Used for safe, non-sensitive commands that any group member can use.
func (i *Indexer) isGroupAllowed(m *telebot.Message) bool {
	if i.isAllowed(m) {
		return true
	}
	if m.Chat == nil {
		return false
	}
	_, ok := i.allowedGroups[m.Chat.ID]
	return ok
}

func (i *Indexer) requireNodeQuery(m *telebot.Message) bool {
	if i.nodeQuery == nil {
		i.bot.Send(m.Chat, "Node query not configured")
		return false
	}
	return true
}

func (i *Indexer) cmdHelp(m *telebot.Message) {
	if !i.isGroupAllowed(m) {
		return
	}
	msg := "\U0001F986 duckBot Commands\n\n" +
		"`/help` \u2014 Show this help message\n" +
		"`/status` \u2014 DB sync status\n" +
		"`/tip` \u2014 Current chain tip\n" +
		"`/epoch` \u2014 Current epoch info\n" +
		"`/leaderlog` \\[next|current|epoch] \u2014 Leader schedule\n" +
		"`/nextblock` \u2014 Next scheduled block slot & time\n" +
		"`/nonce` \\[next|current] \u2014 Epoch nonce\n" +
		"`/validate` <hash> \u2014 Check block on-chain\n" +
		"`/stake` \u2014 Pool & network stake\n" +
		"`/blocks` \\[epoch] \u2014 Pool block count\n" +
		"`/ping` \u2014 Node connectivity check\n" +
		"`/duck` \u2014 Random duck pic\n" +
		"`/version` \u2014 Bot version info"
	i.bot.Send(m.Chat, msg, telebot.ModeMarkdown)
}

func (i *Indexer) cmdStatus(m *telebot.Message) {
	if !i.isGroupAllowed(m) {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	lastSlot, err := i.store.GetLastSyncedSlot(ctx)
	if err != nil {
		i.bot.Send(m.Chat, fmt.Sprintf("Error: %v", err))
		return
	}

	msg := fmt.Sprintf("\U0001F4CA Sync Status\n\n"+
		"Last synced slot: %d\n"+
		"Epoch: %d\n"+
		"Mode: %s\n",
		lastSlot, i.epoch, i.mode)

	if i.nodeQuery == nil {
		i.bot.Send(m.Chat, msg)
		return
	}

	tipSlot, _, tipEpoch, tipErr := i.nodeQuery.QueryTip(ctx)
	if tipErr == nil {
		distance := int64(tipSlot) - int64(lastSlot)
		syncPct := 0.0
		if tipSlot > 0 {
			syncPct = float64(lastSlot) / float64(tipSlot) * 100
		}
		msg += fmt.Sprintf("\nChain tip: slot %d (epoch %d)\n"+
			"Distance: %d slots\n"+
			"Synced: %.2f%%\n",
			tipSlot, tipEpoch, distance, syncPct)
	} else {
		msg += fmt.Sprintf("\nNode tip unavailable: %v", tipErr)
	}

	i.bot.Send(m.Chat, msg)
}

func (i *Indexer) cmdTip(m *telebot.Message) {
	if !i.isGroupAllowed(m) || !i.requireNodeQuery(m) {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	slot, blockHash, epoch, err := i.nodeQuery.QueryTip(ctx)
	if err != nil {
		i.bot.Send(m.Chat, fmt.Sprintf("Error querying tip: %v", err))
		return
	}

	progress := i.getEpochProgress(slot)
	hashDisplay := blockHash
	if len(hashDisplay) > 16 {
		hashDisplay = blockHash[:16] + "..."
	}

	msg := fmt.Sprintf("\U0001F517 Chain Tip\n\n"+
		"Slot: %d\n"+
		"Block: %s\n"+
		"Epoch: %d (%.1f%%)\n",
		slot, hashDisplay, epoch, progress)

	i.bot.Send(m.Chat, msg)
}

func (i *Indexer) cmdEpoch(m *telebot.Message) {
	if !i.isGroupAllowed(m) || !i.requireNodeQuery(m) {
		return
	}

	epoch := getCurrentEpoch()
	epochStart := GetEpochStartSlot(epoch, i.networkMagic)
	epochLen := GetEpochLength(i.networkMagic)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var slotsIntoEpoch uint64
	tipSlot, _, _, tipErr := i.nodeQuery.QueryTip(ctx)
	if tipErr == nil && tipSlot >= epochStart {
		slotsIntoEpoch = tipSlot - epochStart
	}
	if slotsIntoEpoch > epochLen {
		slotsIntoEpoch = epochLen
	}

	slotsRemaining := epochLen - slotsIntoEpoch
	progress := float64(slotsIntoEpoch) / float64(epochLen) * 100
	timeRemaining := time.Duration(slotsRemaining) * time.Second
	days := int(timeRemaining.Hours()) / 24
	hours := int(timeRemaining.Hours()) % 24
	minutes := int(timeRemaining.Minutes()) % 60

	msg := fmt.Sprintf("\U0001F4C5 Epoch %d\n\n"+
		"Progress: %.1f%%\n"+
		"Slots: %d / %d\n"+
		"Remaining: %dd %dh %dm\n",
		epoch, progress, slotsIntoEpoch, epochLen, days, hours, minutes)

	if progress >= 60 {
		msg += "\n\u2705 Past stability window (60%)"
	}

	i.bot.Send(m.Chat, msg)
}

func (i *Indexer) cmdLeaderlog(m *telebot.Message) {
	if !i.isAllowed(m) {
		return
	}

	if !i.leaderlogEnabled || i.vrfKey == nil {
		i.bot.Send(m.Chat, "Leaderlog not enabled (no VRF key configured)")
		return
	}

	args := strings.TrimSpace(m.Payload)

	// Parse argument: "next" (default), "current", or epoch number
	var targetEpoch int
	var snapType SnapshotType
	switch {
	case args == "" || args == "next":
		targetEpoch = getCurrentEpoch() + 1
		snapType = SnapshotMark
	case args == "current":
		targetEpoch = getCurrentEpoch()
		snapType = SnapshotSet
	default:
		parsed, err := strconv.Atoi(args)
		if err != nil {
			i.bot.Send(m.Chat, "Usage: `/leaderlog` \\[next|current|<epoch>]", telebot.ModeMarkdown)
			return
		}
		targetEpoch = parsed

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()

		// Check DB first
		stored, storeErr := i.store.GetLeaderSchedule(ctx, targetEpoch)
		if storeErr == nil && stored != nil {
			msg := FormatScheduleForTelegram(stored, i.poolName, i.leaderlogTZ)
			i.bot.Send(m.Chat, msg)
			return
		}

		// On-demand: compute using cached nonce + NtC/Koios for stake
		sent, sendErr := i.bot.Send(m.Chat, fmt.Sprintf("\u23F3 Calculating leader schedule for epoch %d...", targetEpoch))
		replyEpoch := func(text string) {
			if sendErr == nil {
				i.bot.Edit(sent, text)
			} else {
				i.bot.Send(m.Chat, text)
			}
		}

		epochNonce, nonceErr := i.nonceTracker.GetNonceForEpoch(targetEpoch)
		if nonceErr != nil {
			replyEpoch(fmt.Sprintf("Failed to get nonce for epoch %d: %v", targetEpoch, nonceErr))
			return
		}

		// Try NtC first if epoch is within snapshot range (mark/set/go)
		var poolStake, totalStake uint64
		curEpoch := getCurrentEpoch()
		if i.nodeQuery != nil {
			var snap SnapshotType
			ntcAvailable := true
			switch targetEpoch {
			case curEpoch + 1:
				snap = SnapshotMark
			case curEpoch:
				snap = SnapshotSet
			case curEpoch - 2:
				snap = SnapshotGo
			default:
				ntcAvailable = false
			}
			if ntcAvailable {
				ntcCtx, ntcCancel := context.WithTimeout(ctx, 60*time.Second)
				snapshots, snapErr := i.nodeQuery.QueryPoolStakeSnapshots(ntcCtx, i.bech32PoolId)
				ntcCancel()
				if snapErr != nil {
					log.Printf("NtC stake query for epoch %d failed: %v", targetEpoch, snapErr)
				} else {
					switch snap {
					case SnapshotMark:
						poolStake = snapshots.PoolStakeMark
						totalStake = snapshots.TotalStakeMark
					case SnapshotSet:
						poolStake = snapshots.PoolStakeSet
						totalStake = snapshots.TotalStakeSet
					case SnapshotGo:
						poolStake = snapshots.PoolStakeGo
						totalStake = snapshots.TotalStakeGo
					}
				}
			}
		}

		// Koios fallback for epochs outside NtC range or NtC failure
		if poolStake == 0 && i.koios != nil {
			epochNo := koios.EpochNo(targetEpoch)
			poolHist, histErr := i.koios.GetPoolHistory(ctx, koios.PoolID(i.bech32PoolId), &epochNo, nil)
			if histErr == nil && len(poolHist.Data) > 0 {
				poolStake = uint64(poolHist.Data[0].ActiveStake.IntPart())
			}
			epochInfo, infoErr := i.koios.GetEpochInfo(ctx, &epochNo, nil)
			if infoErr == nil && len(epochInfo.Data) > 0 {
				totalStake = uint64(epochInfo.Data[0].ActiveStake.IntPart())
			}
		}

		if poolStake == 0 || totalStake == 0 {
			replyEpoch(fmt.Sprintf("Failed to get stake for epoch %d from NtC or Koios", targetEpoch))
			return
		}

		epochLength := GetEpochLength(i.networkMagic)
		epochStartSlot := GetEpochStartSlot(targetEpoch, i.networkMagic)
		slotToTimeFn := makeSlotToTime(i.networkMagic)

		schedule, calcErr := CalculateLeaderSchedule(
			targetEpoch, epochNonce, i.vrfKey,
			poolStake, totalStake,
			epochLength, epochStartSlot, slotToTimeFn,
		)
		if calcErr != nil {
			replyEpoch(fmt.Sprintf("Failed to calculate schedule: %v", calcErr))
			return
		}

		if storeScheduleErr := i.store.InsertLeaderSchedule(ctx, schedule); storeScheduleErr != nil {
			log.Printf("Failed to store schedule for epoch %d: %v", targetEpoch, storeScheduleErr)
		}

		msg := FormatScheduleForTelegram(schedule, i.poolName, i.leaderlogTZ)
		replyEpoch(msg)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	// Check for stored schedule first
	stored, err := i.store.GetLeaderSchedule(ctx, targetEpoch)
	if err == nil && stored != nil {
		msg := FormatScheduleForTelegram(stored, i.poolName, i.leaderlogTZ)
		i.bot.Send(m.Chat, msg)
		return
	}

	if !i.requireNodeQuery(m) {
		return
	}

	// Calculate live — send progress message
	sent, sendErr := i.bot.Send(m.Chat, fmt.Sprintf("\u23F3 Calculating leader schedule for epoch %d...", targetEpoch))
	reply := func(text string) {
		if sendErr == nil {
			i.bot.Edit(sent, text)
		} else {
			i.bot.Send(m.Chat, text)
		}
	}

	epochNonce, err := i.nonceTracker.GetNonceForEpoch(targetEpoch)
	if err != nil {
		reply(fmt.Sprintf("Failed to get nonce for epoch %d: %v", targetEpoch, err))
		return
	}

	// Get stake from node — mark for next epoch, set for current (60s timeout)
	ntcCtx, ntcCancel := context.WithTimeout(ctx, 60*time.Second)
	snapshots, err := i.nodeQuery.QueryPoolStakeSnapshots(ntcCtx, i.bech32PoolId)
	ntcCancel()
	if err != nil {
		reply(fmt.Sprintf("Failed to get stake snapshots: %v", err))
		return
	}

	var poolStake, totalStake uint64
	switch snapType {
	case SnapshotSet:
		poolStake = snapshots.PoolStakeSet
		totalStake = snapshots.TotalStakeSet
	default:
		poolStake = snapshots.PoolStakeMark
		totalStake = snapshots.TotalStakeMark
	}
	if totalStake == 0 {
		reply("Total stake is zero")
		return
	}

	epochLength := GetEpochLength(i.networkMagic)
	epochStartSlot := GetEpochStartSlot(targetEpoch, i.networkMagic)
	slotToTimeFn := makeSlotToTime(i.networkMagic)

	schedule, err := CalculateLeaderSchedule(
		targetEpoch, epochNonce, i.vrfKey,
		poolStake, totalStake,
		epochLength, epochStartSlot, slotToTimeFn,
	)
	if err != nil {
		reply(fmt.Sprintf("Failed to calculate schedule: %v", err))
		return
	}

	if storeErr := i.store.InsertLeaderSchedule(ctx, schedule); storeErr != nil {
		log.Printf("Failed to store schedule from /leaderlog: %v", storeErr)
	}

	msg := FormatScheduleForTelegram(schedule, i.poolName, i.leaderlogTZ)
	reply(msg)
}

func (i *Indexer) cmdNonce(m *telebot.Message) {
	if !i.isGroupAllowed(m) {
		return
	}

	if i.nonceTracker == nil {
		i.bot.Send(m.Chat, "Nonce tracking not enabled")
		return
	}

	args := strings.TrimSpace(m.Payload)
	epoch := getCurrentEpoch()
	label := "current"
	if args == "next" {
		epoch = epoch + 1
		label = "next"
	}

	nonce, err := i.nonceTracker.GetNonceForEpoch(epoch)
	if err != nil {
		i.bot.Send(m.Chat, fmt.Sprintf("Error getting %s epoch nonce: %v", label, err))
		return
	}

	msg := fmt.Sprintf("\U0001F511 Epoch Nonce (%s)\n\n"+
		"Epoch: %d\n"+
		"Nonce: %s\n",
		label, epoch, hex.EncodeToString(nonce))

	i.bot.Send(m.Chat, msg)
}

func (i *Indexer) cmdValidate(m *telebot.Message) {
	if !i.isAllowed(m) {
		return
	}

	hash := strings.TrimSpace(m.Payload)
	if hash == "" {
		i.bot.Send(m.Chat, "Usage: `/validate` <block\\_hash>", telebot.ModeMarkdown)
		return
	}

	if len(hash) < 8 {
		i.bot.Send(m.Chat, "Hash prefix must be at least 8 characters")
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	records, err := i.store.GetBlockByHash(ctx, hash)
	if err != nil {
		i.bot.Send(m.Chat, fmt.Sprintf("Error: %v", err))
		return
	}

	if len(records) == 0 {
		display := hash
		if len(display) > 16 {
			display = hash[:16]
		}
		i.bot.Send(m.Chat, fmt.Sprintf("Block %s... not found in local DB\n(may be from another pool or not yet synced)", display))
		return
	}

	suffix := ""
	if len(records) != 1 {
		suffix = "es"
	}
	msg := fmt.Sprintf("\u2705 Block Found (%d match%s)\n\n", len(records), suffix)
	for _, r := range records {
		msg += fmt.Sprintf("Slot: %d\nEpoch: %d\nHash: %s\n\n", r.Slot, r.Epoch, r.BlockHash)
	}

	i.bot.Send(m.Chat, msg)
}

func (i *Indexer) cmdStake(m *telebot.Message) {
	if !i.isAllowed(m) {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	var poolStake, totalStake uint64
	source := "NtC"

	// Try NtC first with 5s timeout
	if i.nodeQuery != nil {
		ntcCtx, ntcCancel := context.WithTimeout(ctx, 5*time.Second)
		snapshots, err := i.nodeQuery.QueryPoolStakeSnapshots(ntcCtx, i.bech32PoolId)
		ntcCancel()
		if err != nil {
			log.Printf("/stake NtC query failed: %v", err)
		} else {
			poolStake = snapshots.PoolStakeMark
			totalStake = snapshots.TotalStakeMark
		}
	}

	// Koios fallback
	if poolStake == 0 && i.koios != nil {
		source = "Koios (NtC socat stalled)"
		currentEpoch := getCurrentEpoch()
		for _, tryEpoch := range []int{currentEpoch, currentEpoch - 1, currentEpoch - 2} {
			epochNo := koios.EpochNo(tryEpoch)
			poolHist, histErr := i.koios.GetPoolHistory(ctx, koios.PoolID(i.bech32PoolId), &epochNo, nil)
			if histErr != nil || len(poolHist.Data) == 0 {
				continue
			}
			poolStake = uint64(poolHist.Data[0].ActiveStake.IntPart())
			epochInfo, infoErr := i.koios.GetEpochInfo(ctx, &epochNo, nil)
			if infoErr != nil || len(epochInfo.Data) == 0 {
				poolStake = 0
				continue
			}
			totalStake = uint64(epochInfo.Data[0].ActiveStake.IntPart())
			break
		}
	}

	if poolStake == 0 || totalStake == 0 {
		i.bot.Send(m.Chat, "Failed to get stake data from NtC and Koios")
		return
	}

	sigma := float64(poolStake) / float64(totalStake)

	msg := fmt.Sprintf("\U0001F4B0 Stake Info (%s)\n\n"+
		"Pool: %s\u20B3\n"+
		"Network: %s\u20B3\n"+
		"Sigma: %.10f\n",
		source, formatNumber(int64(poolStake)), formatNumber(int64(totalStake)), sigma)

	i.bot.Send(m.Chat, msg)
}

func (i *Indexer) cmdBlocks(m *telebot.Message) {
	if !i.isGroupAllowed(m) {
		return
	}

	args := strings.TrimSpace(m.Payload)
	epoch := getCurrentEpoch()
	if args != "" {
		parsed, err := strconv.Atoi(args)
		if err != nil {
			i.bot.Send(m.Chat, "Usage: `/blocks` \\[epoch\\_number]", telebot.ModeMarkdown)
			return
		}
		epoch = parsed
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Try stored leader schedule for assigned slot count
	schedule, err := i.store.GetLeaderSchedule(ctx, epoch)
	if err == nil && schedule != nil {
		msg := fmt.Sprintf("\U0001F4E6 Blocks \u2014 Epoch %d\n\n"+
			"Assigned: %d slots\n"+
			"Expected: %.2f\n",
			epoch, len(schedule.AssignedSlots), schedule.IdealSlots)
		i.bot.Send(m.Chat, msg)
		return
	}

	// Current epoch: use in-memory counter
	if epoch == getCurrentEpoch() {
		msg := fmt.Sprintf("\U0001F4E6 Blocks \u2014 Epoch %d\n\n"+
			"Minted this epoch: %d\n",
			epoch, i.epochBlocks)
		i.bot.Send(m.Chat, msg)
		return
	}

	i.bot.Send(m.Chat, fmt.Sprintf("No data for epoch %d", epoch))
}

func (i *Indexer) cmdPing(m *telebot.Message) {
	if !i.isGroupAllowed(m) || !i.requireNodeQuery(m) {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	start := time.Now()
	slot, _, epoch, err := i.nodeQuery.QueryTip(ctx)
	elapsed := time.Since(start)

	if err != nil {
		i.bot.Send(m.Chat, fmt.Sprintf("\u274C Node unreachable: %v", err))
		return
	}

	msg := fmt.Sprintf("\U0001F3D3 Pong!\n\n"+
		"Node: %dms\n"+
		"Tip: slot %d (epoch %d)\n",
		elapsed.Milliseconds(), slot, epoch)

	if len(i.nodeAddresses) > 0 {
		msg += fmt.Sprintf("Node: %s\n", i.nodeAddresses[0])
	}

	i.bot.Send(m.Chat, msg)
}

func (i *Indexer) cmdDuck(m *telebot.Message) {
	if !i.isGroupAllowed(m) {
		return
	}
	url, err := getDuckImage()
	if err != nil {
		i.bot.Send(m.Chat, fmt.Sprintf("Failed to fetch duck: %v", err))
		return
	}
	photo := &telebot.Photo{File: telebot.FromURL(url)}
	i.bot.Send(m.Chat, photo)
}

func (i *Indexer) cmdNextBlock(m *telebot.Message) {
	if !i.isAllowed(m) {
		return
	}

	if !i.leaderlogEnabled || i.vrfKey == nil {
		i.bot.Send(m.Chat, "Leaderlog not enabled")
		return
	}

	currentEpoch := getCurrentEpoch()

	// Try DB first with short timeout
	ctxShort, cancelShort := context.WithTimeout(context.Background(), 10*time.Second)
	schedule, err := i.store.GetLeaderSchedule(ctxShort, currentEpoch)
	cancelShort()

	// If no schedule, auto-compute it
	if err != nil || schedule == nil {
		sent, sendErr := i.bot.Send(m.Chat, "\U0001F986 Schedule not in database. Running leaderlog... standby quack")
		reply := func(text string) {
			if sendErr == nil {
				i.bot.Edit(sent, text)
			} else {
				i.bot.Send(m.Chat, text)
			}
		}

		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
		defer cancel()

		epochNonce, nonceErr := i.nonceTracker.GetNonceForEpoch(currentEpoch)
		if nonceErr != nil {
			reply(fmt.Sprintf("Failed to get nonce: %v", nonceErr))
			return
		}

		var poolStake, totalStake uint64
		if i.nodeQuery != nil {
			ntcCtx, ntcCancel := context.WithTimeout(ctx, 60*time.Second)
			snapshots, snapErr := i.nodeQuery.QueryPoolStakeSnapshots(ntcCtx, i.bech32PoolId)
			ntcCancel()
			if snapErr != nil {
				log.Printf("NtC stake query failed (60s timeout), trying Koios fallback: %v", snapErr)
			} else {
				poolStake = snapshots.PoolStakeSet
				totalStake = snapshots.TotalStakeSet
			}
		}
		if poolStake == 0 && i.koios != nil {
			// Try recent epochs (Koios may lag 1-2 epochs behind)
			for _, tryEpoch := range []int{currentEpoch, currentEpoch - 1, currentEpoch - 2} {
				epochNo := koios.EpochNo(tryEpoch)
				poolHist, histErr := i.koios.GetPoolHistory(ctx, koios.PoolID(i.bech32PoolId), &epochNo, nil)
				if histErr != nil || len(poolHist.Data) == 0 {
					log.Printf("Koios GetPoolHistory(%d) empty or error: %v", tryEpoch, histErr)
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
				log.Printf("Using Koios stake fallback for /nextblock (from epoch %d)", tryEpoch)
				break
			}
		}
		if poolStake == 0 || totalStake == 0 {
			reply("No stake data available from NtC or Koios")
			return
		}

		epochLength := GetEpochLength(i.networkMagic)
		epochStartSlot := GetEpochStartSlot(currentEpoch, i.networkMagic)
		slotToTimeFn := makeSlotToTime(i.networkMagic)

		computed, calcErr := CalculateLeaderSchedule(
			currentEpoch, epochNonce, i.vrfKey,
			poolStake, totalStake,
			epochLength, epochStartSlot, slotToTimeFn,
		)
		if calcErr != nil {
			reply(fmt.Sprintf("Failed to calculate schedule: %v", calcErr))
			return
		}

		if storeErr := i.store.InsertLeaderSchedule(ctx, computed); storeErr != nil {
			log.Printf("Failed to store schedule from /nextblock: %v", storeErr)
		}

		schedule = computed
	}

	if len(schedule.AssignedSlots) == 0 {
		i.bot.Send(m.Chat, "No blocks scheduled this epoch")
		return
	}

	// Find the next slot that hasn't passed yet
	now := time.Now().UTC()
	var nextSlot *LeaderSlot

	for idx := range schedule.AssignedSlots {
		slot := &schedule.AssignedSlots[idx]
		if slot.At.After(now) {
			nextSlot = slot
			break
		}
	}

	if nextSlot == nil {
		i.bot.Send(m.Chat, "No more blocks scheduled this epoch")
		return
	}

	// Load timezone for display
	loc, err := time.LoadLocation(i.leaderlogTZ)
	if err != nil {
		loc = time.UTC
	}

	// Calculate time remaining
	timeUntil := time.Until(nextSlot.At)
	hours := int(timeUntil.Hours())
	minutes := int(timeUntil.Minutes()) % 60
	seconds := int(timeUntil.Seconds()) % 60

	// Format countdown
	var countdown string
	if hours > 0 {
		countdown = fmt.Sprintf("%dh %dm %ds", hours, minutes, seconds)
	} else if minutes > 0 {
		countdown = fmt.Sprintf("%dm %ds", minutes, seconds)
	} else {
		countdown = fmt.Sprintf("%ds", seconds)
	}

	localTime := nextSlot.At.In(loc)

	msg := fmt.Sprintf("\U0001F4C5 Next Scheduled Block\n\n"+
		"Slot: %d\n"+
		"Time: %s\n"+
		"Countdown: %s\n",
		nextSlot.Slot,
		localTime.Format("01/02/2006 03:04:05 PM MST"),
		countdown)

	i.bot.Send(m.Chat, msg)
}

func (i *Indexer) cmdVersion(m *telebot.Message) {
	if !i.isGroupAllowed(m) {
		return
	}

	sha := commitSHA
	if len(sha) > 8 {
		sha = sha[:8]
	}

	msg := fmt.Sprintf("\U0001F986 duckBot %s\n\n"+
		"Commit: %s\n"+
		"Built: %s\n"+
		"Mode: %s",
		version, sha, buildDate, i.mode)

	i.bot.Send(m.Chat, msg)
}
