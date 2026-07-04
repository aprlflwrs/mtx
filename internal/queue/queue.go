// Package queue is the persistent job queue and history, backed by SQLite.
// One row per (file version, attempt outcome): a file is identified by its
// path plus size and mtime, so a file that changes on disk is eligible
// again, while an unchanged file is never enqueued twice.
package queue

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"time"

	_ "modernc.org/sqlite"

	"mtx/internal/encode"
)

const schema = `
CREATE TABLE IF NOT EXISTS jobs (
	id          INTEGER PRIMARY KEY,
	path        TEXT    NOT NULL,
	size_bytes  INTEGER NOT NULL,
	mtime_unix  INTEGER NOT NULL,
	grain       INTEGER NOT NULL DEFAULT 0,
	status      TEXT    NOT NULL DEFAULT 'pending', -- pending|running|done|failed|skipped
	profile     TEXT    NOT NULL DEFAULT '',
	skip_reason TEXT    NOT NULL DEFAULT '',
	error       TEXT    NOT NULL DEFAULT '',
	size_after  INTEGER NOT NULL DEFAULT 0,
	created_at  TEXT    NOT NULL,
	finished_at TEXT    NOT NULL DEFAULT '',
	UNIQUE (path, size_bytes, mtime_unix)
);
CREATE INDEX IF NOT EXISTS jobs_by_status ON jobs (status);
`

type Store struct {
	db *sql.DB
}

type Job struct {
	ID    int64
	Path  string
	Grain bool
}

func Open(dbPath string) (*Store, error) {
	db, err := sql.Open("sqlite", dbPath+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("initializing queue schema: %w", err)
	}
	// Jobs left "running" by a previous process were interrupted; they are
	// safe to retry because originals are only ever swapped after a verified
	// encode.
	if _, err := db.Exec(`UPDATE jobs SET status = 'pending' WHERE status = 'running'`); err != nil {
		return nil, err
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// Enqueue adds a file to the queue unless this exact version of it (same
// path, size, and mtime) has already been seen. Reports whether a new job
// was created.
func (s *Store) Enqueue(path string, grain bool) (bool, error) {
	stat, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	result, err := s.db.Exec(`
		INSERT OR IGNORE INTO jobs (path, size_bytes, mtime_unix, grain, created_at)
		VALUES (?, ?, ?, ?, ?)`,
		path, stat.Size(), stat.ModTime().Unix(), grain, now())
	if err != nil {
		return false, err
	}
	inserted, err := result.RowsAffected()
	return inserted > 0, err
}

// ClaimNext atomically takes the oldest pending job and marks it running.
// Returns nil when the queue is empty.
func (s *Store) ClaimNext() (*Job, error) {
	row := s.db.QueryRow(`
		UPDATE jobs SET status = 'running'
		WHERE id = (SELECT id FROM jobs WHERE status = 'pending' ORDER BY id LIMIT 1)
		RETURNING id, path, grain`)
	var job Job
	if err := row.Scan(&job.ID, &job.Path, &job.Grain); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &job, nil
}

// RecordResult stores how a claimed job ended: done, skipped, or failed.
func (s *Store) RecordResult(jobID int64, result encode.Result, processErr error) error {
	var err error
	switch {
	case processErr != nil:
		_, err = s.db.Exec(
			`UPDATE jobs SET status = 'failed', error = ?, finished_at = ? WHERE id = ?`,
			processErr.Error(), now(), jobID)
	case result.Skipped != "":
		_, err = s.db.Exec(
			`UPDATE jobs SET status = 'skipped', skip_reason = ?, finished_at = ? WHERE id = ?`,
			result.Skipped, now(), jobID)
	default:
		_, err = s.db.Exec(
			`UPDATE jobs SET status = 'done', profile = ?, size_after = ?, finished_at = ? WHERE id = ?`,
			string(result.Profile), result.SizeAfter, now(), jobID)
	}
	return err
}

type Summary struct {
	CountByStatus map[string]int
	BytesSaved    int64
}

func (s *Store) Summarize() (Summary, error) {
	summary := Summary{CountByStatus: map[string]int{}}

	rows, err := s.db.Query(`SELECT status, COUNT(*) FROM jobs GROUP BY status`)
	if err != nil {
		return summary, err
	}
	defer rows.Close()
	for rows.Next() {
		var status string
		var count int
		if err := rows.Scan(&status, &count); err != nil {
			return summary, err
		}
		summary.CountByStatus[status] = count
	}
	if err := rows.Err(); err != nil {
		return summary, err
	}

	err = s.db.QueryRow(
		`SELECT COALESCE(SUM(size_bytes - size_after), 0) FROM jobs WHERE status = 'done'`,
	).Scan(&summary.BytesSaved)
	return summary, err
}

func now() string { return time.Now().UTC().Format(time.RFC3339) }
