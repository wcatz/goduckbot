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

// BlockData holds block information for batch processing during chain sync.
type BlockData struct {
	Slot         uint64
	Epoch        int
	BlockHash    string
	VrfOutput    []byte
	NetworkMagic int
}

// BlockNonceRows is an iterator over blocks for nonce computation.
type BlockNonceRows interface {
	Next() bool
	Scan() (epoch int, slot uint64, nonceValue []byte, blockHash string, err error)
	Close()
	Err() error
}

// BlockVrfRows is an iterator over blocks returning raw VRF output for integrity checking.
type BlockVrfRows interface {
	Next() bool
	Scan() (epoch int, slot uint64, vrfOutput []byte, nonceValue []byte, blockHash string, err error)
	Close()
	Err() error
}

// Store is the database abstraction layer for goduckbot.
// Both SQLite and PostgreSQL backends implement this interface.
type Store interface {
	InsertBlock(ctx context.Context, slot uint64, epoch int, blockHash string, vrfOutput, nonceValue []byte) (bool, error)
	InsertBlockBatch(ctx context.Context, blocks []BlockData) error
	UpsertEvolvingNonce(ctx context.Context, epoch int, nonce []byte, blockCount int) error
	SetCandidateNonce(ctx context.Context, epoch int, nonce []byte) error
	SetFinalNonce(ctx context.Context, epoch int, nonce []byte, source string) error
	GetFinalNonce(ctx context.Context, epoch int) ([]byte, error)
	GetEvolvingNonce(ctx context.Context, epoch int) ([]byte, int, error)
	GetBlockHash(ctx context.Context, slot uint64) (string, error)
	GetBlockByHash(ctx context.Context, hashPrefix string) ([]BlockRecord, error)
	InsertLeaderSchedule(ctx context.Context, schedule *LeaderSchedule) error
	GetLeaderSchedule(ctx context.Context, epoch int) (*LeaderSchedule, error)
	IsSchedulePosted(ctx context.Context, epoch int) bool
	MarkSchedulePosted(ctx context.Context, epoch int) error
	GetForgedSlots(ctx context.Context, epoch int) ([]uint64, error)
	GetLastSyncedSlot(ctx context.Context) (uint64, error)
	StreamBlockNonces(ctx context.Context) (BlockNonceRows, error)
	StreamBlockVrfOutputs(ctx context.Context) (BlockVrfRows, error)
	GetLastNBlocks(ctx context.Context, n int) ([]BlockRecord, error)
	GetBlockCountForEpoch(ctx context.Context, epoch int) (int, error)
	GetNonceValuesForEpoch(ctx context.Context, epoch int) ([][]byte, error)
	GetVrfOutputsForEpoch(ctx context.Context, epoch int) ([]VrfBlock, error)
	GetCandidateNonce(ctx context.Context, epoch int) ([]byte, error)
	GetLastBlockHashForEpoch(ctx context.Context, epoch int) (string, error)
	GetPrevHashOfLastBlock(ctx context.Context, epoch int) (string, error)
	UpsertSlotOutcomes(ctx context.Context, epoch int, outcomes []SlotOutcome) error
	GetSlotOutcomes(ctx context.Context, epoch int) ([]SlotOutcome, error)
	IsEpochClassified(ctx context.Context, epoch int) bool
	MarkEpochClassified(ctx context.Context, epoch int) error
	TruncateAll(ctx context.Context) error
	Close() error
}

// BlockRecord holds a block's on-chain data for validation queries.
type BlockRecord struct {
	Slot      uint64
	Epoch     int
	BlockHash string
}

// VrfBlock holds the raw VRF output and epoch for a block, used for nonce recomputation.
type VrfBlock struct {
	Epoch     int
	VrfOutput []byte
}

// SlotOutcome records the classification of an assigned leader slot.
type SlotOutcome struct {
	Epoch    int    `json:"epoch"`
	Slot     uint64 `json:"slot"`
	Outcome  string `json:"outcome"`  // "forged", "battle", "missed"
	Opponent string `json:"opponent"` // bech32 pool ID (battle only)
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
    history_classified INTEGER DEFAULT 0,
    calculated_at  TEXT DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS slot_outcomes (
    epoch    INTEGER NOT NULL,
    slot     INTEGER NOT NULL,
    outcome  TEXT NOT NULL,
    opponent TEXT,
    PRIMARY KEY (epoch, slot)
);
CREATE INDEX IF NOT EXISTS idx_slot_outcomes_epoch ON slot_outcomes(epoch);
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

	// Migrations for existing databases
	db.Exec(`ALTER TABLE leader_schedules ADD COLUMN history_classified INTEGER DEFAULT 0`)

	log.Printf("SQLite database opened at %s", path)
	return &SqliteStore{db: db}, nil
}

