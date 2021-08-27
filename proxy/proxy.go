package proxy

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"github.com/coinbase/memcachedbetween/listener"
	"github.com/coinbase/memcachedbetween/pool"
	"github.com/coinbase/redisbetween/config"
	"github.com/coinbase/redisbetween/handlers"
	"github.com/coinbase/redisbetween/redis"
	"github.com/coinbase/mongobetween/util"
	"github.com/mediocregopher/radix/v3"
	"net"
	"regexp"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/DataDog/datadog-go/statsd"
	"go.uber.org/zap"
)

const restartSleep = 1 * time.Second
const disconnectTimeout = 10 * time.Second

var CacheableCommands = map[string]bool{
	"GET":  true,
	"MGET": true,
}

type Proxy struct {
	log    *zap.Logger
	statsd *statsd.Client

	config *config.Config

	upstreamConfigHost string
	localConfigHost    string
	maxPoolSize        int
	minPoolSize        int
	readTimeout        time.Duration
	writeTimeout       time.Duration
	database           int
	cachePrefixes      []string

	quit chan interface{}
	kill chan interface{}

	listeners    map[string]*listener.Listener
	listenerLock sync.Mutex
	listenerWg   sync.WaitGroup

	invalidators  map[string]*Invalidator
	invalidatorWg sync.WaitGroup
	cache         *Cache
}

func NewProxy(log *zap.Logger, sd *statsd.Client, config *config.Config, label, upstreamHost string, database int, minPoolSize, maxPoolSize int, readTimeout, writeTimeout time.Duration, cachePrefixes []string) (*Proxy, error) {
	if label != "" {
		log = log.With(zap.String("cluster", label))

		var err error
		sd, err = util.StatsdWithTags(sd, []string{fmt.Sprintf("cluster:%s", label)})
		if err != nil {
			return nil, err
		}
	}
	return &Proxy{
		log:    log,
		statsd: sd,
		config: config,

		upstreamConfigHost: upstreamHost,
		localConfigHost:    localSocketPathFromUpstream(upstreamHost, database, config.LocalSocketPrefix, config.LocalSocketSuffix),
		minPoolSize:        minPoolSize,
		maxPoolSize:        maxPoolSize,
		readTimeout:        readTimeout,
		writeTimeout:       writeTimeout,
		database:           database,
		cachePrefixes:      cachePrefixes,

		quit: make(chan interface{}),
		kill: make(chan interface{}),

		listeners:    make(map[string]*listener.Listener),
		invalidators: make(map[string]*Invalidator),
		cache:        NewCache(),
	}, nil
}

func (p *Proxy) Run() error {
	return p.run()
}

func (p *Proxy) Shutdown() {
	defer func() {
		_ = recover() // "close of closed channel" panic if Shutdown() was already called
	}()
	p.listenerLock.Lock()
	for _, l := range p.listeners {
		l.Shutdown()
	}
	p.listenerLock.Unlock()
	for _, i := range p.invalidators {
		err := i.Shutdown()
		if err != nil {
			p.log.Error("error closing Invalidator", zap.Error(err))
		}
	}
	close(p.quit)
}

func (p *Proxy) Kill() {
	p.Shutdown()
	defer func() {
		_ = recover() // "close of closed channel" panic if Kill() was already called
	}()
	p.listenerLock.Lock()
	for _, l := range p.listeners {
		l.Kill()
	}
	p.listenerLock.Unlock()
	close(p.kill)
}

func (p *Proxy) run() error {
	defer func() {
		if r := recover(); r != nil {
			p.log.Error("Crashed", zap.String("panic", fmt.Sprintf("%v", r)), zap.String("stack", string(debug.Stack())))

			time.Sleep(restartSleep)

			p.log.Info("Restarting", zap.Duration("sleep", restartSleep))
			go func() {
				err := p.run()
				if err != nil {
					p.log.Error("Error restarting", zap.Error(err))
				}
			}()
		}
	}()

	l, err := p.createListener(p.localConfigHost, p.upstreamConfigHost)
	if err != nil {
		return err
	}
	defer func() {
		p.listenerWg.Wait()
		p.invalidatorWg.Wait()
	}()

	p.listenerLock.Lock()
	p.listeners[p.upstreamConfigHost] = l
	for _, l := range p.listeners {
		p.runListener(l)
	}
	p.listenerLock.Unlock()

	return nil
}

func (p *Proxy) runListener(l *listener.Listener) {
	p.listenerWg.Add(1)
	go func() {
		defer p.listenerWg.Done()

		err := l.Run()
		if err != nil {
			p.log.Error("Error", zap.Error(err))
		}
	}()
}

