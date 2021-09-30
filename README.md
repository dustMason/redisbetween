# redisbetween

This is a connection pooling proxy for redis. It was originally built for an application that was hitting
connection limits against its redis clusters. Its purpose is to solve a specific problem: many application processes
that cannot otherwise share a connection pool need to connect to a single redis cluster.

redisbetween supports both standalone and clustered redis deployments with the following caveats:

- **Blocking Commands** that cause the client to hold a connection open such as `BLPOP`, `BRPOPLPUSH`, `SUBSCRIBE` and
`WAIT` are not allowed by redisbetween because of the risk of exhausting the connection pool. For example, redisbetween
is not a good solution for sidekiq servers which rely on these blocking commands.

- **Pipelines** are supported, but require a client patch. Normally, redis clients may send multiple commands
back-to-back before reading a batch of responses all at once from the server. Since redisbetween shares upstream
connections among many clients, it relies on special "signal" messages to indicate the beginning and end of batched
commands. Clients using redisbetween must prepend a `GET 🔜` and append a `GET 🔚` to their batch of messages in order
for redisbetween to properly proxy the pipelined commands and responses.

- **Transactions are only supported _within pipelines_.** This means that the commands `DISCARD`, `EXEC`, `MULTI`,
`UNWATCH` and `WATCH` will return errors from the proxy before they reach an upstream host unless they occur between the
two signal values described above in the **Pipelines** section. This is because redis stores state about open
transactions on the server side, attached to each client connection. In order to support transactions without
connection-pinning, we require that the full set of operations be sent in one batch so that the connection we check back
into the pool does not leak state to other clients.

- The **SELECT** command, which is used by redis clients when connecting to a db other than the default `0`, is not
allowed. However, redisbetween _does_ support multiple dbs by specifying the db number in the endpoint url path. With an
example URL of `redis://example.com/3`, the resulting connection pool would be mapped to the socket path
`/var/tmp/redisbetween-example.com-3.sock` suffix, and all connections would issue a `SELECT 3` command before entering
the pool. Note that each db number gets its own connection pool, so adjust `maxpoolsize` accordingly when using this
feature.

- The **AUTH** command is not supported. If this is needed in the future, we
could add support by pre-emptively sending the AUTH command on all new connections, like we do with `SELECT`.

### How it works

redisbetween creates a connection pool for each upstream redis server it discovers (either via configuration at start
time, snooping on `CLUSTER` commands or via `ASK`/`MOVED` errors) and maps a local unix socket to that pool.
Applications running on the same host can connect to redis via this unix socket instead of connecting directly to the
redis server, thus sharing a relatively smaller number of connections among the many processes on a machine.

Upon startup, redisbetween creates a pool of connections to the redis endpoint provided and listens on a unix socket
named after the endpoint. By default, it will be named `/var/tmp/redisbetween-${host}-${port}(-${db})(-ro).sock`. This can be
customized using the `-localsocketprefix` and `-localsocketsuffix` options. For standalone redis deployments, this will
be the only socket created. However, redisbetween will inspect responses to `CLUSTER` commands, looking for references to
cluster members that it hasn't yet seen. When it sees a new cluster member, it allocates a new connection pool and unix
socket for it before relaying the response to the client.

### Server-Assisted Client Side Caching

redisbetween implements a [caching strategy described in the redis docs](https://redis.io/topics/client-side-caching)
that relies on a new `CLIENT TRACKING` command added in Redis 6. redisbetween can maintain a local cache of all keys
with a given prefix pattern which receives updates pushed down by redis itself. This means that the cache can be both
efficient and lively without adding any complexity to the applications communicating through redisbetween.

To use it, configure an upstream with a `cache_prefixes` URL param with a comma-separated list of prefixes. When
creating each upstream connection, redisbetween spins up an "invalidator" goroutine. It then issues a
`CLIENT TRACKING on REDIRECT {invalidatorConnectionId} BCAST` which tells the upstream to broadcast invalidation events
about all tracked keys to the invalidator's connection. The invalidator purges entries in its cache by reading these
messages.

When redisbetween sees a `GET` or `MGET` command, it first checks the cache to see if _all_ of the values requested
are present and returns them immediately if possible. Otherwise, the original query is forwarded to the upstream, and
the cache is updated with the values returned before relaying them to the client.

Note that sending `CLIENT TRACKING` commands from the client directly is not supported.

### Redisbetween Gem

The [ruby](/ruby) directory contains a ruby gem that monkey patches the ruby redis client to support redisbetween. See
the [readme](/ruby/README.md) for more details.

Here's an example of a patch to the go-redis client. Note that this one does not handle db number selection, as that is
not supported by redis cluster anyway.

```go
readonly := true
opt := &redis.ClusterOptions{
    Addrs: []string{address},
    Dialer: func(ctx context.Context, network, addr string) (net.Conn, error) {
        if strings.Contains(network, "tcp") {
            host, port, err := net.SplitHostPort(addr)
            if err != nil {
                return nil, err
            }
            addr = "/var/tmp/redisbetween-" + host + "-" + port
            if readonly {
                addr += "-ro"
            }
            addr += ".sock"
            network = "unix"
        }
        return net.Dial(network, addr)
    },
}
client := redis.NewClusterClient(opt)
res := client.Do(context.Background(), "ping")
```

### Installation
```
go install github.com/coinbase/redisbetween
```

### Usage
```
Usage: bin/redisbetween [OPTIONS] uri1 [uri2] ...
  -localsocketprefix string
    	prefix to use for unix socket filenames (default "/var/tmp/redisbetween-")
  -localsocketsuffix string
    	suffix to use for unix socket filenames (default ".sock")
  -loglevel string
    	one of: debug, info, warn, error, dpanic, panic, fatal (default "info")
  -network string
    	one of: tcp, tcp4, tcp6, unix or unixpacket (default "unix")
  -pretty
    	pretty print logging
  -statsd string
    	statsd address (default "localhost:8125")
  -unlink
    	unlink existing unix sockets before listening
```

Each URI can specify the following settings as GET params:

- `minpoolsize` sets the min connection pool size for this host. Defaults to 1
- `maxpoolsize` sets the max connection pool size for this host. Defaults to 10
- `label` optionally tags events and metrics for proxy activity on this host or cluster. Defaults to `""` (disabled)
- `readtimeout` timeout for reads to this upstream. Defaults to 5s
- `writetimeout` timeout for writes to this upstream. Defaults to 5s
- `cacheprefixes` maintains a local cache for GETs and MGETs to keys with this prefix. Defaults to empty (nil)
- `cachesizemb` size, as an int of MB, of the local cache. Defaults to 100
- `cachettlseconds` time, as an int of seconds, to keep values cached locally. Defaults to 360
- `readonly` every connection issues a [READONLY](https://redis.io/commands/readonly) command before entering the pool. Defaults to false
