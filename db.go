package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const pgSchema = `
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

// PgStore implements Store using PostgreSQL via pgx.
type PgStore struct {
	pool *pgxpool.Pool
}

// NewPgStore connects to PostgreSQL and creates tables if they don't exist.
func NewPgStore(connString string) (*PgStore, error) {
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

	if _, err := pool.Exec(ctx, pgSchema); err != nil {
		pool.Close()
		return nil, fmt.Errorf("creating schema: %w", err)
	}

	log.Println("PostgreSQL connected and schema initialized")
	return &PgStore{pool: pool}, nil
}

func (s *PgStore) Close() error {
	s.pool.Close()
	return nil
}

func (s *PgStore) InsertBlock(ctx context.Context, slot uint64, epoch int, blockHash string, vrfOutput, nonceValue []byte) (bool, error) {
	result, err := s.pool.Exec(ctx,
		`INSERT INTO blocks (slot, epoch, block_hash, vrf_output, nonce_value)
		 VALUES ($1, $2, $3, $4, $5)
		 ON CONFLICT (slot) DO NOTHING`,
		int64(slot), epoch, blockHash, vrfOutput, nonceValue,
	)
	if err != nil {
		return false, err
	}
	return result.RowsAffected() > 0, nil
}

func (s *PgStore) UpsertEvolvingNonce(ctx context.Context, epoch int, nonce []byte, blockCount int) error {
	_, err := s.pool.Exec(ctx,
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

func (s *PgStore) SetCandidateNonce(ctx context.Context, epoch int, nonce []byte) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO epoch_nonces (epoch, evolving_nonce, candidate_nonce, updated_at)
		 VALUES ($1, $2, $2, NOW())
		 ON CONFLICT (epoch) DO UPDATE SET
		   candidate_nonce = EXCLUDED.candidate_nonce,
		   updated_at = NOW()`,
		epoch, nonce,
	)
	return err
}