func (s *SqliteStore) Close() error {
	return s.db.Close()
}

func (s *SqliteStore) InsertBlock(ctx context.Context, slot uint64, epoch int, blockHash string, vrfOutput, nonceValue []byte) (bool, error) {
	result, err := s.db.ExecContext(ctx,
		`INSERT INTO blocks (slot, epoch, block_hash, vrf_output, nonce_value)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT (slot) DO NOTHING`,
		int64(slot), epoch, blockHash, vrfOutput, nonceValue,
	)
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return rows > 0, nil
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

func (s *SqliteStore) GetBlockHash(ctx context.Context, slot uint64) (string, error) {
	var hash string
	err := s.db.QueryRowContext(ctx,
		`SELECT block_hash FROM blocks WHERE slot = ?`,
		int64(slot),
	).Scan(&hash)
	return hash, err
}

func (s *SqliteStore) GetForgedSlots(ctx context.Context, epoch int) ([]uint64, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT slot FROM blocks WHERE epoch = ? ORDER BY slot`, epoch)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var slots []uint64
	for rows.Next() {
		var slot int64
		if err := rows.Scan(&slot); err != nil {
			return nil, err
		}
		slots = append(slots, uint64(slot))
	}
	return slots, rows.Err()
}

func (s *SqliteStore) InsertBlockBatch(ctx context.Context, blocks []BlockData) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO blocks (slot, epoch, block_hash, vrf_output, nonce_value)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT (slot) DO NOTHING`,
	)
	if err != nil {
		return fmt.Errorf("prepare: %w", err)
	}
	defer stmt.Close()

	for _, b := range blocks {
		nonceValue := vrfNonceValueForEpoch(b.VrfOutput, b.Epoch, b.NetworkMagic)
		if _, err := stmt.ExecContext(ctx, int64(b.Slot), b.Epoch, b.BlockHash, b.VrfOutput, nonceValue); err != nil {
			return fmt.Errorf("insert slot %d: %w", b.Slot, err)
		}
	}

	return tx.Commit()
}

