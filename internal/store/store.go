package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/LJAYi/komari-bridge/internal/model"
	_ "modernc.org/sqlite"
)

var ErrNotBound = errors.New("resource has no Komari binding")

type Store struct{ db *sql.DB }

func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create database directory: %w", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	s := &Store{db: db}
	if err := s.migrate(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		db.Close()
		return nil, fmt.Errorf("secure database permissions: %w", err)
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS resources (
    source_type        TEXT NOT NULL,
    source_id          TEXT NOT NULL,
    external_id        TEXT NOT NULL,
    parent_external_id TEXT NOT NULL DEFAULT '',
    name               TEXT NOT NULL,
    resource_type      TEXT NOT NULL,
    group_name         TEXT NOT NULL DEFAULT '',
    komari_uuid        TEXT NOT NULL DEFAULT '',
    komari_token       TEXT NOT NULL DEFAULT '',
    last_seen          DATETIME NOT NULL,
    last_reported      DATETIME,
    PRIMARY KEY (source_type, source_id, external_id)
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_resources_komari_uuid
    ON resources(komari_uuid) WHERE komari_uuid <> '';
`)
	if err != nil {
		return fmt.Errorf("migrate database: %w", err)
	}
	return nil
}

func (s *Store) Observe(ctx context.Context, snapshot model.Snapshot) error {
	if !snapshot.Identity.Valid() {
		return fmt.Errorf("invalid resource identity")
	}
	seen := snapshot.CollectedAt.UTC()
	if seen.IsZero() {
		seen = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO resources (
    source_type, source_id, external_id, parent_external_id,
    name, resource_type, group_name, last_seen
) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(source_type, source_id, external_id) DO UPDATE SET
    parent_external_id = excluded.parent_external_id,
    name = excluded.name,
    resource_type = excluded.resource_type,
    group_name = excluded.group_name,
    last_seen = excluded.last_seen`,
		snapshot.Identity.SourceType, snapshot.Identity.SourceID, snapshot.Identity.ExternalID,
		snapshot.ParentExternalID, snapshot.Name, snapshot.ResourceType, snapshot.Group, seen)
	if err != nil {
		return fmt.Errorf("observe resource: %w", err)
	}
	return nil
}

func (s *Store) Binding(ctx context.Context, identity model.Identity) (model.Binding, error) {
	var b model.Binding
	b.Identity = identity
	err := s.db.QueryRowContext(ctx, `
SELECT komari_uuid, komari_token FROM resources
WHERE source_type = ? AND source_id = ? AND external_id = ?`,
		identity.SourceType, identity.SourceID, identity.ExternalID).Scan(&b.KomariUUID, &b.Token)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && (b.KomariUUID == "" || b.Token == "")) {
		return model.Binding{}, ErrNotBound
	}
	if err != nil {
		return model.Binding{}, fmt.Errorf("get binding: %w", err)
	}
	return b, nil
}

func (s *Store) Bind(ctx context.Context, binding model.Binding) error {
	if !binding.Identity.Valid() || binding.KomariUUID == "" || binding.Token == "" {
		return fmt.Errorf("invalid binding")
	}
	result, err := s.db.ExecContext(ctx, `
UPDATE resources SET komari_uuid = ?, komari_token = ?
WHERE source_type = ? AND source_id = ? AND external_id = ?`,
		binding.KomariUUID, binding.Token, binding.Identity.SourceType,
		binding.Identity.SourceID, binding.Identity.ExternalID)
	if err != nil {
		return fmt.Errorf("save binding: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read bind result: %w", err)
	}
	if rows != 1 {
		return fmt.Errorf("bind resource: resource not found")
	}
	return nil
}

func (s *Store) MarkReported(ctx context.Context, identity model.Identity, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `
UPDATE resources SET last_reported = ?
WHERE source_type = ? AND source_id = ? AND external_id = ?`,
		at.UTC(), identity.SourceType, identity.SourceID, identity.ExternalID)
	if err != nil {
		return fmt.Errorf("mark reported: %w", err)
	}
	return nil
}
