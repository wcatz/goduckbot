package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"time"

	_ "modernc.org/sqlite"
)

// Store is the database abstraction layer for goduckbot.
// Both SQLite and PostgreSQL backends implement this interface.
type Store interface {
	InsertBlock(ctx context.Context, slot uint64, epoch int, blockHash string, vrfOutput, nonceValue []byte) error
	UpsertEvolvingNonce(ctx context.Context, epoch int, nonce []byte, blockCount int) error
	SetCandidateNonce(ctx context.Context, epoch int, nonce []byte) error
	SetFinalNonce(ctx context.Context, epoch int, nonce []byte, source string) error
	GetFinalNonce(ctx context.Context, epoch int) ([]byte, error)
	GetEvolvingNonce(ctx context.Context, epoch int) ([]byte, int, error)
	InsertLeaderSchedule(ctx context.Context, schedule *LeaderSchedule) error
	IsSchedulePosted(ctx context.Context, epoch int) bool
	MarkSchedulePosted(ctx context.Context, epoch int) error
	GetLastSyncedSlot(ctx context.Context) (uint64, error)
	Close() error
}

// SqliteStore implements Store using SQLite via modernc.org/sqlite (pure Go, no CGO).
type SqliteStore struct {
	db *sql.DB
}

const sqliteSchema = `
CREATE TABLE IF NOT EXISTS blocks (
    slot         INTEGER PRIMARY KEY,
    epoch        INTEGER NOT NULL,
    block_hash   TEXT NOT NULL,
    vrf_output   BLOB NOT NULL,
    nonce_value  BLOB NOT NULL,
    created_at   TEXT DEFAULT (datetime('now'))
);
CREATE INDEX IF NOT EXISTS idx_blocks_epoch ON blocks(epoch);

CREATE TABLE IF NOT EXISTS epoch_nonces (
    epoch            INTEGER PRIMARY KEY,
    evolving_nonce   BLOB NOT NULL,
    candidate_nonce  BLOB,
    final_nonce      BLOB,
    block_count      INTEGER DEFAULT 0,
    source           TEXT DEFAULT 'chain_sync',
    updated_at       TEXT DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS leader_schedules (
    epoch          INTEGER PRIMARY KEY,
    pool_stake     INTEGER NOT NULL,
    total_stake    INTEGER NOT NULL,
    epoch_nonce    TEXT NOT NULL,
    sigma          REAL,
    ideal_slots    REAL,
    slot_count     INTEGER DEFAULT 0,
    performance    REAL,
    slots          TEXT DEFAULT '[]',
    posted         INTEGER DEFAULT 0,
    calculated_at  TEXT DEFAULT (datetime('now'))
);
`

// NewSqliteStore opens (or creates) a SQLite database at the given path.
func NewSqliteStore(path string) (*SqliteStore, error) {
	dsn := fmt.Sprintf("file:%s?_journal_mode=WAL&_busy_timeout=5000", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening sqlite: %w", err)
	}
	db.SetMaxOpenConns(1) // SQLite is single-writer

	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("pinging sqlite: %w", err)
	}

	if _, err := db.Exec(sqliteSchema); err != nil {
		db.Close()
		return nil, fmt.Errorf("creating sqlite schema: %w", err)
	}

	log.Printf("SQLite database opened at %s", path)
	return &SqliteStore{db: db}, nil
}

func (s *SqliteStore) Close() error {
	return s.db.Close()
}

func (s *SqliteStore) InsertBlock(ctx context.Context, slot uint64, epoch int, blockHash string, vrfOutput, nonceValue []byte) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO blocks (slot, epoch, block_hash, vrf_output, nonce_value)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT (slot) DO NOTHING`,
		int64(slot), epoch, blockHash, vrfOutput, nonceValue,
	)
	return err
}

func (s *SqliteStore) UpsertEvolvingNonce(ctx context.Context, epoch int, nonce []byte, blockCount int) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO epoch_nonces (epoch, evolving_nonce, block_count, updated_at)
		 VALUES (?, ?, ?, datetime('now'))
		 ON CONFLICT (epoch) DO UPDATE SET
		   evolving_nonce = excluded.evolving_nonce,
		   block_count = excluded.block_count,
		   updated_at = datetime('now')`,
		epoch, nonce, blockCount,
	)
	return err
}

