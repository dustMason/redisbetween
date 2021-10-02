- [x] implement new url param
- [x] invalidator
- [x] RWlock around cache reads/writes (built into freecache)
- [x] dedicated invalidator connection per upstream
- [x] handle cache set errors
- [x] allow cache ttl to be configurable
- [x] allow cache bounds to be configurable
- [x] have the invalidator itself re-send the CLIENT TRACKING command when it dies
- [ ] support for partial cache responses within `MGET`s. need to implement response value parsing to get this.
- [ ] implement `OPTIN` caching (https://redis.io/topics/client-side-caching#opt-in-caching)? requires client patch


- python client patch for pipeline signals
- make sure cache works for pipelined gets
- add a heartbeat to the invalidator run goroutine so that if it catastrophically dies the owner can kill the cache

## Invalidator

Happy path: before the upstream proxy struct is returned to the caller, an `Invalidator` is created and connected. It
has a heartbeat goroutine that sends a `PING` upstream every 5 seconds. It constructs a `CLIENT TRACKING` command for
the "main" proxy upstream connection to send. This command references the server side connection id of the invalidator's
connection.

Failure modes:
- The invalidator's long-lived connection dies. When it reconnects, it will have a new connection id. How does the
  upstream issue a new `CLIENT TRACKING` command to make sure that invalidation events are getting broadcast to the new
  invalidator connection?
  A: we could have the `Proxy` store the connection id of the invalidator locally, and each time it processes a message
  it asks the invalidator if this connection id matches the one it currently has. If not, issue a new `CLIENT TRACKING`
  command. this connection id should change very infrequently.

# Old notes from January:

- Add a new url param on upstreams called `cache_prefixes=list,of,prefixes` 
    - can use special value `__all__`? not sure if we need this
- Before creating the first "normal" upstream connection on one that has `cache_prefixes`, create a dedicated
  invalidation channel and save its ID
    - loop on reading messages, processing each invalidation against the local cache
- Then on each subsequent connection, send `CLIENT TRACKING on BCAST PREFIX <list> REDIRECT <ID>` where `<ID>` is the
  client id the invalidation channel
- !!! Any time the invalidation channel disconnects or goes bad, cache must be flushed
- Redisbetween must parse keys out of commands and selectively return values for ones that are found in the cache
    - can gradually support more as needed, starting with just simple GET
- When issuing supported commands like GET, first check in mem cache for the key, then do a full round trip
    - if multiple keys are supported, we will have to parse response values as well. can probably leverage an existing
      redis package for that. radix would work, but should fix the "double decode" situation now with the codis Encoder
      as combined with radix, since radix has its own decoder
- Use a RWLock around the cache on each value
- Local cache _must_ have overall mem bounds and upper-bound TTLs (10 minutes to start? with random jitter?)
- IDEA: redisbetween could proactively fetch and store values as they are invalidated? would make sense for kill
  switches, rates and i18n maybe
