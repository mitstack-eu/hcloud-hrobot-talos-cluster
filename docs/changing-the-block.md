# Changing the block

This page is for anyone who changes hcloud-hrobot-talos-cluster or studies it to
build their own block. It says how to run the tests, what each test proves and
what every file in the repository holds. Words such as block, operator and
manifest are defined in
[the workspace glossary](https://github.com/mitstack-eu/mitstack-workspace/blob/main/docs/glossary.md)
and on [Params, connections and outputs](reference.md#words-used-on-this-page).

## How to run the tests

The tests run every verb against an in-memory Hetzner Cloud project, an
in-memory Robot account, a GitHub fake and a fake of the platform's node
reports, in `harness_test.go`. Its `engine` plays the platform's settle check,
sending signed requests to the block's own handler. The worker's install runs
without a rescue system in these tests, on a schedule the test sets. The script
tests in `robotworker/` run each embedded script with bash against fake
commands. Run the suites from the repository root:

```bash
go vet ./...
go test ./...
shellcheck robotworker/scripts/*.sh
```

The control plane's own tests, such as the user data, the builder's scripts and
the Hetzner client, live in talos-cluster-kit's `controlplane` package.

| Test file | What it proves |
|---|---|
| `subscribe_test.go` | One subscribe reaches the control plane and the worker through the engine's re-drives. The image server waits for the gateway to hold its address. Every refusal comes before any change. A lost or failed install is refused and observed so. A written worker's lost Robot resource is put back, and a cancelled vSwitch is refused. The cluster reads unknown until the newest node report reads every node and the worker Ready. The enrolment outputs and the issuer outputs appear as `reference.md` describes. The Robot resources are read by the subscription's name |
| `redrive_test.go` | No Cloud node is created before the gateway's mirror is complete, even when it completes a minute before the build deadline. Each call waits on at most one Hetzner action and creates at most one node, validate asks for a re-drive exactly when a subscribe has a step to take, a re-drive sent twice makes every resource once, and a re-driven subscribe changes nothing made before |
| `hosting_test.go` | A cluster that hosts mitstack gets a write and a read-only deploy key whose public halves match the private halves it opens, sealed. An install replaces the keys an earlier one left under its titles and keeps any other. The repository reads present only with both keys at their access. The four recovery outputs open with the recipient key, and the cluster reads unknown until OpenBao's init file is committed. No private half appears unsealed in an answer, a log, the gateway or builder user data, or a repository file, and a recipient key that cannot be encrypted to is refused before any change |
| `robot_limits_test.go` | A 105-minute settle, called every nine seconds, reads the dedicated server at most 120 times in any hour and every other Robot request under its documented limit. A Robot read refused with `403 RATE_LIMIT_EXCEEDED` makes subscribe answer in progress and observe answer unknown, both naming the limit, with no Robot write |
| `teardown_test.go` | Unsubscribe deletes every resource the snapshot included and releases the Robot server, stops a running install, keeps a repository without the mark, and without a connection deletes what it can reach and refuses naming what it left |
| `manifest_test.go` | The manifest provides `hetzner.kubernetes-cluster`, declares the Cloud control plane and the worker's three Robot resources, fits the platform's description bounds and settle budget, and every declared resource has an observer |
| `declared_resources_test.go`, `secrets_leak_test.go` | block-kit's block guards, and that no token or Robot login reaches an answer or the logs |
| `robotworker/*_test.go` | A Robot worker's install in each of the three locations, the refusal of any other, the image server's build script, and that no key crosses the rescue session, as [How a Robot worker is installed](robot-worker-install.md#what-the-tests-prove) lists |

## Files every block starts with

Every new block starts from the same files, which mitstack's scaffold, its
generator of new block repositories, writes. They come from the templates in
the mitstack workspace's `platform/api/internal/scaffold/blockkitv1/`, plus
`go.mod` and `.github/workflows/block-verify.yml`. This block has most of
them, and adds its own files after.

| File | What it holds in this block |
|---|---|
| `manifest.yaml` | What the block is, the resources it builds, the type it provides and the requests it answers |
| `main.go` | Starts the operator's web service: `/hook` takes mitstack's requests, `/logs` serves the operator's recent log lines and `/healthz` answers a health check. It reads `MITSTACK_JWKS_URL`, the address where mitstack publishes its signing keys, to check each request |
| `resource.go` | Reads the params, the Hetzner token and Robot login and the operator's `MITSTACK_EGRESS_IPV4`, refuses what it cannot act on, and takes each subscribe stage through the kit's `Plane` |
| `Dockerfile` | Builds the operator's container image |
| `go.mod` | The Go module, and the versions of the modules it uses, among them block-kit and talos-cluster-kit |
| `go.sum` | The checksums of those modules |
| `secrets_leak_test.go` | Proves that no token reaches an answer or the logs |
| `teardown_test.go` | Proves that unsubscribe deletes every resource, the snapshot included |
| `.github/` | `workflows/ci.yml`, which this block has in place of the scaffold's `block-verify.yml`. It runs block-kit's publish workflow, which tests the block and publishes its image on a push to `main`, and runs gitleaks and shellcheck |

The scaffold's `resource_test.go`, `verbsemantics_test.go` and
`types/snapshot.json` are not in this repository. Its copy of the platform's
type snapshot sits at `testdata/type-snapshot.json` instead.

## Files this block adds

| File | What it does |
|---|---|
| `README.md` | The front page of the repository |
| `docs/` | These pages, the detail behind the README |
| `LICENSE` | The Business Source License 1.1, whose grant allows production use only for infrastructure the mitstack platform manages |
| `identity.go` | Names the Cloud control plane the block drives, and names the Robot server and vSwitch after the subscription |
| `worker.go` | The Robot worker's install in the background, its progress labels on the Cloud network, its refusals and repairs, and the image server's placement |
| `state.go` | Reports the resources a subscribe created, the Cloud ones and the worker's Robot ones |
| `repository.go` | Creates, reads and deletes the cluster repository on GitHub |
| `observe.go` | The observers, which read each resource from Hetzner and GitHub and report it present, absent or not yet known. They also set the outputs and tell mitstack when to send the next subscribe |
| `nodereports.go` | Reads the cluster's node reports from mitstack, and judges them against the Cloud nodes and the worker |
| `hosting.go` | A cluster that hosts mitstack: reads the recipient key, creates and seals the two deploy keys, judges them on the repository, and reads the recovery outputs |
| `teardown.go` | Runs the unsubscribe |
| `robotworker/` | Writes an adopted Robot server's Talos disk from its rescue system, with the scripts the rescue system and the image server run in `robotworker/scripts/` |
| `harness_test.go` | The shared test fakes: Hetzner Cloud and Robot in memory, GitHub, the node reports, and an engine that plays mitstack |
| `subscribe_test.go` | Proves what a subscribe builds, every refusal and the outputs |
| `redrive_test.go` | Proves that one subscribe reaches the cluster through mitstack's repeated requests |
| `manifest_test.go` | Proves the manifest's type, resources, description and settle budget, and that every declared resource has an observer |
| `declared_resources_test.go` | Runs block-kit's block guards |
| `testdata/` | `type-snapshot.json`, the platform's resource types the tests read |