func (s *SqliteStore) SetCandidateNonce(ctx context.Context, epoch int, nonce []byte) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO epoch_nonces (epoch, evolving_nonce, candidate_nonce, updated_at)
		 VALUES (?, ?, ?, datetime('now'))
		 ON CONFLICT (epoch) DO UPDATE SET
		   candidate_nonce = excluded.candidate_nonce,
		   updated_at = datetime('now')`,
		epoch, nonce, nonce,
	)
	return err
}

func (s *SqliteStore) SetFinalNonce(ctx context.Context, epoch int, nonce []byte, source string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO epoch_nonces (epoch, evolving_nonce, final_nonce, source, updated_at)
		 VALUES (?, ?, ?, ?, datetime('now'))
		 ON CONFLICT (epoch) DO UPDATE SET
		   final_nonce = excluded.final_nonce,
		   source = excluded.source,
		   updated_at = datetime('now')`,
		epoch, nonce, nonce, source,
	)
	return err
}

func (s *SqliteStore) GetFinalNonce(ctx context.Context, epoch int) ([]byte, error) {
	var nonce []byte
	err := s.db.QueryRowContext(ctx,
		`SELECT final_nonce FROM epoch_nonces WHERE epoch = ? AND final_nonce IS NOT NULL`,
		epoch,
	).Scan(&nonce)
	if err != nil {
		return nil, err
	}
	return nonce, nil
}

func (s *SqliteStore) GetEvolvingNonce(ctx context.Context, epoch int) ([]byte, int, error) {
	var nonce []byte
	var blockCount int
	err := s.db.QueryRowContext(ctx,
		`SELECT evolving_nonce, block_count FROM epoch_nonces WHERE epoch = ?`,
		epoch,
	).Scan(&nonce, &blockCount)
	if err != nil {
		return nil, 0, err
	}
	return nonce, blockCount, nil
}

func (s *SqliteStore) InsertLeaderSchedule(ctx context.Context, schedule *LeaderSchedule) error {
	slotsJSON, err := json.Marshal(schedule.AssignedSlots)
	if err != nil {
		return fmt.Errorf("marshaling slots: %w", err)
	}

	performance := 0.0
	if schedule.IdealSlots > 0 {
		performance = float64(len(schedule.AssignedSlots)) / schedule.IdealSlots * 100
	}

	_, err = s.db.ExecContext(ctx,
		`INSERT INTO leader_schedules (epoch, pool_stake, total_stake, epoch_nonce, sigma, ideal_slots, slot_count, performance, slots, calculated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (epoch) DO UPDATE SET
		   pool_stake = excluded.pool_stake,
		   total_stake = excluded.total_stake,
		   epoch_nonce = excluded.epoch_nonce,
		   sigma = excluded.sigma,
		   ideal_slots = excluded.ideal_slots,
		   slot_count = excluded.slot_count,
		   performance = excluded.performance,
		   slots = excluded.slots,
		   calculated_at = excluded.calculated_at`,
		schedule.Epoch,
		int64(schedule.PoolStake),
		int64(schedule.TotalStake),
		schedule.EpochNonce,
		schedule.Sigma,
		schedule.IdealSlots,
		len(schedule.AssignedSlots),
		performance,
		string(slotsJSON),
		schedule.CalculatedAt.Format(time.RFC3339),
	)
	return err
}

func (s *SqliteStore) IsSchedulePosted(ctx context.Context, epoch int) bool {
	var posted int
	err := s.db.QueryRowContext(ctx,
		`SELECT posted FROM leader_schedules WHERE epoch = ?`,
		epoch,
	).Scan(&posted)
	if err != nil {
		return false
	}
	return posted != 0
}

func (s *SqliteStore) MarkSchedulePosted(ctx context.Context, epoch int) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE leader_schedules SET posted = 1 WHERE epoch = ?`,
		epoch,
	)
	return err
}

func (s *SqliteStore) GetLastSyncedSlot(ctx context.Context) (uint64, error) {
	var slot *int64
	err := s.db.QueryRowContext(ctx,
		`SELECT MAX(slot) FROM blocks`,
	).Scan(&slot)
	if err != nil {
		return 0, err
	}
	if slot == nil {
		return 0, nil
	}
	return uint64(*slot), nil
}
