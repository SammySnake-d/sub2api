package repository

import (
	"context"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"time"
)

// Same hash tag permits atomic scripting with Redis Cluster as well as the
// standalone deployment. The interval survives lease release; only its owner
// can renew/release the in-flight permit.
func mirasimProbeKeys(scope string) []string {
	prefix := "mirasim:probe:{" + scope + "}"
	return []string{prefix + ":lease", prefix + ":interval"}
}

var claimMirasimProbe = redis.NewScript(`
if redis.call('EXISTS',KEYS[1])==1 or redis.call('EXISTS',KEYS[2])==1 then return 0 end
redis.call('PSETEX',KEYS[1],ARGV[2],ARGV[1])
redis.call('PSETEX',KEYS[2],ARGV[3],'1')
return 1`)
var renewMirasimProbe = redis.NewScript(`
if redis.call('GET',KEYS[1])~=ARGV[1] then return 0 end
redis.call('PEXPIRE',KEYS[1],ARGV[2]); return 1`)
var releaseMirasimProbe = redis.NewScript(`
if redis.call('GET',KEYS[1])~=ARGV[1] then return 0 end
return redis.call('DEL',KEYS[1])`)

func (c *gatewayCache) AcquireMirasimProbe(ctx context.Context, scope string, interval, lease time.Duration) (string, error) {
	token := uuid.NewString()
	n, err := claimMirasimProbe.Run(ctx, c.rdb, mirasimProbeKeys(scope), token, lease.Milliseconds(), interval.Milliseconds()).Int()
	if err != nil || n != 1 {
		return "", err
	}
	return token, nil
}
func (c *gatewayCache) RenewMirasimProbe(ctx context.Context, scope, token string, lease time.Duration) (bool, error) {
	n, err := renewMirasimProbe.Run(ctx, c.rdb, mirasimProbeKeys(scope)[:1], token, lease.Milliseconds()).Int()
	return n == 1, err
}
func (c *gatewayCache) ReleaseMirasimProbe(ctx context.Context, scope, token string) error {
	return releaseMirasimProbe.Run(ctx, c.rdb, mirasimProbeKeys(scope)[:1], token).Err()
}
