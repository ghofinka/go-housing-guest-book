package db

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// InTx runs fn in a transaction whose row-level-security context is set to the
// caller's role and (for owners) house. The policies on visits read these
// settings; without them the app role sees no visit rows at all. The settings
// are transaction-local, so they cannot leak to another request on the same
// pooled connection.
func InTx(ctx context.Context, pool *pgxpool.Pool, role string, houseID *uuid.UUID, fn func(q *Queries) error) error {
	house := ""
	if houseID != nil {
		house = houseID.String()
	}
	return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`SELECT set_config('app.role', $1, true), set_config('app.house_id', $2, true)`,
			role, house); err != nil {
			return err
		}
		return fn(New(tx))
	})
}
