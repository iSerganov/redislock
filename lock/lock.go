package lock

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
)

var (
	luaRefresh   = redis.NewScript(`if redis.call("get", KEYS[1]) == ARGV[1] then return redis.call("pexpire", KEYS[1], ARGV[2]) else return 0 end`)
	luaRelease   = redis.NewScript(`if redis.call("get", KEYS[1]) == ARGV[1] then return redis.call("del", KEYS[1]) else return 0 end`)
	luaPTTL      = redis.NewScript(`if redis.call("get", KEYS[1]) == ARGV[1] then return redis.call("pttl", KEYS[1]) else return -3 end`)
	luaGetRecord = redis.NewScript(`return redis.call("get", KEYS[1])`)
)

// ErrLockNotHeld is returned when trying to release an inactive lock.
type ErrLockNotHeld struct {
	err error
}

func (e *ErrLockNotHeld) Error() string {
	msg := ""
	if e.err != nil {
		msg = e.err.Error()
	}
	return fmt.Sprintf("redislock - lock not held.%s", msg)
}

// ErrNotObtained is returned when a lock cannot be obtained.
type ErrNotObtained struct {
	err error
}

func (e *ErrNotObtained) Error() string {
	msg := ""
	if e.err != nil {
		msg = e.err.Error()
	}
	return fmt.Sprintf("redislock - lock not obtained.%s", msg)
}

// RedisClient is a minimal client interface.
type RedisClient interface {
	redis.Scripter
	SetNX(ctx context.Context, key string, value interface{}, expiration time.Duration) *redis.BoolCmd
}

// Client wraps a redis client.
type Client struct {
	client RedisClient
	tmp    []byte
	tmpMu  sync.Mutex
}

// New creates a new Client instance with a custom namespace.
func New(client RedisClient) *Client {
	return &Client{client: client}
}

// Obtain tries to obtain a new lock using a key with the given TTL.
// May return ErrNotObtained if not successful.
func (c *Client) Obtain(ctx context.Context, key string, ttl time.Duration, opt *Options) (*Lock, error) {
	// Create a random token
	token, err := c.randomToken()
	if err != nil {
		return nil, err
	}

	value := token + opt.getMetadata()
	retry := opt.getRetryStrategy()

	// make sure we don't retry forever
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, time.Now().Add(ttl))
		defer cancel()
	}

	var ticker *time.Ticker
	for {
		ok, err := c.obtain(ctx, key, value, ttl)
		if err != nil {
			return nil, err
		} else if ok {
			return &Lock{client: c, key: key, value: value}, nil
		}

		backoff := retry.NextBackoff()
		if backoff < 1 {
			return nil, &ErrNotObtained{err: fmt.Errorf("exceeded number of retries")}
		}

		if ticker == nil {
			ticker = time.NewTicker(backoff)
			defer ticker.Stop()
		}

		select {
		case <-ctx.Done():
			return nil, &ErrNotObtained{err: fmt.Errorf("context exceeded")}
		case <-ticker.C:
		}
	}
}

// Meta tries to get metadata for existing lock.
// May return ErrLockNotHeld if not successful.
func (c *Client) Meta(ctx context.Context, key string) (string, error) {
	res, err := luaGetRecord.Run(ctx, c.client, []string{key}).Result()
	if err != nil {
		return "", &ErrLockNotHeld{err: err}
	}

	if len(res.(string)) < 23 {
		return "", &ErrLockNotHeld{err: fmt.Errorf("got record with provided key, but it is not a lock")}
	}

	return res.(string)[22:], nil
}

func (c *Client) obtain(ctx context.Context, key, value string, ttl time.Duration) (bool, error) {
	return c.client.SetNX(ctx, key, value, ttl).Result()
}

func (c *Client) randomToken() (string, error) {
	c.tmpMu.Lock()
	defer c.tmpMu.Unlock()

	if len(c.tmp) == 0 {
		c.tmp = make([]byte, 16)
	}

	if _, err := io.ReadFull(rand.Reader, c.tmp); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(c.tmp), nil
}

// --------------------------------------------------------------------

// Lock represents an obtained, distributed lock.
type Lock struct {
	client *Client
	key    string
	value  string
}

// Obtain is a short-cut for New(...).Obtain(...).
func Obtain(ctx context.Context, client RedisClient, key string, ttl time.Duration, opt *Options) (*Lock, error) {
	return New(client).Obtain(ctx, key, ttl, opt)
}

