package proxy

import (
	"github.com/coinbase/redisbetween/redis"
	"github.com/coocood/freecache"
	"go.uber.org/zap"
)

type Cache struct {
	c   *freecache.Cache
	ttl int
	log *zap.Logger
}

func NewCache(bytes int, ttlSeconds int, log *zap.Logger) *Cache {
	return &Cache{
		c:   freecache.NewCache(bytes), // note that this allocated up front
		log: log,
		ttl: ttlSeconds,
	}
}

// Set deals with single values and array alike, because both GET and MGET are cacheable
func (c *Cache) Set(keys [][]byte, m *redis.Message) {
	if m.IsError() { // could be MOVED, etc
		return
	}
	if m.IsArray() {
		for i, mm := range m.Array { // recurse to handle nested responses (eg, MGET)
			c.Set([][]byte{keys[i]}, mm)
		}
	} else {
		c.set(keys[0], m)
	}
}

func (c *Cache) Get(key []byte) (*redis.Message, error) {
	cached, err := c.c.Get(key)
	if err != nil {
		return nil, err
	}
	return redis.DecodeFromBytes(cached)
}

func (c *Cache) GetAll(keys [][]byte) ([]*redis.Message, error) {
	cachedMsgs := make([]*redis.Message, len(keys))
	for i, kk := range keys {
		cached, err := c.Get(kk)
		if err != nil {
			return nil, err
		}
		cachedMsgs[i] = cached
	}
	return cachedMsgs, nil
}

func (c *Cache) Del(key []byte) bool {
	return c.c.Del(key)
}

func (c *Cache) Clear() {
	c.c.Clear()
}

func (c *Cache) set(key []byte, mm *redis.Message) {
	b, err := redis.EncodeToBytes(mm)
	if err != nil {
		c.log.Error("error encoding redis message", zap.String("key", string(key)))
	}
	err = c.c.Set(key, b, c.ttl)
	if err != nil {
		c.log.Error("error writing to cache", zap.String("key", string(key)))
	}
}
