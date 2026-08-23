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

// Title returns the display title for a book ID. The database is opened
// read-only; SQLite includes its live WAL when the reader application is open.
func Title(ctx context.Context, database, bookID string) (title string, err error) {
	if database == "" || bookID == "" {
		return "", nil
	}
	dsn := (&url.URL{Scheme: "file", Path: database, RawQuery: "mode=ro"}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return "", fmt.Errorf("open book metadata: %w", err)
	}
	db.SetMaxOpenConns(1)
	defer func() { err = errors.Join(err, db.Close()) }()

	if err := db.QueryRowContext(ctx, titleQuery, bookID).Scan(&title); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return "", fmt.Errorf("read book title: %w", err)
	}
	return strings.TrimSpace(title), nil
}
