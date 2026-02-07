package main

import (
	"context"
	"encoding/hex"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	telebot "gopkg.in/tucnak/telebot.v2"
)

// registerCommands registers all Telegram bot command handlers.
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
	log.Println("Bot commands registered")
}

func (i *Indexer) isAllowed(m *telebot.Message) bool {
	if m.Sender == nil {
		return false
	}
	_, ok := i.allowedUsers[int64(m.Sender.ID)]
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
	if !i.isAllowed(m) {
		return
	}
	msg := "\U0001F986 duckBot Commands\n\n" +
		"/help \u2014 Show this help message\n" +
		"/status \u2014 DB sync status\n" +
		"/tip \u2014 Current chain tip\n" +
		"/epoch \u2014 Current epoch info\n" +
		"/leaderlog [next|current|epoch] \u2014 Leader schedule\n" +
		"/nonce [next|current] \u2014 Epoch nonce\n" +
		"/validate <hash> \u2014 Check block on-chain\n" +
		"/stake \u2014 Pool & network stake\n" +
		"/blocks [epoch] \u2014 Pool block count\n" +
		"/ping \u2014 Node connectivity check\n" +
		"/duck \u2014 Random duck pic"
	i.bot.Send(m.Chat, msg)
}

func (i *Indexer) cmdStatus(m *telebot.Message) {
	if !i.isAllowed(m) {
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
	if !i.isAllowed(m) || !i.requireNodeQuery(m) {
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
	if !i.isAllowed(m) || !i.requireNodeQuery(m) {
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
			i.bot.Send(m.Chat, "Usage: /leaderlog [next|current|<epoch>]")
			return
		}
		targetEpoch = parsed
		// Specific epoch — stored lookup only
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		stored, storeErr := i.store.GetLeaderSchedule(ctx, targetEpoch)
		if storeErr == nil && stored != nil {
			msg := FormatScheduleForTelegram(stored, i.poolName, i.leaderlogTZ)
			i.bot.Send(m.Chat, msg)
		} else {
			i.bot.Send(m.Chat, fmt.Sprintf("No stored schedule for epoch %d", targetEpoch))
		}
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
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

	// Get stake from node — mark for next epoch, set for current
	snapshots, err := i.nodeQuery.QueryPoolStakeSnapshots(ctx, i.bech32PoolId)
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
	if !i.isAllowed(m) {
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
		i.bot.Send(m.Chat, "Usage: /validate <block_hash>")
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
	if !i.isAllowed(m) || !i.requireNodeQuery(m) {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	snapshots, err := i.nodeQuery.QueryPoolStakeSnapshots(ctx, i.bech32PoolId)
	if err != nil {
		i.bot.Send(m.Chat, fmt.Sprintf("Error querying stake: %v", err))
		return
	}

	if snapshots.TotalStakeMark == 0 {
		i.bot.Send(m.Chat, "Total network stake is zero")
		return
	}

	sigma := float64(snapshots.PoolStakeMark) / float64(snapshots.TotalStakeMark)

	msg := fmt.Sprintf("\U0001F4B0 Stake Info (mark snapshot)\n\n"+
		"Pool: %s\u20B3\n"+
		"Network: %s\u20B3\n"+
		"Sigma: %.10f\n",
		formatNumber(int64(snapshots.PoolStakeMark)), formatNumber(int64(snapshots.TotalStakeMark)), sigma)

	i.bot.Send(m.Chat, msg)
}

func (i *Indexer) cmdBlocks(m *telebot.Message) {
	if !i.isAllowed(m) {
		return
	}

	args := strings.TrimSpace(m.Payload)
	epoch := getCurrentEpoch()
	if args != "" {
		parsed, err := strconv.Atoi(args)
		if err != nil {
			i.bot.Send(m.Chat, "Usage: /blocks [epoch_number]")
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
	if !i.isAllowed(m) || !i.requireNodeQuery(m) {
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
	if !i.isAllowed(m) {
		return
	}
	url := getDuckImage()
	photo := &telebot.Photo{File: telebot.FromURL(url)}
	i.bot.Send(m.Chat, photo)
}
