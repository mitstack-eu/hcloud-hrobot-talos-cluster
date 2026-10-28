package main

// A platform-hosting install's keys exist only in the install's memory, the cluster, and the cluster repository's
// files as PGP messages to the owner's recipient key, so no person stores or seals one.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/mitstack-eu/block-kit/handler"
	"github.com/mitstack-eu/block-kit/helpers/safe"
	"github.com/mitstack-eu/block-kit/observe"
	"github.com/mitstack-eu/talos-cluster-kit/components"
	"github.com/mitstack-eu/talos-cluster-kit/controlplane"
	"golang.org/x/crypto/ssh"
)

const (
	paramRecipient = "openbaoSealKeyRecipient"

	// The files a hosting install commits at the top of the cluster repository, which Argo CD syncs none of.
	sealKeyFile          = "openbao-seal-key.asc"
	sealedSecretsKeyFile = "sealed-secrets-key.asc"
	sealedSecretsCert    = "sealed-secrets.crt"
	openbaoInitFile      = "openbao-init.asc"

	writeKeyTitle = "mitstack OpenBao init"
	readKeyTitle  = "mitstack Argo CD"
)

// readRecipient refuses a hosting request whose recipient is not one armored public key that can be encrypted to.
func readRecipient(raw string) ([]byte, error) {
	if raw == "" {
		return nil, handler.Refuse("missing-param",
			`params["%s"] is empty, and a cluster that hosts the platform hands its keys out only as PGP messages to that key`, paramRecipient)
	}
	if _, err := encryptTo([]byte(raw), nil); err != nil {
		return nil, handler.Refuse("invalid-param", `params["%s"] is not one armored OpenPGP public key that can be encrypted to: %v`, paramRecipient, err)
	}
	return []byte(raw), nil
}

// encryptTo is plain as an armored PGP message to the one public key in recipient.
func encryptTo(recipient, plain []byte) (string, error) {
	block, err := armor.Decode(bytes.NewReader(recipient))
	if err != nil || block.Type != openpgp.PublicKeyType {
		return "", fmt.Errorf("the key is not an armored public key block")
	}
	keys, err := openpgp.ReadKeyRing(block.Body)
	if err != nil {
		return "", fmt.Errorf("reading the key: %w", err)
	}
	if len(keys) != 1 {
		return "", fmt.Errorf("the block holds %d keys, want one", len(keys))
	}
	var out bytes.Buffer
	w, err := armor.Encode(&out, "PGP MESSAGE", nil)
	if err != nil {
		return "", err
	}
	pt, err := openpgp.Encrypt(w, keys, nil, nil, nil)
	if err != nil {
		return "", fmt.Errorf("encrypting to the key: %w", err)
	}
	if _, err := pt.Write(plain); err != nil {
		return "", err
	}
	if err := pt.Close(); err != nil {
		return "", err
	}
	if err := w.Close(); err != nil {
		return "", err
	}
	return out.String() + "\n", nil
}

// hosting gives the install the cluster repository's deploy keys, sealed to the keys it mints, and commits those keys
// as PGP messages to the recipient.
func (req ensureRequest) hosting() *controlplane.Hosting {
	return &controlplane.Hosting{
		Repository: req.gh.repo.fullName(),
		Recipient:  req.params.recipient,
		DeployKeys: func(ctx context.Context, keys controlplane.HostingKeys) (controlplane.SealedDeployKeys, error) {
			return req.gh.deployKeys(ctx, keys, req.params.recipient)
		},
	}
}

type deployKey struct {
	ID       int64  `json:"id"`
	Title    string `json:"title"`
	Key      string `json:"key"`
	ReadOnly bool   `json:"read_only"`
}

