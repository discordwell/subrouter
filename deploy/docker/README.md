# Docker

The image runs as UID 65532 by default on a read-only root filesystem. The Compose launcher uses the invoking non-root UID so private host secret and state files stay readable without broader permissions. Compose binds the proxy to loopback, caps it at 256 MiB and 256 processes, limits the Go heap to 192 MiB for native and WebSocket buffers, drops Linux capabilities, and mounts control credentials as files under `/run/secrets`.

The standalone image listens on the container interface and requires proxy, admin, and account-import secrets at `/run/secrets`. It exits before listening when any secret is absent, so a published port cannot expose an unauthenticated control plane. The Compose profiles mount the required secrets and publish only to host loopback by default.

Create local control credentials, then start local-account mode:

```bash
./deploy/docker/init-secrets.sh
./deploy/docker/compose.sh local up --build -d
```

Import Codex or Claude credentials through the authenticated `GET` and `POST /_subrouter/account-import` flow. The container never needs SSH access or a host credential directory mount.

Team mode reads an existing cmux.com team login from one read-only secret. Copy the config without printing it:

```bash
./deploy/docker/init-secrets.sh
install -m 0600 ~/.config/subrouter/cloud.json deploy/docker/secrets/team-cloud.json
./deploy/docker/compose.sh team up --build -d
```

The team profile selects team credential storage and the hosted cmux.com API in
memory. This lets it consume a workstation config that still names a loopback
development API or legacy routing mode without modifying the read-only secret.
Set `SUBROUTER_CLOUD_BASE_URL` on the Compose command only when testing another
cmux.com API deployment.

Run one profile at a time because both publish the same loopback port. Use `compose.sh` so the container UID matches the owner of the `0600` secret files and `0700` state directories. To bind another private interface, set `SUBROUTER_BIND_IP` for the launcher. Do not bind a public address without a network firewall.

## Test runner caches

`./deploy/docker/test.sh`, which `make docker-test` runs, keeps two Docker volumes between runs. `subrouter-test-gomod` holds the downloaded modules, and `subrouter-test-gobuild` holds Go's build cache. Go deletes only build cache entries that no build used for five days. On a busy machine the build cache volume grew to 17 GB in three days.

Before each run, the runner checks the size of the build cache. When it holds more than `SUBROUTER_TEST_CACHE_LIMIT_GB` (default 6), the runner deletes the entries that no build used for a day, and prints one line that says how much it freed. When every entry may have been used within the day, the line says that nothing was freed. At or under the limit, it deletes nothing and prints nothing. Set the variable to `0` to turn the trim off, or to another number of GB, such as `0.5` or `20`:

```bash
SUBROUTER_TEST_CACHE_LIMIT_GB=10 ./deploy/docker/test.sh ./cmd/subrouter
```

Several runs at the same time are safe. Go updates the time of an entry that it uses only once that time is an hour old, so the trim waits a day and an hour, as Go's own trim adds the same hour. It never deletes an entry used within the day, or one that a running build uses. When two trims delete the same entry, neither fails. The trim never touches the module volume or the cache's `README` and `trim.txt`. `ops/bin/sr-local-ci --docker` applies the same limit to its own build cache volume, `sr-local-ci-gocache`.

To clear a volume by hand, remove it while no test run uses it. Docker refuses to remove a volume that a container uses. The next run creates the volume again, empty:

```bash
docker volume rm subrouter-test-gobuild   # the build cache: the next run compiles everything again
docker volume rm subrouter-test-gomod     # the modules: needed after go.mod gains a dependency
```
