package market

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strconv"
)

// migrations/NNN_name.sql run once each, in numeric order, after the frozen schema.sql baseline.
// Never edit a merged migration. Number blocks: 001-009 Foundation, 010-019 P1 ... 060-069 P6.
//
//go:embed migrations/*.sql
var migrationFiles embed.FS

type migration struct {
	Version   int
	Name, SQL string
}

var migrationName = regexp.MustCompile(`^(\d{3})_([a-z0-9_]+)\.sql$`)

func loadMigrations(files fs.FS) ([]migration, error) {
	names, err := fs.Glob(files, "migrations/*.sql")
	if err != nil {
		return nil, err
	}
	var list []migration
	seen := map[int]string{}
	for _, path := range names {
		m := migrationName.FindStringSubmatch(path[len("migrations/"):])
		if m == nil {
			return nil, fmt.Errorf("migration %s must be named NNN_name.sql", path)
		}
		v, _ := strconv.Atoi(m[1])
		if other, dup := seen[v]; dup {
			return nil, fmt.Errorf("migrations %s and %s share version %d", other, path, v)
		}
		seen[v] = path
		body, err := fs.ReadFile(files, path)
		if err != nil {
			return nil, err
		}
		list = append(list, migration{v, m[2], string(body)})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Version < list[j].Version })
	return list, nil
}

// applyMigrations runs every migration whose version is not yet recorded, regardless of the highest
// applied version (packages merge in any order). Callers hold the startup advisory lock in tx.
func applyMigrations(ctx context.Context, tx *sql.Tx, list []migration) error {
	if _, err := tx.ExecContext(ctx, "CREATE TABLE IF NOT EXISTS schema_migrations (version integer PRIMARY KEY, name text NOT NULL, applied timestamptz NOT NULL DEFAULT now())"); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, "SELECT version FROM schema_migrations")
	if err != nil {
		return err
	}
	applied := map[int]bool{}
	for rows.Next() {
		var v int
		if err = rows.Scan(&v); err != nil {
			rows.Close()
			return err
		}
		applied[v] = true
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	for _, m := range list {
		if applied[m.Version] {
			continue
		}
		if _, err = tx.ExecContext(ctx, m.SQL); err != nil {
			return fmt.Errorf("migration %03d_%s: %w", m.Version, m.Name, err)
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO schema_migrations(version,name) VALUES($1,$2)", m.Version, m.Name); err != nil {
			return err
		}
	}
	return nil
}

func migrate(ctx context.Context, tx *sql.Tx) error {
	list, err := loadMigrations(migrationFiles)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, schema); err != nil {
		return err
	}
	return applyMigrations(ctx, tx, list)
}
