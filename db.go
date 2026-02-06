package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const schema = `
CREATE TABLE IF NOT EXISTS blocks (
    slot         BIGINT PRIMARY KEY,
    epoch        INTEGER NOT NULL,
    block_hash   TEXT NOT NULL,
    vrf_output   BYTEA NOT NULL,
    nonce_value  BYTEA NOT NULL,
    created_at   TIMESTAMPTZ DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_blocks_epoch ON blocks(epoch);

CREATE TABLE IF NOT EXISTS epoch_nonces (
    epoch            INTEGER PRIMARY KEY,
    evolving_nonce   BYTEA NOT NULL,
    candidate_nonce  BYTEA,
    final_nonce      BYTEA,
    block_count      INTEGER DEFAULT 0,
    source           TEXT DEFAULT 'chain_sync',
    updated_at       TIMESTAMPTZ DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS leader_schedules (
    epoch          INTEGER PRIMARY KEY,
    pool_stake     BIGINT NOT NULL,
    total_stake    BIGINT NOT NULL,
    epoch_nonce    TEXT NOT NULL,
    sigma          DOUBLE PRECISION,
    ideal_slots    DOUBLE PRECISION,
    slot_count     INTEGER DEFAULT 0,
    performance    DOUBLE PRECISION,
    slots          JSONB DEFAULT '[]',
    posted         BOOLEAN DEFAULT FALSE,
    calculated_at  TIMESTAMPTZ DEFAULT NOW()
);
`

// InitDB connects to PostgreSQL and creates tables if they don't exist.
func InitDB(connString string) (*pgxpool.Pool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, connString)
	if err != nil {
		return nil, fmt.Errorf("connecting to database: %w", err)
	}

	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("pinging database: %w", err)
	}

	if _, err := pool.Exec(ctx, schema); err != nil {
		pool.Close()
		return nil, fmt.Errorf("creating schema: %w", err)
	}

	log.Println("Database connected and schema initialized")
	return pool, nil
}

// InsertBlock stores a block's VRF data. Skips duplicates.
func InsertBlock(ctx context.Context, pool *pgxpool.Pool, slot uint64, epoch int, blockHash string, vrfOutput, nonceValue []byte) error {
	_, err := pool.Exec(ctx,
		`INSERT INTO blocks (slot, epoch, block_hash, vrf_output, nonce_value)
		 VALUES ($1, $2, $3, $4, $5)
		 ON CONFLICT (slot) DO NOTHING`,
		int64(slot), epoch, blockHash, vrfOutput, nonceValue,
	)
	return err
}

// UpsertEvolvingNonce updates the running evolving nonce for an epoch.
func UpsertEvolvingNonce(ctx context.Context, pool *pgxpool.Pool, epoch int, nonce []byte, blockCount int) error {
	_, err := pool.Exec(ctx,
		`INSERT INTO epoch_nonces (epoch, evolving_nonce, block_count, updated_at)
		 VALUES ($1, $2, $3, NOW())
		 ON CONFLICT (epoch) DO UPDATE SET
		   evolving_nonce = EXCLUDED.evolving_nonce,
		   block_count = EXCLUDED.block_count,
		   updated_at = NOW()`,
		epoch, nonce, blockCount,
	)
	return err
}

// SetCandidateNonce freezes the candidate nonce at the stability window.
func SetCandidateNonce(ctx context.Context, pool *pgxpool.Pool, epoch int, nonce []byte) error {
	_, err := pool.Exec(ctx,
		`UPDATE epoch_nonces SET candidate_nonce = $2, updated_at = NOW() WHERE epoch = $1`,
		epoch, nonce,
	)
	return err
}

// SetFinalNonce stores the calculated final epoch nonce.
func SetFinalNonce(ctx context.Context, pool *pgxpool.Pool, epoch int, nonce []byte, source string) error {
	_, err := pool.Exec(ctx,
		`INSERT INTO epoch_nonces (epoch, evolving_nonce, final_nonce, source, updated_at)
		 VALUES ($1, $2, $2, $3, NOW())
		 ON CONFLICT (epoch) DO UPDATE SET
		   final_nonce = EXCLUDED.final_nonce,
		   source = EXCLUDED.source,
		   updated_at = NOW()`,
		epoch, nonce, source,
	)
	return err
}

// GetFinalNonce retrieves the stored final nonce for an epoch.
func GetFinalNonce(ctx context.Context, pool *pgxpool.Pool, epoch int) ([]byte, error) {
	var nonce []byte
	err := pool.QueryRow(ctx,
		`SELECT final_nonce FROM epoch_nonces WHERE epoch = $1 AND final_nonce IS NOT NULL`,
		epoch,
	).Scan(&nonce)
	if err != nil {
		return nil, err
	}
	return nonce, nil
}

// GetEvolvingNonce retrieves the current evolving nonce and block count for an epoch.
func GetEvolvingNonce(ctx context.Context, pool *pgxpool.Pool, epoch int) ([]byte, int, error) {
	var nonce []byte
	var blockCount int
	err := pool.QueryRow(ctx,
		`SELECT evolving_nonce, block_count FROM epoch_nonces WHERE epoch = $1`,
		epoch,
	).Scan(&nonce, &blockCount)
	if err != nil {
		return nil, 0, err
	}
	return nonce, blockCount, nil
}

// InsertLeaderSchedule stores a calculated leader schedule.
func InsertLeaderSchedule(ctx context.Context, pool *pgxpool.Pool, schedule *LeaderSchedule) error {
	slotsJSON, err := json.Marshal(schedule.AssignedSlots)
	if err != nil {
		return fmt.Errorf("marshaling slots: %w", err)
	}

	performance := 0.0
	if schedule.IdealSlots > 0 {
		performance = float64(len(schedule.AssignedSlots)) / schedule.IdealSlots * 100
	}

	_, err = pool.Exec(ctx,
		`INSERT INTO leader_schedules (epoch, pool_stake, total_stake, epoch_nonce, sigma, ideal_slots, slot_count, performance, slots, calculated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		 ON CONFLICT (epoch) DO UPDATE SET
		   pool_stake = EXCLUDED.pool_stake,
		   total_stake = EXCLUDED.total_stake,
		   epoch_nonce = EXCLUDED.epoch_nonce,
		   sigma = EXCLUDED.sigma,
		   ideal_slots = EXCLUDED.ideal_slots,
		   slot_count = EXCLUDED.slot_count,
		   performance = EXCLUDED.performance,
		   slots = EXCLUDED.slots,
		   calculated_at = EXCLUDED.calculated_at`,
		schedule.Epoch,
		int64(schedule.PoolStake),
		int64(schedule.TotalStake),
		schedule.EpochNonce,
		schedule.Sigma,
		schedule.IdealSlots,
		len(schedule.AssignedSlots),
		performance,
		slotsJSON,
		schedule.CalculatedAt,
	)
	return err
}

// IsSchedulePosted checks if a leader schedule has been posted for an epoch.
func IsSchedulePosted(ctx context.Context, pool *pgxpool.Pool, epoch int) bool {
	var posted bool
	err := pool.QueryRow(ctx,
		`SELECT posted FROM leader_schedules WHERE epoch = $1`,
		epoch,
	).Scan(&posted)
	if err != nil {
		return false
	}
	return posted
}

// MarkSchedulePosted marks a leader schedule as posted to Telegram.
func MarkSchedulePosted(ctx context.Context, pool *pgxpool.Pool, epoch int) error {
	_, err := pool.Exec(ctx,
		`UPDATE leader_schedules SET posted = TRUE WHERE epoch = $1`,
		epoch,
	)
	return err
}