// deployKeys replaces any deploy key an earlier install left under the block's titles, since that install's private
// halves are gone, then creates both keys and records what the owner needs to recover the cluster.
func (g *githubAccess) deployKeys(ctx context.Context, keys controlplane.HostingKeys, recipient []byte) (controlplane.SealedDeployKeys, error) {
	if err := g.deleteDeployKeys(ctx); err != nil {
		return controlplane.SealedDeployKeys{}, err
	}
	write, err := g.createDeployKey(ctx, writeKeyTitle, false)
	if err != nil {
		return controlplane.SealedDeployKeys{}, err
	}
	read, err := g.createDeployKey(ctx, readKeyTitle, true)
	if err != nil {
		return controlplane.SealedDeployKeys{}, err
	}
	var sealed controlplane.SealedDeployKeys
	if sealed.Write, err = components.Seal(keys.SealedSecretsCertPEM, components.DeployKeyNamespace, components.DeployKeySecret, components.DeployKeyItem, write); err != nil {
		return controlplane.SealedDeployKeys{}, err
	}
	if sealed.Read, err = components.Seal(keys.SealedSecretsCertPEM, components.RepositoryCredentialNamespace, components.RepositoryCredentialName, components.RepositoryCredentialItem, read); err != nil {
		return controlplane.SealedDeployKeys{}, err
	}
	sealKey, err := encryptTo(recipient, []byte(base64.StdEncoding.EncodeToString(keys.SealKey)))
	if err != nil {
		return controlplane.SealedDeployKeys{}, fmt.Errorf("encrypting OpenBao's seal key: %w", err)
	}
	sealedSecretsKey, err := encryptTo(recipient, keys.SealedSecretsKeyPEM)
	if err != nil {
		return controlplane.SealedDeployKeys{}, fmt.Errorf("encrypting the Sealed Secrets key: %w", err)
	}
	for _, f := range []struct{ path, content string }{
		{sealKeyFile, sealKey}, {sealedSecretsKeyFile, sealedSecretsKey}, {sealedSecretsCert, string(keys.SealedSecretsCertPEM)},
	} {
		if err := g.putFile(ctx, f.path, f.content); err != nil {
			return controlplane.SealedDeployKeys{}, err
		}
	}
	return sealed, nil
}

// deployKeyProblems names each of the install's two deploy keys that repo lacks, holds twice, or holds at the wrong access.
func deployKeyProblems(repo string, keys []deployKey) []string {
	var problems []string
	for _, want := range []deployKey{{Title: writeKeyTitle}, {Title: readKeyTitle, ReadOnly: true}} {
		var matched []deployKey
		for _, k := range keys {
			if k.Title == want.Title {
				matched = append(matched, k)
			}
		}
		switch {
		case len(matched) == 0:
			problems = append(problems, fmt.Sprintf("the cluster repository %s holds no deploy key titled %q", repo, want.Title))
		case len(matched) > 1:
			problems = append(problems, fmt.Sprintf("the cluster repository %s holds %d deploy keys titled %q", repo, len(matched), want.Title))
		case matched[0].ReadOnly != want.ReadOnly:
			problems = append(problems, fmt.Sprintf("the deploy key %q on %s is %s, and the install made it %s", want.Title, repo, access(matched[0].ReadOnly), access(want.ReadOnly)))
		}
	}
	return problems
}

func access(readOnly bool) string {
	if readOnly {
		return "read-only"
	}
	return "write"
}

func (g *githubAccess) listDeployKeys(ctx context.Context) ([]deployKey, error) {
	raw, found, err := g.getIfFound(ctx, "/repos/"+g.repo.fullName()+"/keys?per_page=100", "the deploy keys of "+g.repo.fullName())
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("reading the deploy keys of %s: github answered 404", g.repo.fullName())
	}
	var keys []deployKey
	if err := json.Unmarshal(raw, &keys); err != nil {
		return nil, fmt.Errorf("reading the deploy keys of %s: %w", g.repo.fullName(), err)
	}
	return keys, nil
}

func (g *githubAccess) deleteDeployKeys(ctx context.Context) error {
	keys, err := g.listDeployKeys(ctx)
	if err != nil {
		return err
	}
	for _, k := range keys {
		if k.Title != writeKeyTitle && k.Title != readKeyTitle {
			continue
		}
		status, raw, err := g.call(ctx, http.MethodDelete, "/repos/"+g.repo.fullName()+"/keys/"+strconv.FormatInt(k.ID, 10), nil)
		if err != nil {
			return err
		}
		if status != http.StatusNoContent && status != http.StatusNotFound {
			return fmt.Errorf("deleting the deploy key %q of %s: github returned %d: %s", k.Title, g.repo.fullName(), status, safe.LogSafe(string(raw), g.client.Token))
		}
	}
	return nil
}

