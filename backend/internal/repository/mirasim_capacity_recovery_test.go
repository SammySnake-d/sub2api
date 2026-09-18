package repository

import (
	"context"
	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/DATA-DOG/go-sqlmock"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestClearMirasimCapacityIfUnchanged(t *testing.T) {
	for _, rows := range []int64{0, 1} {
		db, mock, err := sqlmock.New()
		require.NoError(t, err)
		client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
		repo := newAccountRepositoryWithSQL(client, db, nil)
		mock.ExpectExec(`(?s)UPDATE accounts SET extra=extra #-.+WHERE id=\$2.+rate_limited_at.+=\$3.+rate_limit_reset_at.+=\$4`).
			WithArgs("mirasim:capacity:claude-fable-5-1", int64(17), "2026-09-18T00:00:00Z", "2026-09-18T00:00:30Z").WillReturnResult(sqlmock.NewResult(0, rows))
		if rows > 0 {
			mock.ExpectExec("INSERT INTO scheduler_outbox").WithArgs(service.SchedulerOutboxEventAccountChanged, int64(17), nil, nil, sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(1, 1))
		}
		cleared, err := repo.ClearMirasimCapacityIfUnchanged(context.Background(), 17, "mirasim:capacity:claude-fable-5-1", "2026-09-18T00:00:00Z", "2026-09-18T00:00:30Z")
		require.NoError(t, err)
		require.Equal(t, rows > 0, cleared)
		require.NoError(t, mock.ExpectationsWereMet())
		_ = client.Close()
	}
}
