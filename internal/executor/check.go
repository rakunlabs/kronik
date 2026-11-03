package executor

import (
	"context"
	"fmt"
	"strings"

	authv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/moby/moby/client"
)

// Checker is an executor that can try its connection.
type Checker interface {
	// Check connects and says what it reached.
	Check(ctx context.Context) (string, error)
}

func (d *Docker) Check(ctx context.Context) (string, error) {
	cli, err := d.cli()
	if err != nil {
		return "", err
	}

	v, err := cli.ServerVersion(ctx, client.ServerVersionOptions{})
	if err != nil {
		return "", fmt.Errorf("docker engine: %w", err)
	}

	return fmt.Sprintf("Docker Engine %s (API %s, %s/%s) at %s", v.Version, v.APIVersion, v.Os, v.Arch, cli.DaemonHost()), nil
}

func (s *Swarm) Check(ctx context.Context) (string, error) {
	cli, err := s.cli()
	if err != nil {
		return "", err
	}

	info, err := cli.Info(ctx, client.InfoOptions{})
	if err != nil {
		return "", fmt.Errorf("docker engine: %w", err)
	}

	sw := info.Info.Swarm
	if sw.LocalNodeState != "active" {
		return "", fmt.Errorf("the engine at %s is not in a swarm (state %q)", cli.DaemonHost(), sw.LocalNodeState)
	}

	if !sw.ControlAvailable {
		return "", fmt.Errorf("the engine at %s is a swarm worker; kronik needs a manager to create services", cli.DaemonHost())
	}

	return fmt.Sprintf("Swarm manager %s, %d nodes, %d managers", info.Info.Name, sw.Nodes, sw.Managers), nil
}

func (k *Kubernetes) Check(ctx context.Context) (string, error) {
	cli, _, err := k.cli()
	if err != nil {
		return "", err
	}

	v, err := cli.Discovery().ServerVersion()
	if err != nil {
		return "", fmt.Errorf("kubernetes API: %w", err)
	}

	ns := k.defaultNamespace()

	var missing []string

	for _, rule := range []struct{ group, resource, verb string }{
		{"batch", "jobs", "create"}, {"batch", "jobs", "delete"}, {"", "pods", "list"}, {"", "pods/log", "get"},
	} {
		review, err := cli.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, &authv1.SelfSubjectAccessReview{
			Spec: authv1.SelfSubjectAccessReviewSpec{ResourceAttributes: &authv1.ResourceAttributes{
				Namespace: ns, Group: rule.group, Resource: rule.resource, Verb: rule.verb,
			}},
		}, metav1.CreateOptions{})
		if err != nil {
			return "", fmt.Errorf("check permissions: %w", err)
		}

		if !review.Status.Allowed {
			missing = append(missing, rule.verb+" "+rule.resource)
		}
	}

	if len(missing) > 0 {
		return "", fmt.Errorf("Kubernetes %s reached, but kronik may not %s in namespace %s (see the RBAC in deploy/kubernetes.yaml)",
			v.GitVersion, strings.Join(missing, ", "), ns)
	}

	return fmt.Sprintf("Kubernetes %s, namespace %s: Jobs can be created", v.GitVersion, ns), nil
}