func (p *Proxy) runInvalidator(i *Invalidator) {
	p.invalidatorWg.Add(1)
	go func() {
		defer p.invalidatorWg.Done()
		i.Run(p.cache)
	}()
}

func (p *Proxy) interceptMessages(originalCmds []string, mm []*redis.Message, rt handlers.RoundTripper) ([]*redis.Message, error) {
	var cacheKeys [][][]byte

	if p.cachePrefixes != nil {
		k, allCached, err := p.fetchFromCache(mm, originalCmds)
		cacheKeys = k
		if err == nil {
			return allCached, nil
		}
	}

	var err error
	mm, err = rt(mm)
	if err != nil {
		return mm, err
	}

	for i, m := range mm {
		if cacheKeys != nil {
			p.cache.Set(cacheKeys[i], m)
		}

		if originalCmds[i] == "CLUSTER SLOTS" {
			b, err := redis.EncodeToBytes(m)
			if err != nil {
				p.log.Error("failed to encode cluster slots message", zap.Error(err))
				return mm, err
			}
			slots := radix.ClusterTopo{}
			err = slots.UnmarshalRESP(bufio.NewReader(bytes.NewReader(b)))
			if err != nil {
				p.log.Error("failed to unmarshal cluster slots message", zap.Error(err))
				return mm, err
			}
			for _, slot := range slots {
				p.ensureListenerForUpstream(slot.Addr, originalCmds[i])
			}
			return mm, err
		}

		if originalCmds[i] == "CLUSTER NODES" {
			if m.IsBulkBytes() {
				lines := strings.Split(string(m.Value), "\n")
				for _, line := range lines {
					lt := strings.IndexByte(line, ' ')
					rt := strings.IndexByte(line, '@')
					if lt > 0 && rt > 0 {
						hostPort := line[lt+1 : rt]
						p.ensureListenerForUpstream(hostPort, originalCmds[i])
					}
				}
			}
		}

		if m.IsError() {
			msg := string(m.Value)
			if strings.HasPrefix(msg, "MOVED") || strings.HasPrefix(msg, "ASK") {
				parts := strings.Split(msg, " ")
				if len(parts) < 3 {
					p.log.Error("failed to parse MOVED error", zap.String("original command", originalCmds[i]), zap.String("original message", msg))
					return mm, err
				}
				p.ensureListenerForUpstream(parts[2], originalCmds[i]+" "+parts[0])
			}
		}
	}
	return mm, err
}

// returns the slice of keys that were attempted to fetch, the fetched messages,
// and an error. slice of keys is nil if the messages are not cacheable
func (p *Proxy) fetchFromCache(mm []*redis.Message, originalCmds []string) ([][][]byte, []*redis.Message, error) {
	keys := make([][][]byte, len(mm))
	for i, m := range mm {
		if !CacheableCommands[originalCmds[i]] {
			return nil, nil, errors.New("not cacheable")
		}
		keys[i] = m.Keys()
	}

	var err error
	m := make([]*redis.Message, len(keys))
	for i, k := range keys {
		var cached []*redis.Message
		cached, err = p.cache.GetAll(k)
		if err != nil {
			return keys, nil, errors.New("not found")
		}
		if originalCmds[i] == "GET" {
			m[i] = cached[0]
		} else { // assumes MGET
			m[i] = redis.NewArray(cached)
		}
	}
	return keys, m, err
}

func localSocketPathFromUpstream(upstream string, database int, prefix, suffix string) string {
	path := prefix + strings.Replace(upstream, ":", "-", -1)
	if database > -1 {
		path += "-" + strconv.Itoa(database)
	}
	return path + suffix
}

func (p *Proxy) ensureListenerForUpstream(upstream, originalCmd string) {
	p.log.Info("ensuring we have a listener for", zap.String("upstream", upstream), zap.String("command", originalCmd))
	p.listenerLock.Lock()
	defer p.listenerLock.Unlock()
	_, ok := p.listeners[upstream]
	if !ok {
		local := localSocketPathFromUpstream(upstream, p.database, p.config.LocalSocketPrefix, p.config.LocalSocketSuffix)
		p.log.Info("did not find listener, creating new one", zap.String("upstream", upstream), zap.String("local", local), zap.String("command", originalCmd))
		l, err := p.createListener(local, upstream)
		if err != nil {
			p.log.Error("unable to create listener", zap.Error(err))
		}
		p.listeners[upstream] = l
		p.runListener(l)
	}
}

