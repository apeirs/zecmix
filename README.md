# zecmix: Zcash light-wallet traffic over the Nym mixnet

`zecmix` hides your IP address from the lightwalletd server your Zcash wallet
syncs from and broadcasts through. It has two parts:

- **a local lightwalletd gateway**: `zecmix gateway` listens on `127.0.0.1:9067`
  and forwards every gRPC call from your wallet to a public lightwalletd server
  (default `zec.rocks:443`) **through the Nym mixnet**. Any wallet or tool that
  accepts a custom lightwalletd server can use it, with no code changes to the
  wallet. Tested live with `zcash-devtool`.
- **a CLI that speaks lightwalletd over the mixnet directly**: `info`, `tip`,
  `blocks` (streams compact blocks), `treestate`, `tx`, and `send` (raw
  transaction broadcast), plus `ip` to show which address the servers see.

It is a single static Go binary. Since 0.2.0 it reaches the mixnet through
[nymdial](https://github.com/apeirs/nymdial), a small Rust binary on Nym's own
`nym-sdk` and `nym-smolmix`: each upstream connection is a fresh, short-lived
mixnet client inside a `nymdial` process. There is no `nym-socks5-client` to
install, init and keep running, and no local SOCKS port. zecmix does not
reimplement any mixnet cryptography: Sphinx packets, cover traffic, Poisson
delays and gateway handling all come from Nym's code. The old
`nym-socks5-client` path is still there behind `--transport socks5`.

By apeir (https://apeir.dk/online/).

---

## The problem

A Zcash light wallet hides amounts, addresses and memos on chain. The network
layer is a separate matter. To sync, the wallet connects to a lightwalletd
server (or a Zaino indexer) and:

- downloads compact blocks from its **birthday height**, which roughly tells
  the server when the wallet was created;
- asks for tree states, transactions and, for transparent addresses, UTXOs and
  history by address;
- **broadcasts its own transactions** with `SendTransaction`.

All of this is tied to the client's IP address. A server operator, or anyone
who can watch or subpoena it, learns "this IP sent transaction X" and "this
IP's wallet is about this old and syncs at these times". For transparent
address queries it learns "this IP owns these addresses". Shielded crypto
cannot fix that, because it is metadata about the connection.

ZecHub's [issue #2219](https://github.com/ZecHub/zechub/issues/2219)
("Routing Zcash Wallet Traffic Over the Nym Mixnet") proposes a guide on
system-level setup for exactly this gap. `zecmix` is a tool for the job: a
single command that puts a wallet's lightwalletd traffic on the mixnet without
a VPN, a system-wide proxy, or a server run by anyone.

## Design

```
 wallet (zcash-devtool, zingo-cli, your script)
   │  plaintext h2c gRPC (or TLS with a local cert), loopback only
   ▼
 zecmix gateway 127.0.0.1:9067 ── HTTP/2 reverse proxy, streams + trailers preserved
   │  TLS 1.3 to zec.rocks (verified against system roots, SNI zec.rocks, ALPN h2)
   ▼  ...over a socketpair to a child process
 nymdial connect zec.rocks 443 ── a fresh mixnet client + userspace TCP/IP (smoltcp),
   │                                DNS-over-HTTPS through the tunnel
   ▼  Sphinx packets, cover traffic; replies via SURBs
 entry gateway → 3 mix layers → exit gateway (IP Packet Router, Nym exit policy)
   ▼  TCP
 public lightwalletd (zec.rocks:443) ── sees the exit gateway's IP, not yours
```

- **End-to-end TLS.** The exit gateway relays ciphertext only. The server
  certificate is verified for the real hostname.
- **One mixnet client per upstream connection.** Every connection zecmix opens
  is a `nymdial connect HOST PORT --events` child process with one end of a
  socketpair as its stdin and stdout; zecmix keeps the other end as an ordinary
  `net.Conn` and runs TLS and HTTP/2 over it. nymdial starts a new ephemeral
  Nym client (new identity), picks an exit from the nym-api (IP Packet Router
  exits, performance-weighted random order), health-checks the tunnel, skips
  exits that don't answer, and reports the exit (node id, country, IPs) as a
  JSON line, which zecmix logs.
- **One connection per gateway session.** All of a wallet's calls are
  multiplexed as HTTP/2 streams over a single TLS connection, so one nymdial
  process carries a whole sync. You pay the setup cost once, and a sync does
  not open hundreds of streams. If the connection dies, the next call opens a
  new one (a new client and exit).
- **No local DNS.** The server name is passed to nymdial unresolved, and
  nymdial resolves it with DNS-over-HTTPS through its tunnel (Quad9, Mullvad,
  Cloudflare at pinned IPs). The gRPC client uses the `passthrough` resolver on
  purpose, because gRPC's default `dns` resolver would look the name up
  locally. (On `--transport socks5`, `--dns doh` does DoH through the SOCKS
  stream and `--dns exit` hands the name to the exit.)
- **No clearnet fallback.** If nymdial is missing or no exit works (or, on
  socks5, the client isn't running), the CLI refuses to connect, and the
  gateway answers every call with gRPC `UNAVAILABLE`. It never goes direct.
- **No identifying headers.** The gateway sets `:authority` to the upstream
  host, never adds `X-Forwarded-For`/`Forwarded`/`Via`, strips them if a
  client sends them, and replaces the wallet's User-Agent with a generic one.
- **Loopback only.** The gateway refuses a non-loopback listen address unless
  you pass `--allow-remote`.

### How Nym is used

- **nymdial (default):** `nym-sdk` 1.21.6 + `nym-smolmix` 1.21.6. Each process
  is an ephemeral `MixnetClient` with an `IpMixStream` to the exit's IP Packet
  Router, and smoltcp on top. The IPR answers through reply SURBs, so it never
  learns the client's Nym address. The exit list comes from the nym-api
  (`nymdial exits` shows it); `--country` and `--exit` narrow it.

On `--transport socks5`:

- `zecmix nym setup` asks the Nym API for active exit gateways that run a
  network requester with the exit policy and perform at 0.95 or better. It
  picks one **at random**, runs `nym-socks5-client init` with **reply SURBs
  on** (so the exit never learns the client's mixnet address), and starts the
  client.
- lightwalletd ports are allowed by the Nym exit policy: 443 (HTTPS), 9067
  (in the 8081-9080 range), and 8232-8233 (listed as Zcash).
- `nym-socks5-client` 1.1.86 (release v2026.19) writes a config at `init`
  that its own `run` cannot load (`missing field credential_requests_database`).
  zecmix patches it automatically. If you run the client by hand, add
  `credential_requests_database = '<data dir>/credential_requests.db'` under
  `credentials_database`.

## Install

Linux or macOS (the nymdial transport uses a Unix socketpair).

```sh
# 1. zecmix (Go 1.26+)
go install github.com/apeirs/zecmix/cmd/zecmix@latest

# 2. nymdial (Rust 1.85+); zecmix finds it on PATH, or --nymdial PATH / $ZECMIX_NYMDIAL
cargo install --git https://github.com/apeirs/nymdial

# 3. Check: which IP does a server see? It should be the exit nymdial used.
zecmix ip
```

Only for `--transport socks5`: download `nym-socks5-client` from
https://github.com/nymtech/nym/releases (nym-binaries-*), check its sha256
against the release's hashes.json, `chmod +x`, set
`ZECMIX_NYM_SOCKS5=/path/to/nym-socks5-client` and run `zecmix nym setup`.

## Usage

### Gateway for wallets

```sh
zecmix gateway -v                 # http://127.0.0.1:9067 -> mixnet -> zec.rocks:443
zecmix gateway --server na.zec.rocks:443
zecmix gateway --tls --listen 127.0.0.1:9443   # https://localhost:9443, self-signed cert
```

Then point the wallet at the gateway:

```sh
# zcash-devtool (treats localhost as plaintext)
zcash-devtool wallet -w ./w init --name demo -i ./w.age -n main -s 127.0.0.1:9067
zcash-devtool wallet -w ./w sync -s 127.0.0.1:9067

# zingo-cli (not tested here; its docs say a scheme-less --server becomes http://)
zingo-cli --server 127.0.0.1:9067 --data-dir ./zw

# zecmix itself, through the gateway
zecmix info --server http://127.0.0.1:9067
```

On startup the gateway does a self-check: one `GetLightdInfo` through itself,
and so through the mixnet. It then logs one line per proxied call: method,
gRPC status, bytes and time.

### Direct CLI

```sh
zecmix info                       # GetLightdInfo
zecmix tip                        # GetLatestBlock
zecmix blocks tip-9 tip           # GetBlockRange: stream + check the prev-hash chain
zecmix treestate 3513720          # GetTreeState (or `latest`)
zecmix tx <txid> [--raw]          # GetTransaction
zecmix send tx.hex                # SendTransaction (hex string, file, or - for stdin)
zecmix ip                         # what IP do servers see? checked against the exit nymdial used and the nym-api node list
zecmix servers                    # known public servers
nymdial exits                     # the exits nymdial picks from
zecmix nym exits | status | stop  # socks5 only
```

Common flags: `--server host:port`, `-v` (trace every mixnet step, the exit and
nymdial's own log), `--json`, `--timeout 3m`. nymdial: `--nymdial PATH`,
`--country CC` and `--exit NODE` (repeatable, passed to nymdial),
`--setup-timeout 3m` (per connection, exit failover included). socks5:
`--transport socks5`, `--proxy 127.0.0.1:1080`, `--dns doh|exit`. Environment
variables: `ZECMIX_SERVER`, `ZECMIX_TRANSPORT`, `ZECMIX_NYMDIAL`,
`ZECMIX_PROXY`, `ZECMIX_DNS`, `ZECMIX_NYM_SOCKS5`.

`send` checks the transaction header locally first (hex, version, version group
id), so a copy-paste mistake never reaches the network. It exits 3 when the
server rejects the transaction.

## Live run with nymdial (Nym mainnet, 2026-11-08)

Ten runs. Each run starts `zecmix gateway`, waits for its self-check, then
points zecmix's own Go gRPC client at the gateway as a wallet would:
`info` (GetLightdInfo), `tip` (GetLatestBlock), `blocks tip-99 tip` (a
100-block compact-block sync, prev-hash chain checked) and `send` of an
already-mined mainnet transaction (the node's rejection must come back). Then a
separate `zecmix ip`. Upstream: `zec.rocks:443` (lightwalletd v0.6.1 on Zebra 7).

```
$ zecmix gateway -v       (run 1, excerpt)
[   7.02s] nym  zec.rocks:443 open via exit node 2681 (CH) (fresh mixnet client; tunnel 5.54s, DNS+TCP 1.47s, 7.02s in all)
[   9.18s] tls  zec.rocks TLS 1.3 alpn="h2" verified for zec.rocks (2.17s)
[  10.54s] self-check ok: upstream ECC LightWalletD v0.6.1, chain=main height=3551978 (10.54s through the gateway)
[  12.46s] call #3 .../GetLatestBlock -> zec.rocks:443 via mixnet: grpc-status=0, 44 bytes, 1.02s
[  16.43s] call #5 .../GetBlockRange -> zec.rocks:443 via mixnet: grpc-status=0, 38141 bytes, 2.43s
[  20.80s] call #6 .../SendTransaction -> zec.rocks:443 via mixnet: grpc-status=0, 97 bytes, 4.36s
$ zecmix send examples/already-mined-tx.hex --server http://127.0.0.1:9067
REJECTED  ...  code -25  failed to validate tx: WtxId("private"), error: transaction is already in state    (via Nym mixnet)
```

| step (10 runs) | ok | wall time: median (min, max) |
|---|---|---|
| gateway up + self-check (new client, exit, TLS, first call) | 10/10 | 11.3 s (9.5, 62.0) |
| GetLightdInfo through the gateway | 10/10 | 1.2 s (0.4, 3.6) |
| GetLatestBlock | 10/10 | 1.2 s (1.0, 3.9) |
| 100-block sync (about 37 KB of compact blocks) | 10/10 | 4.1 s (3.6, 10.5) |
| SendTransaction, rejection came back | 10/10 | 4.5 s (3.4, 7.6) |
| `zecmix ip` (server-seen IP is the exit's) | 8/10 | 12.8 s (10.2, 15.1) |

Each gateway run used one upstream connection, so one nymdial process carried
the whole session (10 different exits over the 10 runs). The two `ip` failures
were HTTP 522 answers from api.ipify.org (Cloudflare could not reach its
origin); the tunnel itself worked.

A second series later that afternoon, with an experimental nymdial build
(nym-sdk patched to skip lost reply frames) and two mixpool series running on
the same machine, did worse (6/10 block syncs, 7/10 self-checks): one block
stream stalled after 2 blocks and the gateway reconnected for the next call,
one DoH lookup failed at every resolver, and one run lost every exit it tried.
Those logs are kept with the first ones; read the first table as a good hour,
not a guarantee.

## Earlier proof with nym-socks5-client (2026-10-11)

All output below is from a real run on 2026-10-11 against Zcash **mainnet**
through the **live Nym mixnet** (`nym-socks5-client` 1.1.86, release v2026.19).
Full logs: [`examples/live-run-cli.txt`](examples/live-run-cli.txt) and
[`examples/live-run-gateway-zcash-devtool.txt`](examples/live-run-gateway-zcash-devtool.txt).

**Servers see a Nym exit gateway, not me.** `zecmix nym setup` picked exit
node 3156 (CH) at random from 98 active exits. ipify, reached over the same
path, reports that node's IP:

```
$ zecmix ip -v
[   3.91s] dns  api.ipify.org -> [104.26.13.205 104.26.12.205 172.67.74.152] (DoH via 1.1.1.1, over the mixnet, 3.91s)
[   3.91s] dial api.ipify.org:443 -> socks5 127.0.0.1:1080 -> mixnet -> exit -> 104.26.13.205:443
https://api.ipify.org sees this request coming from 185.191.239.61 (8.30s over the mixnet)
= Nym node 3156 (CH, exit gateway: true) in the nym-api node list
```

**GetLightdInfo / GetLatestBlock, mainnet height:**

```
$ zecmix info -v
[   0.00s] lwd  server zec.rocks:443 over the Nym mixnet (socks5 127.0.0.1:1080, dns=doh); no clearnet fallback
[   3.11s] dns  zec.rocks -> [137.66.62.60] (DoH via 1.1.1.1, over the mixnet, 3.11s)
[   3.11s] dial zec.rocks:443 -> socks5 127.0.0.1:1080 -> mixnet -> exit -> 137.66.62.60:443
server            zec.rocks:443 (via Nym mixnet, 7.08s)
chain             main
block height      3513917 (estimated 3513918)
vendor            ECC LightWalletD
version           v0.5.3 (062e8bfc5bb47e42e3c26567cd6e093cfb854ed4)
consensus branch  37a5165b
$ zecmix tip
3513917  0000000000536e9a4fe3af7f4fb2a5f786525ee35a8e3dab3e4c4359c3581ada    (zec.rocks:443, via Nym mixnet, 5.35s)
```

**GetBlockRange, streaming compact blocks** (prev-hash chain checked; the
IRONW column is the new NU6.3 Ironwood pool):

```
$ zecmix blocks tip-9 tip
HEIGHT   HASH                                                             TIME (UTC)          TXS  T-IN T-OUT SAPOUT SAPSP  ORCH IRONW  BYTES
3513908  000000000039c21fddd8f98aed70d284fdd7605c2df546a5304495ff0e8a56ed 2026-10-11 09:19:09   1     0     0      2     1     0     0    418
...
3513917  0000000000536e9a4fe3af7f4fb2a5f786525ee35a8e3dab3e4c4359c3581ada 2026-10-11 09:27:36   3     0     0      0     0     0     7   1325
10 compact blocks 3513908..3513917, 6022 bytes, 8 sapling outputs, 0 orchard + 22 ironwood actions, prev-hash chain linked; 2.20s (zec.rocks:443, via Nym mixnet)
```

**SendTransaction: the server's rejection comes back through the mixnet.**
No coins move. First, a real mainnet transaction from block 3,513,720
(fetched with `zecmix tx --raw` over the mixnet) is re-broadcast. Then a
malformed one is sent:

```
$ zecmix send examples/already-mined-tx.hex -v
tx: v6 (overwintered, version group d884b698, consensus branch 37a5165b), 21790 bytes
REJECTED  zec.rocks:443  code -25  4.80s  failed to validate tx: WtxId("private"), error: transaction is already in state    (via Nym mixnet)

$ zecmix send examples/invalid-tx.hex
tx: v5 (overwintered, version group 26a7270a, consensus branch 00000000), 68 bytes
REJECTED  zec.rocks:443  code -22  7.24s  parse error: invalid consensus branch id    (via Nym mixnet)
```

**A real light wallet syncing through the gateway.** This is unmodified
`zcash-devtool` (built from zcash/zcash-devtool `main`, no Nym support of its
own), with a fresh zero-balance wallet:

```
$ zecmix gateway -v
[   0.00s] gateway listening on http://127.0.0.1:9067 -> socks5 127.0.0.1:1080 -> Nym mixnet -> exit gateway -> zec.rocks:443 (TLS)
[   5.52s] tls  zec.rocks TLS 1.3 alpn="h2" verified for zec.rocks (2.42s)
[   7.27s] self-check ok: upstream ECC LightWalletD v0.5.3, chain=main height=3513950 (7.27s through the gateway)
[  19.14s] call #2 .../GetLatestBlock -> zec.rocks:443 via mixnet: grpc-status=0, 44 bytes, 2.82s
[  20.26s] call #3 .../GetTreeState -> zec.rocks:443 via mixnet: grpc-status=0, 2925 bytes, 1.12s
[  26.75s] call #4 .../GetSubtreeRoots -> zec.rocks:443 via mixnet: grpc-status=0, 86888 bytes, 2.61s
[  28.69s] call #5 .../GetSubtreeRoots -> zec.rocks:443 via mixnet: grpc-status=0, 59345 bytes, 1.94s
[  29.41s] call #6 .../GetSubtreeRoots -> zec.rocks:443 via mixnet: grpc-status=0, 858 bytes, 0.73s
[  30.51s] call #7 .../GetLatestBlock -> zec.rocks:443 via mixnet: grpc-status=0, 44 bytes, 1.09s
[  31.54s] call #8 .../GetAddressUtxosStream -> zec.rocks:443 via mixnet: grpc-status=0, 0 bytes, 1.03s
[  33.33s] call #9 .../GetBlockRange -> zec.rocks:443 via mixnet: grpc-status=0, 78999 bytes, 1.79s
[  34.69s] call #10 .../GetTreeState -> zec.rocks:443 via mixnet: grpc-status=0, 2925 bytes, 1.36s

$ zcash-devtool wallet -w ./w init --name nymdemo -i ./w.age -n main -s 127.0.0.1:9067 --birthday 3513850
$ zcash-devtool wallet -w ./w sync -s 127.0.0.1:9067
INFO zcash_devtool::commands::wallet::sync: Sapling tree has 1128 subtrees
INFO zcash_devtool::commands::wallet::sync: Orchard tree has 770 subtrees
INFO zcash_devtool::commands::wallet::sync: Ironwood tree has 11 subtrees
INFO zcash_devtool::commands::wallet::sync: Latest block height is 3513950
INFO zcash_devtool::commands::wallet::sync: Fetching ChainTip(3513850..3513951)
INFO zcash_devtool::commands::wallet::sync: Scan complete for range ChainTip(3513850..3513951); progress is 76/76
$ zcash-devtool wallet -w ./w balance      # excerpt
    chain_tip_height: BlockHeight(3513950), fully_scanned_height: BlockHeight(3513950)
```

The wallet went from nothing to fully scanned at the chain tip (101 blocks
plus subtree roots and tree states) in about 11 seconds. Every byte went
through the mixnet. Also verified live: `--dns exit`, the regional servers
(`eu.`/`na.zec.rocks`), testnet (`testnet.zec.rocks`, chain `test`), the
`--tls` listener, and `ZECMIX_LIVE=1 go test -run Live`.

## Threat model and honest limits

**What it hides:** your IP address from the lightwalletd operator, from
anyone watching or compelling that server, and from network observers between
the exit and the server. It also hides the server name from your local DNS
resolver. Your ISP sees that you use Nym, but not which server you talk to or
what you send. The Nym exit sees a TLS connection to `zec.rocks`, but not its
contents and not your IP. Mixing, cover traffic and reply SURBs make
timing correlation much harder than with a plain proxy or VPN.

**What it does not hide:**

- **What the wallet asks for.** The server still sees your birthday height,
  sync pattern and transparent-address queries, and all calls in one gateway
  session share one connection. It can link them to each other, just not to
  you. To avoid linking sessions, restart the gateway: with nymdial that is a
  new mixnet identity and usually a new exit. A `zecmix send` from the CLI is
  its own connection and identity, separate from the gateway's sync.
- **Transaction contents.** The mixnet hides *who* broadcast a transaction,
  not what is on chain. Transparent inputs and outputs are public whatever
  route the transaction takes.
- **Exit and server collusion or a global adversary.** The exit knows *some*
  Nym user talks to zec.rocks. A party running both the exit and the server
  still cannot see your IP, but a global passive adversary is what Nym's
  timing defences are designed for, and those defences have limits.
- **Bootstrap metadata.** nymdial (and `zecmix nym setup` and `ip`) query the
  public Nym API directly for the exit list and topology, just as
  `nym-socks5-client` does. That reveals Nym use, nothing about Zcash.
- **Tunnel stalls (nym-sdk 1.21.6).** An IP Packet Router tunnel can stop
  receiving after one lost reply frame (nym-sdk's in-order `MixnetStream`; see
  the nymdial README). The gateway's HTTP/2 pings notice a dead upstream
  connection within about 75 s and the next call opens a new one; a call
  in flight on the stalled connection fails.
- **The local hop.** Wallet to gateway is plaintext on loopback by default.
  Anyone who can sniff your loopback can already read your wallet. Use
  `--tls` if a wallet insists on HTTPS.
- **Speed.** Expect seconds per call. A first call takes about 10 to 40 s
  (a new mixnet client, exit and health check, plus TLS; longer when nymdial
  has to skip exits), later calls about 1 to 2 s, and compact blocks stream
  at mixnet bandwidth. Fine for a wallet that is mostly
  synced. A multi-year rescan over the mixnet would be slow.
- **Free bandwidth.** nymdial's clients (and the socks5 client) currently run
  without zk-nym bandwidth credentials, which the network allows today. If
  that changes, credentials will be needed.

### Relation to other work

- [nymtech/nym-zcash-grpc-demo](https://github.com/nymtech/nym-zcash-grpc-demo)
  (2024) carried lightwalletd gRPC over the mixnet to a dedicated
  `NymProxyServer` that someone runs next to a lightwalletd instance. zecmix
  needs no extra server. It uses Nym's public exit gateways to reach existing
  public lightwalletd/Zaino servers, with TLS end to end.
- **Zingo** is adding a native "Mixnet Mode" (`nym-proxy` in zingolib's
  `zingo-netutils`). If you use Zingo, use that. zecmix is wallet-agnostic: it
  covers zcash-devtool, wallets built on other SDKs, scripts, and any
  lightwalletd client that can take a custom server URL. It also gives you a
  scriptable CLI for broadcasts and lookups.
- [mixpool](https://github.com/apeirs/mixpool), my earlier Nym build, does the
  same for Bitcoin transaction broadcast via Esplora APIs, and uses nymdial the
  same way.

## Tests

```sh
go test ./...                                  # offline, with fakes
ZECMIX_LIVE=1 go test -run Live -v .           # live: real mixnet + mainnet lightwalletd (nymdial on PATH)
```

The offline tests run a fake nymdial (the test binary itself, re-executed), a
fake SOCKS5 proxy that records every CONNECT, a fake DoH resolver and a fake
TLS lightwalletd. For the nymdial transport they check that:

- the hostname goes to nymdial unresolved, one process per connection, and
  nothing touches the SOCKS/DoH path;
- a wallet's info, block stream, error status and send rejection all work
  through the gateway over a single nymdial process, with no forwarding
  headers;
- the exit nymdial reports is recorded, and `--country`/`--exit` reach it;
- a missing nymdial never leads to a direct connection (`UNAVAILABLE` from the
  gateway, upstream never reached);
- setup errors name the failed step, and a hung setup is timed out (or
  cancelled with its context) and its process stopped;
- closing the connection ends the nymdial process.

For the socks5 transport they check that:

- names are resolved through the mixnet and connected by IPv4;
- `--dns exit` sends the name to the exit (SOCKS ATYP 3);
- there is no clearnet fallback when the proxy is down, from the CLI or the
  gateway (`UNAVAILABLE`, upstream never reached);
- exit-policy refusals surface as errors;
- block streaming and send rejections work end to end;
- the gateway preserves gRPC streams and trailer status codes, sends
  `:authority` = upstream, and adds no forwarding headers;
- plaintext is only allowed to loopback.

There are also unit tests for txid byte order, tx header checks (including a
real v6/NU6.3 header) and the nym config patch.

## Future plans

- `--split` mode: send `SendTransaction` over a separate connection and
  identity from the sync session, so the server cannot link a broadcast to the
  wallet that synced.
- Optional random delay before broadcast (mixpool has `--delay` now), and
  pacing/cover traffic for sync.
- Packaged builds (Linux/macOS/Windows), a systemd user unit, and a ZecHub
  guide that uses it.
- Zebra/zcashd P2P (port 8233) over Nym's IP packet router, for full-node
  users.

## License

MIT. `proto/*.proto` come from
[zcash/lightwalletd](https://github.com/zcash/lightwalletd) (MIT, The Zcash
developers), commit d16d481, with only `go_package` changed.

By apeir (https://apeir.dk/online/).
