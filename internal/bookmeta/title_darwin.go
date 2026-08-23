//go:build darwin

// Package bookmeta reads the local title metadata through database/sql and a
// cgo-free SQLite driver.
package bookmeta

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strings"

	_ "modernc.org/sqlite"
)

const titleQuery = `SELECT ZDISPLAYTITLE
FROM ZBOOK
WHERE ZDISPLAYTITLE IS NOT NULL AND ZDISPLAYTITLE != ''
  AND (ZBOOKID = ?1 OR ZBOOKID = 'A:' || ?1 || '-0'
       OR ZPATH LIKE '%/' || ?1 || '/%')
ORDER BY CASE WHEN ZBOOKID = 'A:' || ?1 || '-0' THEN 0 ELSE 1 END
LIMIT 1`

// Store keeps one read-only metadata connection and prepared statement alive
// while a library root is scanned. SQLite includes the live WAL when the
// reader application is open.
type Store struct {
	database  *sql.DB
	statement *sql.Stmt
}

// Open prepares a reusable title lookup. An empty path creates a disabled
// store whose lookups return no title.
func Open(ctx context.Context, database string) (*Store, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if database == "" {
		return &Store{}, nil
	}
	dsn := (&url.URL{Scheme: "file", Path: database, RawQuery: "mode=ro"}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open book metadata: %w", err)
	}
	db.SetMaxOpenConns(1)
	statement, err := db.PrepareContext(ctx, titleQuery)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("prepare book metadata lookup: %w", err), db.Close())
	}
	return &Store{database: db, statement: statement}, nil
}

// Close releases the reusable metadata connection.
func (store *Store) Close() error {
	if store == nil {
		return nil
	}
	var err error
	if store.statement != nil {
		err = errors.Join(err, store.statement.Close())
		store.statement = nil
	}
	if store.database != nil {
		err = errors.Join(err, store.database.Close())
		store.database = nil
	}
	return err
}

// Title returns the display title for a book ID.
func (store *Store) Title(ctx context.Context, bookID string) (string, error) {
	if bookID == "" || store == nil || store.statement == nil {
		return "", nil
	}
	var title string
	if err := store.statement.QueryRowContext(ctx, bookID).Scan(&title); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return "", fmt.Errorf("read book title: %w", err)
	}
	return strings.TrimSpace(title), nil
}

// Title performs a one-off lookup. Library discovery uses Store directly so a
// multi-book scan opens and prepares SQLite only once per root.
func Title(ctx context.Context, database, bookID string) (title string, err error) {
	store, err := Open(ctx, database)
	if err != nil {
		return "", err
	}
	defer func() { err = errors.Join(err, store.Close()) }()
	return store.Title(ctx, bookID)
}