func (p *Proxy) createListener(local, upstream string) (*listener.Listener, error) {
	logWith := p.log.With(zap.String("upstream", upstream), zap.String("local", local))
	sdWith, err := util.StatsdWithTags(p.statsd, []string{fmt.Sprintf("upstream:%s", upstream), fmt.Sprintf("local:%s", local)})
	if err != nil {
		return nil, err
	}
	opts := []pool.ServerOption{
		pool.WithMinConnections(func(uint64) uint64 { return uint64(p.minPoolSize) }),
		pool.WithMaxConnections(func(uint64) uint64 { return uint64(p.maxPoolSize) }),
		pool.WithConnectionPoolMonitor(func(*pool.Monitor) *pool.Monitor { return poolMonitor(sdWith) }),
	}

	co := pool.WithDialer(func(dialer pool.Dialer) pool.Dialer {
		return pool.DialerFunc(func(ctx context.Context, network, address string) (net.Conn, error) {
			dlr := &net.Dialer{Timeout: 30 * time.Second}
			conn, err := dlr.DialContext(ctx, network, address)
			if err != nil {
				return conn, err
			}
			// if a db number has been specified, we need to issue a SELECT command before
			// adding that connection to the pool, so its always pinned to the right db
			if p.database > -1 {
				d := strconv.Itoa(p.database)
				cmd := redis.NewCommand("SELECT", d)
				err = redis.Encode(conn, cmd)
				if err != nil {
					logWith.Error("failed to write select command", zap.Error(err))
					return conn, err
				}
				var wm *redis.Message
				wm, err = redis.Decode(conn)
				if err != nil {
					logWith.Error("failed to read SELECT response", zap.Error(err), zap.String("response", wm.String()))
					return conn, err
				}
			}

			// if any cachePrefixes have been specified, we need an extra connection to
			// listen for invalidation events from the upstream
			if p.cachePrefixes != nil {
				p.log.Info("creating Invalidator", zap.String("upstream", upstream))
				inv, err := NewInvalidator(upstream, InvalidatorLogger(logWith))
				if err != nil {
					logWith.Error("unable to create Invalidator", zap.Error(err))
				}
				p.invalidators[upstream] = inv
				p.runInvalidator(inv)
				cmd := inv.SubscribeCommand(p.cachePrefixes)
				err = redis.Encode(conn, cmd)
				if err != nil {
					logWith.Error("failed to write CLIENT TRACKING command", zap.Error(err), zap.String("command", cmd.String()))
					return conn, err
				}

				var wm *redis.Message
				wm, err = redis.Decode(conn)
				if err != nil {
					logWith.Error("failed to read CLIENT TRACKING response", zap.Error(err), zap.String("response", wm.String()))
					return conn, err
				}
			}

			return conn, err
		})
	})
	opts = append(opts, pool.WithConnectionOptions(func(cos ...pool.ConnectionOption) []pool.ConnectionOption {
		return append(cos, co)
	}))

	s, err := pool.ConnectServer(pool.Address(upstream), opts...)
	if err != nil {
		return nil, err
	}

	connectionHandler := func(log *zap.Logger, conn net.Conn, id uint64, kill chan interface{}) {
		handlers.CommandConnection(log, p.statsd, conn, local, p.readTimeout, p.writeTimeout, id, s, kill, p.interceptMessages)
	}
	shutdownHandler := func() {
		ctx, cancel := context.WithTimeout(context.Background(), disconnectTimeout)
		defer cancel()
		_ = s.Disconnect(ctx)
	}

	return listener.New(logWith, sdWith, p.config.Network, local, p.config.Unlink, connectionHandler, shutdownHandler)
}

func poolMonitor(sd *statsd.Client) *pool.Monitor {
	checkedOut, checkedIn := util.StatsdBackgroundGauge(sd, "pool.checked_out_connections", []string{})
	opened, closed := util.StatsdBackgroundGauge(sd, "pool.open_connections", []string{})

	return &pool.Monitor{
		Event: func(e *pool.Event) {
			snake := strings.ToLower(regexp.MustCompile("([a-z0-9])([A-Z])").ReplaceAllString(e.Type, "${1}_${2}"))
			name := fmt.Sprintf("pool_event.%s", snake)
			tags := []string{
				fmt.Sprintf("address:%s", e.Address),
				fmt.Sprintf("reason:%s", e.Reason),
			}
			switch e.Type {
			case pool.ConnectionCreated:
				opened(name, tags)
			case pool.ConnectionClosed:
				closed(name, tags)
			case pool.GetSucceeded:
				checkedOut(name, tags)
			case pool.ConnectionReturned:
				checkedIn(name, tags)
			default:
				_ = sd.Incr(name, tags, 1)
			}
		},
	}
}
