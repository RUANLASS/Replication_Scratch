# Replication_Scratch
A basic, from-scratch implementation of a leader-follower database replication in Go.  It is built while working through some concepts in chapters 3, 4, 5 of the book "Designing Data Intensive Applications". 

---

## What this implements

| Component | What it does |
|---|---|
| **Write-Ahead Log** | Append-only, crash-safe log. Every write is durable before it touches memory. |
| **Leader node** | Accepts writes, serves reads from memory, ships WAL entries to followers. |
| **Follower node** | Pulls WAL entries from the leader, applies them locally, serves read-only traffic. |
| **Router** | Single entry point for clients. Enforces read-your-writes consistency via session tokens. |
| **Health checker** | Polls the leader. On repeated failure, promotes the most up-to-date follower. |

---

## Architecture

```
                        ┌─────────────────┐
                        │     Router      │  :8082
                        │  (session       │
         clients ──────▶│   tokens +      │
                        │   failover)     │
                        └────────┬────────┘
                                 │
               ┌─────────────────┼─────────────────┐
               │ writes          │ reads            │
               ▼                 ▼                  │
     ┌──────────────┐   ┌──────────────┐            │
     │    Leader    │   │   Follower   │            │
     │    :8080     │──▶│    :8081     │            │
     │              │   │              │            │
     │  WAL on disk │   │  WAL on disk │            │
     └──────────────┘   └──────────────┘            │
                                                     │
                        ┌────────────────┐           │
                        │ Health Checker │───────────┘
                        │ (embedded in   │
                        │  router)       │
                        └────────────────┘
```

The router tracks a write token per client. After a write, the token contains the WAL offset of that write. On subsequent reads, the router checks whether any follower has caught up to that offset — if yes, it routes to the follower; if no, it falls back to the leader. This is read-your-writes consistency without pinning clients permanently to the leader.

---

## Running the system

You need Go 1.22+ (for native HTTP path parameters).

**Clone and build:**
```bash
git clone https://github.com/your-username/replication-lab
cd replication-lab
go build ./...
```

**Open four terminals:**

```bash
# Terminal 1 — leader
go run ./cmd/ -role=leader -port=8080

# Terminal 2 — follower
go run ./cmd/ -role=follower -port=8081 -leader=http://localhost:8080

# Terminal 3 — router (health checker runs embedded)
go run ./cmd/ -role=router -port=8082 \
  -leader=http://localhost:8080 \
  -followers=http://localhost:8081

# Terminal 4 — send requests
curl -c cookies.txt -X PUT http://localhost:8082/keys/user:1 \
  -H "Content-Type: application/json" \
  -d '{"value": "alice"}'

curl -b cookies.txt http://localhost:8082/keys/user:1
# {"key":"user:1","value":"alice"}
```

**Observe replication lag:**
```bash
curl http://localhost:8081/status
# {"applied_offset":0,"leader_url":"http://localhost:8080"}
```

**Trigger failover:**
```bash
# Kill the leader (Ctrl+C in Terminal 1)
# Wait ~15 seconds for the health checker threshold

curl http://localhost:8082/cluster/status
# leader_url will now point at the follower

# Writes still work
curl -c cookies.txt -X PUT http://localhost:8082/keys/user:2 \
  -H "Content-Type: application/json" \
  -d '{"value": "bob"}'
```

---

## API reference

### Leader / promoted follower

| Method | Path | Body | Description |
|---|---|---|---|
| `PUT` | `/keys/{key}` | `{"value": "..."}` | Write a key |
| `GET` | `/keys/{key}` | — | Read a key |
| `DELETE` | `/keys/{key}` | — | Delete a key |
| `GET` | `/health` | — | Health check |
| `GET` | `/wal?from={offset}` | — | Stream WAL entries from offset |

### Follower

| Method | Path | Description |
|---|---|---|
| `GET` | `/keys/{key}` | Read a key (read-only until promoted) |
| `GET` | `/status` | Returns `applied_offset` and `leader_url` |
| `POST` | `/promote` | Promote this follower to leader |

### Router

| Method | Path | Description |
|---|---|---|
| `PUT` | `/keys/{key}` | Forwarded to leader, sets session token |
| `GET` | `/keys/{key}` | Routed based on session token and follower lag |
| `DELETE` | `/keys/{key}` | Forwarded to leader |
| `GET` | `/cluster/status` | Returns current leader, followers, active tokens |

---

## Key design decisions

**Pull-based replication over push.** The follower polls the leader's `/wal` endpoint on a fixed interval rather than the leader pushing changes out. This is simpler to implement and mirrors how MySQL binlog replication works. The tradeoff is that replication lag is at minimum one polling interval — you can observe this directly by writing to the leader and immediately querying the follower before the next poll fires.

**JSON encoding in the WAL.** Each WAL entry is a newline-delimited JSON object. This makes the log human-readable and trivial to debug — you can open `leader.log` in any text editor and see exactly what happened. A production system would use a binary format (Protocol Buffers or Avro) for compactness and speed. The encoding chapter of DDIA covers exactly this tradeoff.

**Session token TTL.** Read-your-writes consistency is enforced via a write token stored in a client cookie. The token contains the WAL offset of the client's last write. On reads, the router finds a follower whose `applied_offset` is >= that offset, or falls back to the leader. Tokens expire after 30 seconds by default — long enough for replication to catch up under normal conditions, short enough that clients are not permanently pinned to the leader after a single write from days ago.

**Failure detection threshold.** The health checker requires three consecutive failed health checks before declaring the leader dead and initiating promotion. A single failed check could be a transient network hiccup. The tradeoff is detection latency: at a 5-second check interval with a threshold of 3, failover takes up to 15 seconds. Lowering the threshold risks false positives — promoting a follower when the leader is merely slow, which causes split-brain.

**Split-brain is not prevented, only demonstrated.** If the old leader comes back after a follower is promoted, two nodes will accept writes simultaneously and their WALs will diverge. The test `TestSplitBrainObservation` demonstrates this explicitly — you can see two nodes with different data at the same logical point in time. Preventing this requires fencing tokens or a consensus protocol like Raft. 

---

## Running the tests

```bash
# All packages
go test ./...

# Specific package with verbose output
go test -v ./wal/...
go test -v ./leader/...
go test -v ./follower/...
go test -v ./router/...
go test -v ./healthcheck/...

# The split-brain observation test (logs diverged state)
go test -v ./healthcheck/... -run TestSplitBrainObservation
```


---

## Project structure

```
.
├── cmd
│   └── main.go
├── cookies.txt
├── follower
│   ├── server_test.go
│   └── server.go
├── follower.log
├── go.mod
├── healthcheck
│   ├── checker_test.go
│   └── checker.go
├── leader
│   ├── server_test.go
│   └── server.go
├── leader.log
├── README.md
├── router
│   ├── router_test.go
│   └── router.go
└── wal
    ├── wal_test.go
    └── wal.go

7 directories, 16 files

```

---
