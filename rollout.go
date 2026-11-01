package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/mitstack-eu/block-kit/contract"
	"github.com/mitstack-eu/talos-cluster-kit/rollout"
)

// rollOut commits each file of the release the cluster repository does not hold as stated, the policies before the
// Application that prunes what they drop, so Argo CD applies a running cluster's hosting policies, which Talos never
// re-applies once it created them.
func (g *githubAccess) rollOut(ctx context.Context, release rollout.Release) error {
	files, err := release.Files(g.repo.fullName())
	if err != nil {
		return err
	}
	for _, f := range files {
		held, found, err := g.readFile(ctx, f.Path)
		if err != nil {
			return err
		}
		if found && held == string(f.Bytes) {
			continue
		}
		if err := g.putFile(ctx, f.Path, string(f.Bytes)); err != nil {
			return err
		}
	}
	return nil
}

// readBack reads the release back from the newest node report once the repository holds its files, returning what
// it read back or the problem. A repository holding another release's files, or none, reads nothing back and no
// problem, since only an upgrade replaces them.
func (g *githubAccess) readBack(ctx context.Context, d contract.Desired, release rollout.Release) (applied, problem string, err error) {
	files, err := release.Files(g.repo.fullName())
	if err != nil {
		return "", "", err
	}
	for _, f := range files {
		held, found, err := g.readFile(ctx, f.Path)
		if err != nil || !found || held != string(f.Bytes) {
			return "", "", err
		}
	}
	reports, err := nodeReports.NodeReports(ctx, d, d.SubscriptionID)
	if err != nil {
		return "", fmt.Sprintf("the node reports cannot be read to read the hosting policies back: %v", err), nil
	}
	newest := newestReport(reports, time.Time{})
	if newest == nil {
		return "", "the cluster has sent no node report to read the hosting policies back from", nil
	}
	reading, err := release.ReadBack(newest.Body)
	if err != nil {
		return "", "", err
	}
	if reading.Problem != "" {
		return "", reading.Problem, nil
	}
	return fmt.Sprintf("Argo CD's Application %s holds %s", rollout.Application, strings.Join(reading.Applied, ", ")), "", nil
}
