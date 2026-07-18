package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"
)

const LegacyBatchID = "legacy"

type Batch struct {
	ID        string `json:"id"`
	CreatedAt string `json:"created_at"`
}

// CreateBatch records one enqueue operation as a batch and returns its opaque id.
func (db *DB) CreateBatch(ctx context.Context) (Batch, error) {
	var suffix [6]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return Batch{}, fmt.Errorf("batch id: %w", err)
	}
	now := nowFn()
	b := Batch{ID: "batch-" + now.UTC().Format("20060102T150405") + "-" + hex.EncodeToString(suffix[:]), CreatedAt: timestamp(now)}
	_, err := db.sql.ExecContext(ctx, `INSERT INTO batches(id, created_at) VALUES(?,?)`, b.ID, b.CreatedAt)
	return b, err
}

// EnsureBatch is useful to import/simulate a known batch id in tests and tools.
func (db *DB) EnsureBatch(ctx context.Context, id string, created time.Time) error {
	_, err := db.sql.ExecContext(ctx,
		`INSERT INTO batches(id, created_at) VALUES(?,?) ON CONFLICT(id) DO NOTHING`, id, timestamp(created))
	return err
}

func (db *DB) ListBatches(ctx context.Context) ([]Batch, error) {
	rows, err := db.sql.QueryContext(ctx, `SELECT id, created_at FROM batches ORDER BY created_at DESC, id DESC`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Batch
	for rows.Next() {
		var b Batch
		if err := rows.Scan(&b.ID, &b.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}
