# How a subscribe runs

This page explains what hcloud-hrobot-talos-cluster builds when an app subscribes,
the order it builds it in, and how an unsubscribe takes it down. It is for
anyone who wants to know what happens in their Hetzner Cloud project, their
Robot account and their GitHub account, or who studies the block to build their
own. Words such as subscribe, operator, resource and settle budget are defined in
[the workspace glossary](https://github.com/mitstack-eu/mitstack-workspace/blob/main/docs/glossary.md)
and on [Params, connections and outputs](reference.md).

## Why the operator takes one stage per request

mitstack gives each request two minutes, and a cluster takes up to 105 minutes
to build, which is the manifest's settle budget. The operator therefore builds
the cluster one stage per request, and mitstack sends a fresh request until the
cluster is ready. Every Cloud resource carries the subscription id as a label,
and each Robot resource carries it in its name, so each request finds the
cluster alone and needs no memory of the last one. The one exception is the
worker's install, which runs inside the operator process, as
[the worker's install](#how-the-worker-is-installed) explains.

The operator is a Go program built on block-kit,
`github.com/mitstack-eu/block-kit`. block-kit is mitstack's Go
library for blocks, and it checks each signed request and answers it.

The nodes run Talos Linux, an operating system that runs only Kubernetes and
is managed through an API, with no shell and no SSH. The Talos install comes
from [talos-cluster-kit](https://github.com/mitstack-eu/talos-cluster-kit), a
Go module of Talos settings and manifests that this block imports.
[ADR 0095](https://github.com/mitstack-eu/mitstack/blob/main/docs/adr/0095-mitstack-clusters-run-talos-across-fsn1-nbg1-and-hel1-with-a-load-balancer-per-location.md)
records the design.

## What a subscribe builds

A subscribe builds two parts: a Cloud control plane and a worker on a dedicated
server.

The Cloud control plane is three control-plane nodes, one in each of `fsn1`,
`nbg1` and `hel1`, behind a private network whose only way out is a gateway in
`fsn1`, with an API and an ingress load balancer in `fsn1`. Its code lives in
the `controlplane` package of talos-cluster-kit, and
[its README](https://github.com/mitstack-eu/talos-cluster-kit/blob/main/README.md#how-a-block-drives-the-cloud-control-plane)
describes the resources, the stages and the builder. Losing `fsn1` stops the
cluster's outbound and web traffic. The block gives the subscriber no location
to pick, because every cluster spans all three.

The worker is the dedicated server that `robotServerNumber` names. It runs the
app's pods, because the block sets `AppsOnWorkers` on the plane and the
control-plane nodes keep Talos's `NoSchedule` taint. Cilium, ztunnel, the CNI
agent and the log collector still run on every node. On a cluster that hosts
the platform, OpenBao's members run on the control-plane nodes too, as
[Where the stores run](hosting-the-platform.md#where-the-stores-run) explains.
Every other pod, Argo CD among them, waits Pending until the worker joins.

Web traffic from the internet reaches the worker alone. Traefik runs where the
apps run, so the ingress load balancer targets the worker at its vSwitch
address `10.0.1.2` and no control-plane node, and the nodes that hold etcd and
the Kubernetes API take no internet traffic. The block names that address to
the plane in `newPlane` in `identity.go`. The plane adds the target once the
worker's install has coupled the vSwitch subnet, since Hetzner accepts a
private target only inside a vSwitch subnet of the network. The load balancer
reaches the worker from its private address `10.0.0.4`, which the Robot
firewall's rule for the cluster network admits.

| Resource | What it does | Where |
|---|---|---|
| Network `10.0.0.0/16`, firewall, gateway at `10.0.0.2`, two load balancers, builder, snapshot and three nodes | The Cloud control plane, labelled with the subscription | The kit's `controlplane` package, driven from `Ensure` in `resource.go` |
| Cluster repository | A private GitHub repository, `<appName>-cluster`, that Argo CD syncs | `repository.go` |
| Dedicated server | The worker, renamed `mitstack-<subscription id>`, where the id is lowercase with each non-alphanumeric character turned into a dash | `robotName` in `identity.go`, `startWorkerInstall` in `worker.go` |
| vSwitch | Named like the server, coupled to the Cloud network as the subnet `10.0.1.0/24` with the network's routes exposed to it | `imageCloud.coupleSubnet` in `worker.go` |
| Robot firewall | Admits the cluster network `10.0.0.0/16` to the worker | `workerFor` in `worker.go` |
| Image server | A short-lived Cloud server labelled with the role `robot-image-server` that builds and serves the worker's disk image, deleted after | `imageCloud` in `worker.go` |

The worker sits at `10.0.1.2/24` on the vSwitch.

## How a subscribe proceeds

You subscribe your app to the block and fill in the params. mitstack sends the
operator a subscribe request with the params and the connections' tokens. The
operator checks the params, the Hetzner token and Robot login, the GitHub
connection and its own `MITSTACK_EGRESS_IPV4` first, and refuses with a named
reason when one is missing or wrong, as
[Params, connections and outputs](reference.md#params) lists. It then reads the
dedicated server and refuses one it cannot adopt, before it changes anything.

Each subscribe call then takes one stage that waits on Hetzner and answers
pending. The network and the repository need no wait, so a call makes them
beside its stage. block-kit's handler observes each resource once per call
and holds no poll, so no call is held open polling either.

While a subscription settles, the platform polls it with validate. The block's
validate answers `redrive` beside `pending` whenever a subscribe sent now would
take a stage or refuse, judged from what Hetzner, Robot and GitHub hold:

```json
{"pending": true, "redrive": true}
```

The platform then re-drives subscribe, which means it sends a fresh subscribe,
as [ADR 0097](https://github.com/mitstack-eu/mitstack/blob/main/docs/adr/0097-the-settle-check-re-drives-subscribe-when-an-operator-reports-a-long-step-finished.md)
records. While the builder builds, the snapshot images, the nodes boot or the
worker installs, validate answers `pending` alone and the platform waits.
`stepLeft` in `observe.go` makes that judgement from `nextStep` in
`resource.go`, the same function a subscribe takes its stage from. A run moves
through these stages:

1. Calls create the network, the repository and the rest of the control plane,
   as the kit's README describes: firewall, load balancers, then one call that
   mints the cluster's keys and creates the gateway and the builder.
2. In that same call the kit's `Installed` hook starts the worker's install.
   [The next section](#how-the-worker-is-installed) follows it.
3. Later calls attach the gateway and the builder and power them on. Once the
   builder powers itself off, after its image is built and the gateway's mirror
   answers, they snapshot its disk, delete the builder and create the three
   nodes, one per call.
4. The gateway starts etcd on the first node, and inside the cluster a Job
   enrols with the platform, as the kit's README and
   [Inside the cluster](inside-the-cluster.md) describe.
5. Once the worker is written, a subscribe takes the step that puts back a
   Robot resource the worker lost, if one is lost.

The cluster reads present once the control plane stands, the worker is
written, and the newest node report since the last Cloud node's creation names
every Cloud node and the worker's hostname, all Ready. mitstack then records
every output. Each stage logs how long it took, as in this line from the
tests:

```text
hcloud-hrobot-talos-cluster: subscription sub_0123456789ab: the step to create the node in nbg1 from the snapshot took 0s
```

An upgrade request, which moves a subscription to a newer release of the
block, adds whatever is missing and never replaces a server. On a cluster that
hosts mitstack it also commits the release's hosting policies for Argo CD to
apply, which
[How an upgrade changes the hosting policies](hosting-the-platform.md#how-an-upgrade-changes-the-hosting-policies)
covers.
[Build progress and failures](build-progress-and-failures.md) covers a slow or
failed build and every refusal a subscribe can end in.

## How the worker is installed

The worker's install outlasts a request, and it needs the cluster's keys, which
exist only in the call that mints them. `startWorkerInstall` in `worker.go`
therefore runs the install in the background of the operator process, from the
kit's `Installed` hook, with a one-hour limit. It marks the Cloud network's
label `mitstack.dev/worker-install` as `running` before it starts, and as
`written` or `failed` when it ends. The label `mitstack.dev/worker-install-at`
holds the time of the last mark, in Unix seconds. Every later call reads these
labels, so progress survives between calls.

The install follows [How a Robot worker is installed](robot-worker-install.md).
In order, it adopts the server and names it, couples it to the vSwitch of the
same name, writes the Robot firewall, and boots the rescue system. The image
server is created only once the gateway holds `10.0.0.2`, because a server that
joined the network first would take that address. The rescue system then writes
the disk, and the install deletes the image server.

A restart of the operator during the install loses the keys, since only the
operator's memory held them. The label still reads `running`, so the next
subscribe refuses with `worker-install-lost`, and you unsubscribe and subscribe
again. An install that ends in an error marks `failed` and the next subscribe
refuses with `worker-install-failed`.

Once the worker is written, a subscribe checks the Robot side on every call.
It puts back the server's name, its place on the vSwitch or its firewall, when
one was lost. A cancelled vSwitch it cannot put back, because the worker boots
on that vSwitch's VLAN, so it refuses with `vswitch-lost`.

## When an app unsubscribes

`Delete` in `teardown.go` runs the unsubscribe.

1. You remove the block from your app. mitstack sends the operator an
   unsubscribe request with the connections' tokens.
2. The operator stops the worker's install if it runs, and waits for the
   install's clean-up, which gives the server its firewall and boot back.
3. It deletes every Cloud resource found by the subscription's labels, the
   snapshot included, because the snapshot holds every key of the cluster.
4. It takes the dedicated server off the vSwitch and cancels the vSwitch. It
   then releases the server: its name is cleared and its firewall admits all.
   The block never cancels the server.
5. It then deletes the cluster repository, when the repository carries this
   subscription's mark, two GitHub topics. A repository without the mark stays
   untouched.
6. When the Robot login is unusable, the operator still deletes every Cloud
   resource, then refuses with `robot-left-in-place` and names what it kept.
   When the GitHub connection is unusable, it refuses with
   `repository-left-in-place` instead.
7. mitstack counts the unsubscribe finished once every resource reads absent.