func (s *SqliteStore) StreamBlockNonces(ctx context.Context) (BlockNonceRows, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT epoch, slot, nonce_value, block_hash FROM blocks ORDER BY slot`,
	)
	if err != nil {
		return nil, err
	}
	return &sqliteBlockNonceRows{rows: rows}, nil
}

func (s *SqliteStore) GetBlockByHash(ctx context.Context, hashPrefix string) ([]BlockRecord, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT slot, epoch, block_hash FROM blocks WHERE block_hash LIKE ? ORDER BY slot`,
		hashPrefix+"%",
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var records []BlockRecord
	for rows.Next() {
		var r BlockRecord
		var slotInt64 int64
		if err := rows.Scan(&slotInt64, &r.Epoch, &r.BlockHash); err != nil {
			return nil, err
		}
		r.Slot = uint64(slotInt64)
		records = append(records, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return records, nil
}

func (s *SqliteStore) GetLeaderSchedule(ctx context.Context, epoch int) (*LeaderSchedule, error) {
	var (
		poolStake       int64
		totalStake      int64
		epochNonce      string
		sigma           float64
		idealSlots      float64
		slotsJSON       string
		calculatedAtStr string
	)

	err := s.db.QueryRowContext(ctx,
		`SELECT pool_stake, total_stake, epoch_nonce, sigma, ideal_slots, slots, calculated_at
		 FROM leader_schedules WHERE epoch = ?`,
		epoch,
	).Scan(&poolStake, &totalStake, &epochNonce, &sigma, &idealSlots, &slotsJSON, &calculatedAtStr)
	if err != nil {
		return nil, err
	}

	var slots []LeaderSlot
	if err := json.Unmarshal([]byte(slotsJSON), &slots); err != nil {
		return nil, fmt.Errorf("unmarshaling slots: %w", err)
	}

	calculatedAt, err := time.Parse(time.RFC3339, calculatedAtStr)
	if err != nil {
		return nil, fmt.Errorf("parsing calculated_at: %w", err)
	}

	return &LeaderSchedule{
		Epoch:         epoch,
		EpochNonce:    epochNonce,
		PoolStake:     uint64(poolStake),
		TotalStake:    uint64(totalStake),
		Sigma:         sigma,
		IdealSlots:    idealSlots,
		AssignedSlots: slots,
		CalculatedAt:  calculatedAt,
	}, nil
}

// sqliteBlockNonceRows wraps sql.Rows to implement BlockNonceRows.
type sqliteBlockNonceRows struct {
	rows      *sql.Rows
	epoch     int
	slot      uint64
	nonce     []byte
	blockHash string
	err       error
	closed    bool
}

func (r *sqliteBlockNonceRows) Next() bool {
	if r.closed {
		return false
	}
	if !r.rows.Next() {
		r.err = r.rows.Err()
		r.closed = true
		return false
	}
	var slotInt64 int64
	r.err = r.rows.Scan(&r.epoch, &slotInt64, &r.nonce, &r.blockHash)
	r.slot = uint64(slotInt64)
	return r.err == nil
}

func (r *sqliteBlockNonceRows) Scan() (epoch int, slot uint64, nonceValue []byte, blockHash string, err error) {
	return r.epoch, r.slot, r.nonce, r.blockHash, r.err
}

func (r *sqliteBlockNonceRows) Close() {
	if !r.closed {
		r.rows.Close()
		r.closed = true
	}
}

func (r *sqliteBlockNonceRows) Err() error {
	return r.err
}

func (s *SqliteStore) StreamBlockVrfOutputs(ctx context.Context) (BlockVrfRows, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT epoch, slot, vrf_output, nonce_value, block_hash FROM blocks ORDER BY slot`,
	)
	if err != nil {
		return nil, err
	}
	return &sqliteBlockVrfRows{rows: rows}, nil
}

type sqliteBlockVrfRows struct {
	rows       *sql.Rows
	epoch      int
	slot       uint64
	vrfOutput  []byte
	nonceValue []byte
	blockHash  string
	err        error
	closed     bool
}

func (r *sqliteBlockVrfRows) Next() bool {
	if r.closed {
		return false
	}
	if !r.rows.Next() {
		r.err = r.rows.Err()
		r.closed = true
		return false
	}
	var slotInt64 int64
	r.err = r.rows.Scan(&r.epoch, &slotInt64, &r.vrfOutput, &r.nonceValue, &r.blockHash)
	r.slot = uint64(slotInt64)
	return r.err == nil
}

func (r *sqliteBlockVrfRows) Scan() (epoch int, slot uint64, vrfOutput []byte, nonceValue []byte, blockHash string, err error) {
	return r.epoch, r.slot, r.vrfOutput, r.nonceValue, r.blockHash, r.err
}

func (r *sqliteBlockVrfRows) Close() {
	if !r.closed {
		r.rows.Close()
		r.closed = true
	}
}

func (r *sqliteBlockVrfRows) Err() error {
	return r.err
}

func (s *SqliteStore) GetLastNBlocks(ctx context.Context, n int) ([]BlockRecord, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT slot, epoch, block_hash FROM blocks ORDER BY slot DESC LIMIT ?`, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var records []BlockRecord
	for rows.Next() {
		var r BlockRecord
		var slotInt64 int64
		if err := rows.Scan(&slotInt64, &r.Epoch, &r.BlockHash); err != nil {
			return nil, err
		}
		r.Slot = uint64(slotInt64)
		records = append(records, r)
	}
	return records, rows.Err()
}

func (s *SqliteStore) GetBlockCountForEpoch(ctx context.Context, epoch int) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM blocks WHERE epoch = ?`, epoch).Scan(&count)
	return count, err
}

func (s *SqliteStore) GetNonceValuesForEpoch(ctx context.Context, epoch int) ([][]byte, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT nonce_value FROM blocks WHERE epoch = ? ORDER BY slot`, epoch)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var values [][]byte
	for rows.Next() {
		var nonce []byte
		if err := rows.Scan(&nonce); err != nil {
			return nil, err
		}
		values = append(values, nonce)
	}
	return values, rows.Err()
}

