// Package db owns the Postgres connection pool and the schema migrations.
package db

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/opticaltransport/otp/sender/internal/config"
)

// Pool is the application's handle on Postgres.
//
// It is a thin wrapper rather than a bare *pgxpool.Pool so that the health check and
// the migration runner have somewhere to live that is not the store layer, and so a
// test can hold the same type a server does.
type Pool struct {
	*pgxpool.Pool
}

// Open connects to Postgres and verifies the connection before returning.
//
// The verification matters more than it looks: a pool is lazy, so without it a
// misconfigured database URL produces a process that starts cleanly, reports itself
// healthy, and fails on the first request an operator makes. Failing at startup puts
// the error where somebody is watching.
func Open(ctx context.Context, cfg config.Database) (*Pool, error) {
	pcfg, err := pgxpool.ParseConfig(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("db: %w", err)
	}

	pcfg.MaxConns = cfg.MaxConns
	pcfg.MinConns = cfg.MinConns
	pcfg.MaxConnLifetime = cfg.MaxConnLifetime
	pcfg.MaxConnIdleTime = cfg.MaxConnIdleTime
	pcfg.ConnConfig.ConnectTimeout = cfg.ConnectTimeout

	// Every session gets the same statement timeout, so a runaway query cannot hold a
	// connection for ever. It is generous because the pipeline does a few genuinely
	// large writes, and it is set here rather than per query because the point is to
	// bound the ones nobody thought to bound.
	pcfg.ConnConfig.RuntimeParams["statement_timeout"] = "300000"
	pcfg.ConnConfig.RuntimeParams["application_name"] = "otp-sender"

	pool, err := pgxpool.NewWithConfig(ctx, pcfg)
	if err != nil {
		return nil, fmt.Errorf("db: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, cfg.ConnectTimeout)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("db: could not reach %s: %w", redact(cfg.URL), err)
	}
	return &Pool{Pool: pool}, nil
}

// Healthy reports whether the database is reachable, for the readiness endpoint.
func (p *Pool) Healthy(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return p.Ping(ctx)
}

// redact removes the password from a connection string so it can appear in an error.
//
// An unreachable database is exactly the error most likely to be pasted into a ticket
// or a chat message, which makes it exactly the wrong place for a credential.
func redact(url string) string {
	at := -1
	for i := len(url) - 1; i >= 0; i-- {
		if url[i] == '@' {
			at = i
			break
		}
	}
	if at < 0 {
		return url
	}
	scheme := 0
	for i := 0; i+3 < len(url); i++ {
		if url[i] == ':' && url[i+1] == '/' && url[i+2] == '/' {
			scheme = i + 3
			break
		}
	}
	if scheme == 0 || scheme > at {
		return url
	}
	return url[:scheme] + "***@" + url[at+1:]
}