// Key returns the redis key used by the lock.
func (l *Lock) Key() string {
	return l.key
}

// Token returns the token value set by the lock.
func (l *Lock) Token() string {
	return l.value[:22]
}

// Metadata returns the metadata of the lock.
func (l *Lock) Metadata() string {
	return l.value[22:]
}

// TTL returns the remaining time-to-live. Returns 0 if the lock has expired.
func (l *Lock) TTL(ctx context.Context) (time.Duration, error) {
	res, err := luaPTTL.Run(ctx, l.client.client, []string{l.key}, l.value).Result()
	if err == redis.Nil {
		return 0, nil
	} else if err != nil {
		return 0, err
	}

	if num := res.(int64); num > 0 {
		return time.Duration(num) * time.Millisecond, nil
	}
	return 0, nil
}

// Refresh extends the lock with a new TTL.
// May return ErrNotObtained if refresh is unsuccessful.
func (l *Lock) Refresh(ctx context.Context, ttl time.Duration, opt *Options) error {
	ttlVal := strconv.FormatInt(int64(ttl/time.Millisecond), 10)
	status, err := luaRefresh.Run(ctx, l.client.client, []string{l.key}, l.value, ttlVal).Result()
	if err != nil {
		return err
	} else if status == int64(1) {
		return nil
	}
	return &ErrNotObtained{}
}

// Release manually releases the lock.
// May return ErrLockNotHeld.
func (l *Lock) Release(ctx context.Context) error {
	res, err := luaRelease.Run(ctx, l.client.client, []string{l.key}, l.value).Result()
	if err != nil {
		return &ErrLockNotHeld{err}
	}

	if i, ok := res.(int64); !ok || i != 1 {
		return &ErrLockNotHeld{fmt.Errorf("unexpected script response: %d", i)}
	}
	return nil
}

// --------------------------------------------------------------------

// Options describe the options for the lock
type Options struct {
	// RetryStrategy allows to customise the lock retry strategy.
	// Default: do not retry
	RetryStrategy RetryStrategy

	// Metadata string is appended to the lock token.
	Metadata string
}

func (o *Options) getMetadata() string {
	if o != nil {
		return o.Metadata
	}
	return ""
}

func (o *Options) getRetryStrategy() RetryStrategy {
	if o != nil && o.RetryStrategy != nil {
		return o.RetryStrategy
	}
	return NoRetry()
}

// --------------------------------------------------------------------

// RetryStrategy allows to customise the lock retry strategy.
type RetryStrategy interface {
	// NextBackoff returns the next backoff duration.
	NextBackoff() time.Duration
}

type linearBackoff time.Duration

// LinearBackoff allows retries regularly with customized intervals
func LinearBackoff(backoff time.Duration) RetryStrategy {
	return linearBackoff(backoff)
}

// NoRetry acquire the lock only once.
func NoRetry() RetryStrategy {
	return linearBackoff(0)
}

func (r linearBackoff) NextBackoff() time.Duration {
	return time.Duration(r)
}

type limitedRetry struct {
	s   RetryStrategy
	cnt int64
	max int64
}

// LimitRetry limits the number of retries to max attempts.
func LimitRetry(s RetryStrategy, max int) RetryStrategy {
	return &limitedRetry{s: s, max: int64(max)}
}

func (r *limitedRetry) NextBackoff() time.Duration {
	if atomic.LoadInt64(&r.cnt) >= r.max {
		return 0
	}
	atomic.AddInt64(&r.cnt, 1)
	return r.s.NextBackoff()
}

type exponentialBackoff struct {
	cnt uint64

	min, max time.Duration
}

// ExponentialBackoff strategy is an optimization strategy with a retry time of 2**n milliseconds (n means number of times).
// You can set a minimum and maximum value, the recommended minimum value is not less than 16ms.
func ExponentialBackoff(min, max time.Duration) RetryStrategy {
	return &exponentialBackoff{min: min, max: max}
}

func (r *exponentialBackoff) NextBackoff() time.Duration {
	cnt := atomic.AddUint64(&r.cnt, 1)

	ms := 2 << 25
	if cnt < 25 {
		ms = 2 << cnt
	}

	switch d := time.Duration(ms) * time.Millisecond; {
	case d < r.min:
		return r.min
	case r.max != 0 && d > r.max:
		return r.max
	default:
		return d
	}
}
