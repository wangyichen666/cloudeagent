package store

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"cloude-agent/internal/models"
)

// ModelConfigCache 是「热状态」：模型配置只存在于内存（或 Redis，带 TTL），
// 绝不下沉到 PostgreSQL / PVC / 代码仓库。休眠后缓存可丢弃，唤醒时重新注入。
type ModelConfigCache interface {
	Set(ctx context.Context, userID string, cfg *models.ModelConfig, ttl time.Duration) error
	Get(ctx context.Context, userID string) (*models.ModelConfig, error)
	Delete(ctx context.Context, userID string) error
}

// MemoryCache 是默认的进程内实现（本地开发零依赖）。
type MemoryCache struct {
	mu   sync.RWMutex
	ttl  time.Duration
	data map[string]cacheEntry
}

type cacheEntry struct {
	cfg      models.ModelConfig
	expireAt time.Time
}

func NewMemoryCache(ttl time.Duration) *MemoryCache {
	return &MemoryCache{ttl: ttl, data: make(map[string]cacheEntry)}
}

func (c *MemoryCache) Set(ctx context.Context, userID string, cfg *models.ModelConfig, ttl time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if ttl <= 0 {
		ttl = c.ttl
	}
	c.data[userID] = cacheEntry{cfg: *cfg, expireAt: time.Now().Add(ttl)}
	return nil
}

func (c *MemoryCache) Get(ctx context.Context, userID string) (*models.ModelConfig, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	entry, ok := c.data[userID]
	if !ok {
		return nil, fmt.Errorf("model config not cached for %s", userID)
	}
	if time.Now().After(entry.expireAt) {
		return nil, fmt.Errorf("model config expired for %s", userID)
	}
	cfg := entry.cfg
	return &cfg, nil
}

func (c *MemoryCache) Delete(ctx context.Context, userID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.data, userID)
	return nil
}

// RedisCache 是多副本部署时共享的热状态实现（文档：热状态/分布式锁存 Redis）。
type RedisCache struct {
	client *redis.Client
	ttl    time.Duration
}

func NewRedisCache(addr string, ttl time.Duration) (*RedisCache, error) {
	client := redis.NewClient(&redis.Options{Addr: addr})
	if err := client.Ping(context.Background()).Err(); err != nil {
		return nil, fmt.Errorf("ping redis: %w", err)
	}
	return &RedisCache{client: client, ttl: ttl}, nil
}

func (c *RedisCache) Set(ctx context.Context, userID string, cfg *models.ModelConfig, ttl time.Duration) error {
	if ttl <= 0 {
		ttl = c.ttl
	}
	key := "cloude:modelcfg:" + userID
	// 序列化时隐藏 api_key，Redis 只是热缓存；唤醒时经 SeatService/控制面重新注入真值。
	safe := *cfg
	safe.APIKey = ""
	return c.client.Set(ctx, key, safe, ttl).Err()
}

func (c *RedisCache) Get(ctx context.Context, userID string) (*models.ModelConfig, error) {
	var cfg models.ModelConfig
	key := "cloude:modelcfg:" + userID
	if err := c.client.Get(ctx, key).Scan(&cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *RedisCache) Delete(ctx context.Context, userID string) error {
	return c.client.Del(ctx, "cloude:modelcfg:"+userID).Err()
}

func (c *RedisCache) Close() error {
	return c.client.Close()
}