func (s *SqliteStore) GetVrfOutputsForEpoch(ctx context.Context, epoch int) ([]VrfBlock, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT epoch, vrf_output FROM blocks WHERE epoch = ? ORDER BY slot`, epoch)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var blocks []VrfBlock
	for rows.Next() {
		var b VrfBlock
		if err := rows.Scan(&b.Epoch, &b.VrfOutput); err != nil {
			return nil, err
		}
		blocks = append(blocks, b)
	}
	return blocks, rows.Err()
}

func (s *SqliteStore) GetCandidateNonce(ctx context.Context, epoch int) ([]byte, error) {
	var nonce []byte
	err := s.db.QueryRowContext(ctx,
		`SELECT candidate_nonce FROM epoch_nonces WHERE epoch = ? AND candidate_nonce IS NOT NULL`, epoch).Scan(&nonce)
	if err != nil {
		return nil, err
	}
	return nonce, nil
}

func (s *SqliteStore) GetLastBlockHashForEpoch(ctx context.Context, epoch int) (string, error) {
	var hash string
	err := s.db.QueryRowContext(ctx,
		`SELECT block_hash FROM blocks WHERE epoch = ? ORDER BY slot DESC LIMIT 1`, epoch).Scan(&hash)
	if err != nil {
		return "", err
	}
	return hash, nil
}

// GetPrevHashOfLastBlock returns the block hash of the second-to-last block
// in the given epoch. This is the prevHash of the last block, which is what
// the Cardano node uses for praosStateLabNonce (η_ph in the TICKN rule).
func (s *SqliteStore) GetPrevHashOfLastBlock(ctx context.Context, epoch int) (string, error) {
	var hash string
	err := s.db.QueryRowContext(ctx,
		`SELECT block_hash FROM blocks WHERE epoch = ? ORDER BY slot DESC LIMIT 1 OFFSET 1`, epoch).Scan(&hash)
	if err != nil {
		return "", err
	}
	return hash, nil
}

func (s *SqliteStore) UpsertSlotOutcomes(ctx context.Context, epoch int, outcomes []SlotOutcome) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO slot_outcomes (epoch, slot, outcome, opponent)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT (epoch, slot) DO UPDATE SET
		   outcome = excluded.outcome,
		   opponent = excluded.opponent`)
	if err != nil {
		return fmt.Errorf("prepare: %w", err)
	}
	defer stmt.Close()

	for _, o := range outcomes {
		if _, err := stmt.ExecContext(ctx, o.Epoch, int64(o.Slot), o.Outcome, o.Opponent); err != nil {
			return fmt.Errorf("upsert slot %d: %w", o.Slot, err)
		}
	}
	return tx.Commit()
}

func (s *SqliteStore) GetSlotOutcomes(ctx context.Context, epoch int) ([]SlotOutcome, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT epoch, slot, outcome, COALESCE(opponent, '') FROM slot_outcomes WHERE epoch = ? ORDER BY slot`, epoch)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var outcomes []SlotOutcome
	for rows.Next() {
		var o SlotOutcome
		var slot int64
		if err := rows.Scan(&o.Epoch, &slot, &o.Outcome, &o.Opponent); err != nil {
			return nil, err
		}
		o.Slot = uint64(slot)
		outcomes = append(outcomes, o)
	}
	return outcomes, rows.Err()
}

func (s *SqliteStore) IsEpochClassified(ctx context.Context, epoch int) bool {
	var classified int
	err := s.db.QueryRowContext(ctx,
		`SELECT history_classified FROM leader_schedules WHERE epoch = ?`, epoch).Scan(&classified)
	if err != nil {
		return false
	}
	return classified != 0
}

func (s *SqliteStore) MarkEpochClassified(ctx context.Context, epoch int) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE leader_schedules SET history_classified = 1 WHERE epoch = ?`, epoch)
	return err
}

func (s *SqliteStore) TruncateAll(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	for _, table := range []string{"blocks", "epoch_nonces", "leader_schedules", "slot_outcomes"} {
		if _, err := tx.ExecContext(ctx, "DELETE FROM "+table); err != nil {
			return fmt.Errorf("clearing %s: %w", table, err)
		}
	}
	return tx.Commit()
}