// createDeployKey registers a new ed25519 key's public half and returns its private half in OpenSSH form.
func (g *githubAccess) createDeployKey(ctx context.Context, title string, readOnly bool) ([]byte, error) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generating the deploy key %q: %w", title, err)
	}
	sshPublic, err := ssh.NewPublicKey(public)
	if err != nil {
		return nil, fmt.Errorf("encoding the deploy key %q: %w", title, err)
	}
	block, err := ssh.MarshalPrivateKey(private, title)
	if err != nil {
		return nil, fmt.Errorf("encoding the deploy key %q: %w", title, err)
	}
	status, raw, err := g.call(ctx, http.MethodPost, "/repos/"+g.repo.fullName()+"/keys", map[string]any{
		"title": title, "key": strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPublic))), "read_only": readOnly,
	})
	if err != nil {
		return nil, err
	}
	if status != http.StatusCreated {
		return nil, fmt.Errorf("creating the deploy key %q on %s: github returned %d: %s", title, g.repo.fullName(), status, safe.LogSafe(string(raw), g.client.Token))
	}
	return pem.EncodeToMemory(block), nil
}

// putFile writes path whole, replacing what an earlier install wrote there.
func (g *githubAccess) putFile(ctx context.Context, path, content string) error {
	contents := "/repos/" + g.repo.fullName() + "/contents/" + path
	raw, found, err := g.getIfFound(ctx, contents, path+" in "+g.repo.fullName())
	if err != nil {
		return err
	}
	body := map[string]any{
		"message": "Record " + path + " for this cluster's install\n\n" + observe.GitHubSubscriptionTrailer(g.subID),
		"content": base64.StdEncoding.EncodeToString([]byte(content)),
	}
	if found {
		var existing struct {
			SHA string `json:"sha"`
		}
		if err := json.Unmarshal(raw, &existing); err != nil {
			return fmt.Errorf("reading %s in %s: %w", path, g.repo.fullName(), err)
		}
		body["sha"] = existing.SHA
	}
	status, answer, err := g.call(ctx, http.MethodPut, contents, body)
	if err != nil {
		return err
	}
	if status != http.StatusCreated && status != http.StatusOK {
		return fmt.Errorf("writing %s in %s: github returned %d: %s", path, g.repo.fullName(), status, safe.LogSafe(string(answer), g.client.Token))
	}
	return nil
}

// readFile is path's content in the cluster repository, and whether the repository holds it.
func (g *githubAccess) readFile(ctx context.Context, path string) (string, bool, error) {
	raw, found, err := g.getIfFound(ctx, "/repos/"+g.repo.fullName()+"/contents/"+path, path+" in "+g.repo.fullName())
	if err != nil || !found {
		return "", found, err
	}
	var file struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		return "", false, fmt.Errorf("reading %s in %s: %w", path, g.repo.fullName(), err)
	}
	decoded, err := io.ReadAll(base64.NewDecoder(base64.StdEncoding, strings.NewReader(strings.ReplaceAll(file.Content, "\n", ""))))
	if err != nil {
		return "", false, fmt.Errorf("decoding %s in %s: %w", path, g.repo.fullName(), err)
	}
	return string(decoded), true, nil
}

// hostingOutputs are a hosting cluster's recovery outputs, or the file the repository does not hold yet.
func (g *githubAccess) hostingOutputs(ctx context.Context) (map[string]string, string, error) {
	outputs := map[string]string{}
	for output, path := range map[string]string{
		"openbaoSealKey": sealKeyFile, "sealedSecretsKey": sealedSecretsKeyFile,
		"sealedSecretsCertificate": sealedSecretsCert, "openbaoInit": openbaoInitFile,
	} {
		content, found, err := g.readFile(ctx, path)
		if err != nil {
			return nil, "", err
		}
		if !found {
			return nil, "the cluster repository " + g.repo.fullName() + " holds no " + path + " yet", nil
		}
		outputs[output] = content
	}
	return outputs, "", nil
}
