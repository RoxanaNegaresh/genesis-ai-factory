package sqlstore

import (
	"context"
	"database/sql"
	"fmt"
	"runtime"
	"time"

	// Pure-Go SQLite: no cgo, so the server cross-compiles to Windows and Linux
	// from any host and ships as a single static binary.
	_ "modernc.org/sqlite"
)

// Options tune the connection pool.
type Options struct {
	Driver          string
	DSN             string
	MaxOpenConns    int
	MaxIdleConns    int
	ConnMaxLifetime time.Duration
	ConnMaxIdleTime time.Duration
}

// DefaultOptions derives a sane pool size from the machine.
func DefaultOptions(driver, dsn string) Options {
	maxOpen := 4 * runtime.GOMAXPROCS(0)
	if maxOpen < 8 {
		maxOpen = 8
	}
	return Options{
		Driver:          driver,
		DSN:             dsn,
		MaxOpenConns:    maxOpen,
		MaxIdleConns:    maxOpen / 2,
		ConnMaxLifetime: time.Hour,
		ConnMaxIdleTime: 10 * time.Minute,
	}
}

// Open connects to the database and returns a configured store.
func Open(ctx context.Context, opts Options) (*Store, error) {
	driverName := opts.Driver
	dialect := Dialect(opts.Driver)

	switch dialect {
	case SQLite:
		driverName = "sqlite"
		// SQLite serialises writes at the file level. Allowing many open
		// connections buys nothing and produces SQLITE_BUSY under load, so the
		// pool is capped and writes are serialised in the event repository.
		if opts.MaxOpenConns > 8 {
			opts.MaxOpenConns = 8
		}
	case Postgres:
		driverName = "pgx"
	default:
		return nil, fmt.Errorf("unsupported database driver %q", opts.Driver)
	}

	db, err := sql.Open(driverName, opts.DSN)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", opts.Driver, err)
	}
	db.SetMaxOpenConns(opts.MaxOpenConns)
	db.SetMaxIdleConns(opts.MaxIdleConns)
	db.SetConnMaxLifetime(opts.ConnMaxLifetime)
	db.SetConnMaxIdleTime(opts.ConnMaxIdleTime)

	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("connect to %s: %w", opts.Driver, err)
	}

	return New(db, dialect), nil
}
