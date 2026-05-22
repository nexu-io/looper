# loopernet deployment

`loopernet` is the Routed-mode Network control-plane service. It hosts exactly one Network, stores membership + lease + audit state in a local SQLite file, receives centralized GitHub webhooks, and fans wakeup events out to connected Nodes.

## Current deployment model

Today, deploy `loopernet` as a **single active instance per Network**.

- One `loopernet` hosts one Network.
- Do **not** run multiple active `loopernet` replicas for the same Network.
- Do **not** share one SQLite file across multiple `loopernet` containers.

Why:

- the Coordinator lease lives in the `loopernet` database;
- membership state lives in the same database;
- webhook/event fanout is currently in-process, not backed by shared pub/sub.

Different environments may each run their own `loopernet` instance, for example one for staging and one for production.

## Image

Release workflow publishes the container image to GHCR:

```bash
docker pull ghcr.io/nexu-io/loopernet:v0.x.y
```

Stable releases also publish `latest`.

## Required environment variables

`loopernet` reads configuration from environment variables.

Required:

- `LOOPERNET_DB_PATH` — path to the SQLite database file
- `LOOPERNET_ADMIN_TOKEN` — bearer token used by Nodes and admin operations

Optional:

- `LOOPERNET_LISTEN_ADDR` — listen address, default `127.0.0.1:8089`
- `LOOPERNET_NETWORK_ID` — fixed Network identifier; if omitted, `loopernet` creates one and persists it in the DB
- `LOOPERNET_PROTOCOL_VERSION` — defaults to the current protocol version
- `LOOPERNET_MIN_DAEMON_VERSION` — optional minimum `looperd` version gate
- `LOOPERNET_ADVERTISE_URL` — optional externally reachable base URL to surface to clients

## Persistent storage

`loopernet` needs persistent local storage for its SQLite database.

Minimum requirement:

- mount a writable volume at `/var/lib/loopernet`
- keep `LOOPERNET_DB_PATH=/var/lib/loopernet/loopernet.sqlite`

If the volume is lost, GitHub work-intent state remains authoritative, but `loopernet` loses:

- membership state
- lease state
- webhook/audit history
- persisted Network identity if you did not pin `LOOPERNET_NETWORK_ID`

Recovery is then:

1. redeploy `loopernet`
2. restore or recreate the DB volume
3. re-onboard repos if webhook secrets were lost
4. re-join Nodes

## Ports and networking

By default the container listens on port `8089`.

Expose it behind a stable HTTPS endpoint if Nodes or GitHub webhooks reach it over the network. A reverse proxy or load balancer in front of a **single** `loopernet` instance is fine.

## Docker example

```bash
docker run -d \
  --name loopernet \
  -p 8089:8089 \
  -e LOOPERNET_LISTEN_ADDR=0.0.0.0:8089 \
  -e LOOPERNET_DB_PATH=/var/lib/loopernet/loopernet.sqlite \
  -e LOOPERNET_ADMIN_TOKEN="replace-me" \
  -e LOOPERNET_MIN_DAEMON_VERSION="0.2.0" \
  -v loopernet-data:/var/lib/loopernet \
  ghcr.io/nexu-io/loopernet:v0.x.y
```

## Docker Compose example

The repository also includes ready-to-edit deploy assets at [`../deploy/loopernet/`](../deploy/loopernet/) including [`docker-compose.yml`](../deploy/loopernet/docker-compose.yml).

```yaml
services:
  loopernet:
    image: ghcr.io/nexu-io/loopernet:v0.x.y
    restart: unless-stopped
    ports:
      - "8089:8089"
    environment:
      LOOPERNET_LISTEN_ADDR: 0.0.0.0:8089
      LOOPERNET_DB_PATH: /var/lib/loopernet/loopernet.sqlite
      LOOPERNET_ADMIN_TOKEN: replace-me
      LOOPERNET_MIN_DAEMON_VERSION: 0.2.0
    volumes:
      - loopernet-data:/var/lib/loopernet

volumes:
  loopernet-data:
```

## Routed rollout checklist

After `loopernet` is up:

1. choose a stable public URL for `loopernet`
2. join each Node with `looper network join <url> --key ... --name <node>`
3. disable unsupported routed Planner/Fixer auto-discovery before opting projects into `network.mode=routed`
4. onboard repos so GitHub webhooks point to `loopernet`
5. confirm `looper network status --verbose` shows membership, identity, lease, and webhook health

## Availability expectations

Current recommendation is **single-instance, restartable, persistent-volume-backed** deployment.

This is not yet an HA multi-replica service. Running multiple active replicas for one Network risks split-brain or partial-brain behavior because webhook/event delivery and coordination are not backed by shared distributed state.
