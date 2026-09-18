package repository

import (
	"context"
	"fmt"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"strings"
)

func (r *accountRepository) ClearMirasimCapacityIfUnchanged(ctx context.Context, id int64, scope, limitedAt, resetAt string) (bool, error) {
	if !strings.HasPrefix(scope, "mirasim:capacity:") {
		return false, fmt.Errorf("invalid capacity scope")
	}
	result, err := clientFromContext(ctx, r.client).ExecContext(ctx, `
UPDATE accounts SET extra=extra #- ARRAY['model_rate_limits',$1]::text[],updated_at=NOW()
WHERE id=$2 AND deleted_at IS NULL
AND extra->'model_rate_limits'->$1->>'rate_limited_at'=$3
AND extra->'model_rate_limits'->$1->>'rate_limit_reset_at'=$4`, scope, id, limitedAt, resetAt)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if n == 0 {
		return false, nil
	}
	if err := enqueueSchedulerOutbox(ctx, r.sql, service.SchedulerOutboxEventAccountChanged, &id, nil, nil); err != nil {
		return true, err
	}
	r.syncSchedulerAccountSnapshot(ctx, id)
	return true, nil
}
