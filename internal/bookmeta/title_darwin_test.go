//go:build darwin

package bookmeta

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

func TestTitleReadsLiveWAL(t *testing.T) {
	database := filepath.Join(t.TempDir(), "BookData.sqlite")
	db, err := sql.Open("sqlite", database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`PRAGMA journal_mode=WAL`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE ZBOOK (
		ZDISPLAYTITLE TEXT, ZBOOKID TEXT, ZPATH TEXT
	)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO ZBOOK VALUES (?, ?, ?)`,
		"  A Real Title  ", "A:B000000001-0", "/books/B000000001/content"); err != nil {
		t.Fatal(err)
	}

	title, err := Title(context.Background(), database, "B000000001")
	if err != nil {
		t.Fatal(err)
	}
	if title != "A Real Title" {
		t.Fatalf("title = %q, want %q", title, "A Real Title")
	}
}

func TestTitleHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Title(ctx, filepath.Join(t.TempDir(), "missing.sqlite"), "B000000001")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context cancellation", err)
	}
}
