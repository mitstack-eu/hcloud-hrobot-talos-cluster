# A cluster that hosts mitstack

This page covers the cluster the block builds when `hostsPlatform` is `true`:
the one cluster that runs mitstack's own stores, OpenBao for secrets and
Sealed Secrets for encrypted manifests. It is for whoever runs that cluster
for mitstack. No app's cluster sets the param, so an app owner can skip it.

## What you give

A hosting subscribe needs one more param, `openbaoSealKeyRecipient`. It holds
one armored OpenPGP public key, the key you can decrypt with. The install hands
out the cluster's own keys only as messages encrypted to it, so nobody stores
or seals one by hand.

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
certificate. The private halves reach the cluster only sealed, inside the
hosting set the serving server hands the builder once. A deploy key with any
other title, such as one you added yourself, is left alone.

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

To read one, save the output to a file and decrypt it with your key:

```bash
gpg --decrypt openbao-seal-key.asc | base64 -d > openbao-seal.key
```

## Where the stores run

A hosting cluster runs no app on its worker. OpenBao's members keep their data
on volumes pinned to the Cloud nodes, and Traefik answers only on a node that
runs it. The block therefore sets the kit's `AppsOnWorkers` to `false` for a
hosting cluster, in `plane` in `resource.go`.
