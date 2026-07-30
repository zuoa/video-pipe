// Package store persists stream configuration in SQLite.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"video-pipe/internal/model"

	// Pure-Go SQLite driver (no CGO), enabling static, single-binary builds.
	_ "modernc.org/sqlite"
)

// ErrNotFound is returned when no stream matches the requested name.
var ErrNotFound = errors.New("stream not found")

// Store wraps a SQLite database holding stream definitions.
type Store struct {
	db *sql.DB
}

// Open connects to the database at path, applies migrations, and returns a Store.
func Open(path string) (*Store, error) {
	// WAL + a busy timeout make concurrent reads/writes safe under our single
	// connection pool. modernc parses _pragma query parameters.
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(on)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// SQLite serializes writers; a single connection avoids "database is locked".
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return s, nil
}

func (s *Store) migrate() error {
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS streams (
		id            INTEGER PRIMARY KEY AUTOINCREMENT,
		name          TEXT    NOT NULL UNIQUE,
		source_url    TEXT    NOT NULL,
		source_type   TEXT    NOT NULL,
		live          INTEGER NOT NULL,
		desired_state TEXT    NOT NULL,
		created_at    TEXT    NOT NULL
	)`)
	return err
}

// Close releases the database connection.
func (s *Store) Close() error { return s.db.Close() }

// Create persists a new stream and returns it with the assigned ID/timestamp.
func (s *Store) Create(ctx context.Context, st model.Stream) (model.Stream, error) {
	st.CreatedAt = time.Now().UTC()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO streams (name, source_url, source_type, live, desired_state, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		st.Name, st.SourceURL, st.SourceType, boolToInt(st.Live), st.DesiredState, st.CreatedAt.Format(time.RFC3339),
	)
	if err != nil {
		return model.Stream{}, fmt.Errorf("insert stream: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return model.Stream{}, fmt.Errorf("last insert id: %w", err)
	}
	st.ID = id
	return st, nil
}

// Get returns the stream with the given name.
func (s *Store) Get(ctx context.Context, name string) (model.Stream, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, name, source_url, source_type, live, desired_state, created_at
		 FROM streams WHERE name = ?`, name)
	return scanStream(row)
}

// List returns all streams, newest first.
func (s *Store) List(ctx context.Context) ([]model.Stream, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, source_url, source_type, live, desired_state, created_at
		 FROM streams ORDER BY created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("list streams: %w", err)
	}
	defer rows.Close()
	var out []model.Stream
	for rows.Next() {
		st, err := scanStream(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// SetDesired updates the desired runtime state of a stream.
func (s *Store) SetDesired(ctx context.Context, name, state string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE streams SET desired_state = ? WHERE name = ?`, state, name)
	if err != nil {
		return fmt.Errorf("update desired_state: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// Delete removes a stream by name.
func (s *Store) Delete(ctx context.Context, name string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM streams WHERE name = ?`, name)
	if err != nil {
		return fmt.Errorf("delete stream: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// scanner abstracts *sql.Row and *sql.Rows for scanStream.
type scanner interface {
	Scan(dest ...any) error
}

func scanStream(sc scanner) (model.Stream, error) {
	var st model.Stream
	var live int
	var created string
	if err := sc.Scan(&st.ID, &st.Name, &st.SourceURL, &st.SourceType, &live, &st.DesiredState, &created); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return model.Stream{}, ErrNotFound
		}
		return model.Stream{}, fmt.Errorf("scan stream: %w", err)
	}
	st.Live = live == 1
	t, err := time.Parse(time.RFC3339, created)
	if err == nil {
		st.CreatedAt = t
	}
	return st, nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
