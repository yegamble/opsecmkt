package market

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/stdlib"
)

// Commit reaches PostgreSQL, but the caller loses its acknowledgement. The
// row and audit exist, so an upload response must not promise they do not.
func TestUploadLostCommitAcknowledgementReportsUnknownOutcome(t *testing.T) {
	for _, tc := range []struct {
		name, path, place, title string
		parts                    func(*testEnv, string, string, []byte) []part
	}{
		{"listing photo", "/listings/photo", "listing", "Photo status unknown", func(e *testEnv, session, listing string, image []byte) []part {
			return e.photoParts(session, listing, image)
		}},
		{"avatar", "/account/avatar", "account", "Avatar status unknown", func(e *testEnv, session, _ string, image []byte) []part {
			return e.avatarParts(session, image)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, _ := uploadEnv(t)
			owner, session := e.user("lost_commit_vendor", "vendor")
			listing := e.product(owner, "physical")
			base, err := stdlib.GetDefaultDriver().(driver.DriverContext).OpenConnector(os.Getenv("DATABASE_URL"))
			if err != nil {
				t.Fatal(err)
			}
			lost := &atomic.Bool{}
			lost.Store(true)
			wrapped := sql.OpenDB(&lostCommitConnector{Connector: base, lost: lost})
			original := e.DB
			e.A.db = wrapped
			t.Cleanup(func() { original.Close() })

			w := e.upload(tc.path, session, tc.parts(e, session, listing, smallJPEG(t))...)
			if lost.Load() {
				t.Fatal("Commit did not reach the injected acknowledgement loss")
			}
			if n := e.imageCount(); n != 1 {
				t.Fatalf("server committed %d images, want 1", n)
			}
			var audits int
			if err := e.DB.QueryRow("SELECT count(*) FROM audit_events WHERE user_id=$1 AND action LIKE 'Added %'", owner).Scan(&audits); err != nil || audits != 1 {
				t.Fatalf("server committed %d upload audits: %v", audits, err)
			}
			if w.Code != 500 || w.Header().Get("Retry-After") != "" ||
				!strings.Contains(w.Body.String(), "may have been saved") ||
				!strings.Contains(w.Body.String(), tc.title) || strings.Contains(w.Body.String(), "not saved") ||
				!strings.Contains(w.Body.String(), "reload the "+tc.place+" to check before uploading again") ||
				strings.Contains(w.Body.String(), "Nothing was stored") || strings.Contains(w.Body.String(), "Try again in a few seconds") {
				t.Fatalf("lost Commit acknowledgement: status=%d retry=%q body=%s", w.Code, w.Header().Get("Retry-After"), w.Body.String())
			}
		})
	}
}

type lostCommitConnector struct {
	driver.Connector
	lost *atomic.Bool
}

func (c *lostCommitConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.Connector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &lostCommitConn{Conn: conn, lost: c.lost}, nil
}

type lostCommitConn struct {
	driver.Conn
	lost *atomic.Bool
}

func (c *lostCommitConn) Begin() (driver.Tx, error) {
	tx, err := c.Conn.Begin()
	if err != nil {
		return nil, err
	}
	return &committedThenLostTx{Tx: tx, lost: c.lost}, nil
}

func (c *lostCommitConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	tx, err := c.Conn.(driver.ConnBeginTx).BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &committedThenLostTx{Tx: tx, lost: c.lost}, nil
}

func (c *lostCommitConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	return c.Conn.(driver.QueryerContext).QueryContext(ctx, query, args)
}

func (c *lostCommitConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	return c.Conn.(driver.ExecerContext).ExecContext(ctx, query, args)
}

type committedThenLostTx struct {
	driver.Tx
	lost *atomic.Bool
}

func (tx *committedThenLostTx) Commit() error {
	if err := tx.Tx.Commit(); err != nil {
		return err
	}
	if tx.lost.CompareAndSwap(true, false) {
		return driver.ErrBadConn
	}
	return nil
}
