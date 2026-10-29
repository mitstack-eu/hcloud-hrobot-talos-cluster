# A cluster that hosts mitstack

This page covers the one cluster that runs mitstack's own stores: OpenBao,
which keeps secrets, and Sealed Secrets, which opens encrypted manifests inside
the cluster. It is for whoever runs that cluster for mitstack. No app's cluster
hosts mitstack, so an app owner can skip it.

This repository is a block, a service mitstack builds for an app when the app
subscribes to it. A subscribe is that request, and it carries params such as
`appName`, which [Params, connections and outputs](reference.md#params) lists.
The block builds the cluster with talos-cluster-kit, a Go library both Talos
blocks share, which this page calls the kit. The install is the part of a
subscribe that mints the cluster's keys and builds its first nodes.

## What you give

A subscribe for this cluster sets two params beside `appName`:
`hostsPlatform` set to `true`, and `openbaoSealKeyRecipient`. The second holds
one armored OpenPGP public key, the key you can decrypt with. The install hands
out the cluster's own keys only as messages encrypted to it, so nobody stores
or seals one by hand.

To make the value from a key in your GnuPG keyring, export its public half:

```bash
gpg --armor --export admin@example.com
```

`readRecipient` in `hosting.go` reads it before anything is built. An empty
value is refused with `missing-param`. A value that is not one public key that
can be encrypted to, such as a signing-only key, is refused with
`invalid-param`.

## The two deploy keys

The cluster repository of a hosting cluster carries two deploy keys, SSH keys
that GitHub grants on that one repository. The install creates both.

| Title | Access | Used by |
|---|---|---|
| `mitstack OpenBao init` | Write | OpenBao's init Job, which commits `openbao-init.asc` once OpenBao first starts |
| `mitstack Argo CD` | Read-only | Argo CD, which reads the repository over `ssh://git@ssh.github.com:443/<owner>/<repo>.git` |

`deployKeys` in `hosting.go` first deletes any deploy key with either title.
An earlier install's private halves went with that install, so its keys can
never be used again. It then generates both keys, registers each public half
with GitHub, and seals each private half to the cluster's own Sealed Secrets
certificate. A deploy key with any other title, such as one you added
yourself, is left alone.

The private halves reach the cluster only sealed, inside the hosting set, the
manifests of mitstack's stores. The builder, a short-lived server that makes
the cluster's disk image, fetches that set once from a second short-lived
server, the serving server, and both are deleted once the image is made.

Unsubscribe deletes the repository, and GitHub deletes its deploy keys with it.

## How the repository resource reads the keys

The `github.repository` resource of a hosting cluster reads present only when
the repository holds exactly one key of each title, at the access in the table
above. Otherwise it reads absent, and its status detail names each missing,
doubled or wrong key, as in this line:

```text
the deploy key "mitstack OpenBao init" on acme/shop-cluster is read-only, and the install made it write
```

`deployKeyProblems` in `hosting.go` makes that judgement, and
`repositoryObserver` in `observe.go` applies it only when `hostsPlatform` is
`true`. A cluster that hosts nothing reads present without any deploy key.

## What the install commits

The install commits three files at the top of the cluster repository, outside
the `cluster/` folder that Argo CD applies. OpenBao's init Job adds a fourth.

| File | What it holds |
|---|---|
| `openbao-seal-key.asc` | OpenBao's seal key, base64-encoded, as a message encrypted to your key |
| `sealed-secrets-key.asc` | The Sealed Secrets private key in PEM form, as a message encrypted to your key |
| `sealed-secrets.crt` | The Sealed Secrets certificate, in plain text, which anyone may seal to |
| `openbao-init.asc` | OpenBao's root token and its one recovery key, each as a message encrypted to your key, committed by the init Job |

## The recovery outputs

The cluster resource carries four more outputs once it reads present, each a
copy of one of those files.

| Output | File |
|---|---|
| `openbaoSealKey` | `openbao-seal-key.asc` |
| `sealedSecretsKey` | `sealed-secrets-key.asc` |
| `sealedSecretsCertificate` | `sealed-secrets.crt` |
| `openbaoInit` | `openbao-init.asc` |

`hostingOutputs` in `hosting.go` reads them. Until the repository holds all
four files, the cluster reads unknown with a detail such as `the cluster
repository acme/shop-cluster holds no openbao-init.asc yet`.

Each output is a copy of a file in the cluster repository, so you can read it
from GitHub as well as from the outputs mitstack records for the cluster. This
command fetches the seal key's file and decrypts it with your key:

```bash
gh api repos/acme/shop-cluster/contents/openbao-seal-key.asc --jq .content \
  | base64 -d | gpg --decrypt | base64 -d > openbao-seal.key
```

## Where the stores run

A hosting cluster keeps OpenBao's data on the Cloud nodes and runs everything
else, Sealed Secrets and the web traffic included, on the Robot worker. The
nodes that hold etcd, the Kubernetes API and OpenBao's data therefore take no
internet traffic.

The kit's `AppsOnWorkers` setting decides where pods run. On, the Cloud nodes
keep Talos's `NoSchedule` taint, a mark that turns away every pod that does
not tolerate it. The block keeps the setting on for a hosting cluster, as for
any other, and `plane` in `resource.go` adds only the hosting set. OpenBao's
three members tolerate the taint, and the kit pins each member's volume to one
Cloud node, so each node in `fsn1`, `nbg1` and `hel1` runs one member.
Traefik, cert-manager, trust-manager, External Secrets, Sealed Secrets, Argo
CD and mitstack's own workloads tolerate no taint and run on the worker.

The ingress load balancer therefore targets the worker at `10.0.1.2` and no
Cloud node, and the API load balancer targets the Cloud nodes.
`hetznerReader` in `observe.go` judges a hosting cluster's ingress by that
worker target, as for any other cluster.

Each OpenBao member mounts the TLS certificate that cert-manager issues, and
cert-manager runs on the worker. The members therefore start only once the
worker has joined.