func (s *PgStore) SetFinalNonce(ctx context.Context, epoch int, nonce []byte, source string) error {
	_, err := s.pool.Exec(ctx,
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

func (s *PgStore) GetFinalNonce(ctx context.Context, epoch int) ([]byte, error) {
	var nonce []byte
	err := s.pool.QueryRow(ctx,
		`SELECT final_nonce FROM epoch_nonces WHERE epoch = $1 AND final_nonce IS NOT NULL`,
		epoch,
	).Scan(&nonce)
	if err != nil {
		return nil, err
	}
	return nonce, nil
}

func (s *PgStore) GetEvolvingNonce(ctx context.Context, epoch int) ([]byte, int, error) {
	var nonce []byte
	var blockCount int
	err := s.pool.QueryRow(ctx,
		`SELECT evolving_nonce, block_count FROM epoch_nonces WHERE epoch = $1`,
		epoch,
	).Scan(&nonce, &blockCount)
	if err != nil {
		return nil, 0, err
	}
	return nonce, blockCount, nil
}

func (s *PgStore) InsertLeaderSchedule(ctx context.Context, schedule *LeaderSchedule) error {
	slotsJSON, err := json.Marshal(schedule.AssignedSlots)
	if err != nil {
		return fmt.Errorf("marshaling slots: %w", err)
	}

	performance := 0.0
	if schedule.IdealSlots > 0 {
		performance = float64(len(schedule.AssignedSlots)) / schedule.IdealSlots * 100
	}

	_, err = s.pool.Exec(ctx,
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

func (s *PgStore) IsSchedulePosted(ctx context.Context, epoch int) bool {
	var posted bool
	err := s.pool.QueryRow(ctx,
		`SELECT posted FROM leader_schedules WHERE epoch = $1`,
		epoch,
	).Scan(&posted)
	if err != nil {
		return false
	}
	return posted
}

func (s *PgStore) MarkSchedulePosted(ctx context.Context, epoch int) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE leader_schedules SET posted = TRUE WHERE epoch = $1`,
		epoch,
	)
	return err
}

func (s *PgStore) GetLastSyncedSlot(ctx context.Context) (uint64, error) {
	var slot *int64
	err := s.pool.QueryRow(ctx,
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

func (s *PgStore) GetBlockHash(ctx context.Context, slot uint64) (string, error) {
	var hash string
	err := s.pool.QueryRow(ctx,
		`SELECT block_hash FROM blocks WHERE slot = $1`,
		int64(slot),
	).Scan(&hash)
	return hash, err
}

func (s *PgStore) GetForgedSlots(ctx context.Context, epoch int) ([]uint64, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT slot FROM blocks WHERE epoch = $1 ORDER BY slot`, epoch)
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

func (s *PgStore) InsertBlockBatch(ctx context.Context, blocks []BlockData) error {
	rows := make([][]interface{}, len(blocks))
	for i, b := range blocks {
		nonceValue := vrfNonceValueForEpoch(b.VrfOutput, b.Epoch, b.NetworkMagic)
		rows[i] = []interface{}{int64(b.Slot), b.Epoch, b.BlockHash, b.VrfOutput, nonceValue}
	}
	_, err := s.pool.CopyFrom(ctx,
		pgx.Identifier{"blocks"},
		[]string{"slot", "epoch", "block_hash", "vrf_output", "nonce_value"},
		pgx.CopyFromRows(rows),
	)
	return err
}

func (s *PgStore) StreamBlockNonces(ctx context.Context) (BlockNonceRows, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT epoch, slot, nonce_value, block_hash FROM blocks ORDER BY slot`,
	)
	if err != nil {
		return nil, err
	}
	return &pgBlockNonceRows{rows: rows}, nil
}

func (s *PgStore) GetBlockByHash(ctx context.Context, hashPrefix string) ([]BlockRecord, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT slot, epoch, block_hash FROM blocks WHERE block_hash LIKE $1 ORDER BY slot`,
		hashPrefix+"%",
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var records []BlockRecord
	for rows.Next() {
		var r BlockRecord
		if err := rows.Scan(&r.Slot, &r.Epoch, &r.BlockHash); err != nil {
			return nil, err
		}
		records = append(records, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return records, nil
}

func (s *PgStore) GetLeaderSchedule(ctx context.Context, epoch int) (*LeaderSchedule, error) {
	var (
		poolStake    int64
		totalStake   int64
		epochNonce   string
		sigma        float64
		idealSlots   float64
		slotsJSON    []byte
		calculatedAt time.Time
	)

	err := s.pool.QueryRow(ctx,
		`SELECT pool_stake, total_stake, epoch_nonce, sigma, ideal_slots, slots, calculated_at
		 FROM leader_schedules WHERE epoch = $1`,
		epoch,
	).Scan(&poolStake, &totalStake, &epochNonce, &sigma, &idealSlots, &slotsJSON, &calculatedAt)
	if err != nil {
		return nil, err
	}

	var slots []LeaderSlot
	if err := json.Unmarshal(slotsJSON, &slots); err != nil {
		return nil, fmt.Errorf("unmarshaling slots: %w", err)
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

// pgBlockNonceRows wraps pgx.Rows to implement BlockNonceRows.
type pgBlockNonceRows struct {
	rows      pgx.Rows
	epoch     int
	slot      uint64
	nonce     []byte
	blockHash string
	err       error
	closed    bool
}

func (r *pgBlockNonceRows) Next() bool {
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

func (r *pgBlockNonceRows) Scan() (epoch int, slot uint64, nonceValue []byte, blockHash string, err error) {
	return r.epoch, r.slot, r.nonce, r.blockHash, r.err
}

func (r *pgBlockNonceRows) Close() {
	if !r.closed {
		r.rows.Close()
		r.closed = true
	}
}

func (r *pgBlockNonceRows) Err() error {
	return r.err
}

func (s *PgStore) StreamBlockVrfOutputs(ctx context.Context) (BlockVrfRows, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT epoch, slot, vrf_output, nonce_value, block_hash FROM blocks ORDER BY slot`,
	)
	if err != nil {
		return nil, err
	}
	return &pgBlockVrfRows{rows: rows}, nil
}

type pgBlockVrfRows struct {
	rows       pgx.Rows
	epoch      int
	slot       uint64
	vrfOutput  []byte
	nonceValue []byte
	blockHash  string
	err        error
	closed     bool
}

func (r *pgBlockVrfRows) Next() bool {
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

func (r *pgBlockVrfRows) Scan() (epoch int, slot uint64, vrfOutput []byte, nonceValue []byte, blockHash string, err error) {
	return r.epoch, r.slot, r.vrfOutput, r.nonceValue, r.blockHash, r.err
}

func (r *pgBlockVrfRows) Close() {
	if !r.closed {
		r.rows.Close()
		r.closed = true
	}
}

func (r *pgBlockVrfRows) Err() error {
	return r.err
}

func (s *PgStore) GetLastNBlocks(ctx context.Context, n int) ([]BlockRecord, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT slot, epoch, block_hash FROM blocks ORDER BY slot DESC LIMIT $1`, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var records []BlockRecord
	for rows.Next() {
		var r BlockRecord
		if err := rows.Scan(&r.Slot, &r.Epoch, &r.BlockHash); err != nil {
			return nil, err
		}
		records = append(records, r)
	}
	return records, rows.Err()
}

func (s *PgStore) GetBlockCountForEpoch(ctx context.Context, epoch int) (int, error) {
	var count int
	err := s.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM blocks WHERE epoch = $1`, epoch).Scan(&count)
	return count, err
}

func (s *PgStore) GetNonceValuesForEpoch(ctx context.Context, epoch int) ([][]byte, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT nonce_value FROM blocks WHERE epoch = $1 ORDER BY slot`, epoch)
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

func (s *PgStore) GetCandidateNonce(ctx context.Context, epoch int) ([]byte, error) {
	var nonce []byte
	err := s.pool.QueryRow(ctx,
		`SELECT candidate_nonce FROM epoch_nonces WHERE epoch = $1 AND candidate_nonce IS NOT NULL`, epoch).Scan(&nonce)
	if err != nil {
		return nil, err
	}
	return nonce, nil
}

func (s *PgStore) GetLastBlockHashForEpoch(ctx context.Context, epoch int) (string, error) {
	var hash string
	err := s.pool.QueryRow(ctx,
		`SELECT block_hash FROM blocks WHERE epoch = $1 ORDER BY slot DESC LIMIT 1`, epoch).Scan(&hash)
	if err != nil {
		return "", err
	}
	return hash, nil
}

func (s *PgStore) TruncateAll(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, `TRUNCATE blocks, epoch_nonces, leader_schedules`)
	return err
}
