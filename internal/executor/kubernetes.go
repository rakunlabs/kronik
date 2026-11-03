package executor

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// KubernetesOptions are the Kubernetes settings in effect.
type KubernetesOptions struct {
	Kubeconfig        string
	Context           string
	Namespace         string
	AllowedNamespaces []string
	AllowedImages     []string
	ServiceAccount    string
	TTL               time.Duration
}

// Kubernetes runs a job as a batch/v1 Job with one pod and no retries
// (kronik retries), streams the pod's log and deletes the Job after.
type Kubernetes struct {
	opts KubernetesOptions

	mu        sync.Mutex
	client    kubernetes.Interface
	namespace string
}

func NewKubernetes(opts KubernetesOptions) *Kubernetes {
	return &Kubernetes{opts: opts}
}

// NewKubernetesWith uses a given client, for tests.
func NewKubernetesWith(opts KubernetesOptions, c kubernetes.Interface, namespace string) *Kubernetes {
	return &Kubernetes{opts: opts, client: c, namespace: namespace}
}

// inClusterNamespace is where the pod's service account lives.
const inClusterNamespace = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"

func (k *Kubernetes) cli() (kubernetes.Interface, string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()

	if k.client != nil {
		return k.client, k.namespace, nil
	}

	var (
		cfg *rest.Config
		ns  string
		err error
	)

	if k.opts.Kubeconfig == "" {
		cfg, err = rest.InClusterConfig()
		if err == nil {
			if b, err := os.ReadFile(inClusterNamespace); err == nil {
				ns = strings.TrimSpace(string(b))
			}
		}
	}

	if cfg == nil {
		rules := clientcmd.NewDefaultClientConfigLoadingRules()
		if k.opts.Kubeconfig != "" {
			rules.ExplicitPath = k.opts.Kubeconfig
		}

		cc := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{CurrentContext: k.opts.Context})

		cfg, err = cc.ClientConfig()
		if err != nil {
			return nil, "", fmt.Errorf("kubernetes: no in-cluster service account and no usable kubeconfig: %w", err)
		}

		ns, _, _ = cc.Namespace()
	}

	cfg.UserAgent = "kronik"

	c, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, "", fmt.Errorf("kubernetes client: %w", err)
	}

	if ns == "" {
		ns = "default"
	}

	k.client, k.namespace = c, ns

	return c, ns, nil
}

func (k *Kubernetes) Close() error { return nil }

// defaultNamespace is the configured one, else kronik's own.
func (k *Kubernetes) defaultNamespace() string {
	if k.opts.Namespace != "" {
		return k.opts.Namespace
	}

	if b, err := os.ReadFile(inClusterNamespace); err == nil {
		return strings.TrimSpace(string(b))
	}

	k.mu.Lock()
	defer k.mu.Unlock()

	if k.namespace != "" {
		return k.namespace
	}

	return "default"
}

func (k *Kubernetes) Validate(raw json.RawMessage) (json.RawMessage, error) {
	cfg, err := validateContainer(raw, Policy{AllowedImages: k.opts.AllowedImages})
	if err != nil {
		return nil, err
	}

	if cfg.Network != "" || len(cfg.Networks) > 0 || len(cfg.Constraints) > 0 || len(cfg.Mounts) > 0 || cfg.Privileged {
		return nil, invalid("network, networks, constraints, mounts and privileged are for Docker and Swarm jobs")
	}

	if err := k.namespaceAllowed(cfg.Namespace); err != nil {
		return nil, err
	}

	for name, ref := range cfg.SecretEnv {
		if !envName.MatchString(name) {
			return nil, invalid("secret_env name %q is not valid", name)
		}

		if s, key, ok := strings.Cut(ref, "/"); !ok || s == "" || key == "" {
			return nil, invalid("secret_env %s must be secret-name/key", name)
		}
	}

	if cfg.User != "" {
		if _, err := parseUID(cfg.User); err != nil {
			return nil, invalid("user must be a numeric uid (or uid:gid) for Kubernetes")
		}
	}

	return json.Marshal(cfg)
}

func (k *Kubernetes) namespaceAllowed(ns string) error {
	if ns == "" || ns == k.opts.Namespace {
		return nil
	}

	for _, a := range k.opts.AllowedNamespaces {
		if a == ns || a == "*" {
			return nil
		}
	}

	return invalid("namespace %q is not allowed here (Settings → Configuration → Kubernetes)", ns)
}

// parseUID reads "1000" or "1000:1000".
func parseUID(s string) ([2]int64, error) {
	var out [2]int64

	u, g, hasGroup := strings.Cut(s, ":")
	if _, err := fmt.Sscanf(u, "%d", &out[0]); err != nil || fmt.Sprint(out[0]) != u {
		return out, errBadNumber
	}

	out[1] = -1

	if hasGroup {
		if _, err := fmt.Sscanf(g, "%d", &out[1]); err != nil || fmt.Sprint(out[1]) != g {
			return out, errBadNumber
		}
	}

	return out, nil
}

// kubePoll is how often the pod's state is read.
const kubePoll = time.Second

func (k *Kubernetes) Run(ctx context.Context, info Info, raw json.RawMessage, out *Output) (*int, error) {
	cfg, err := decode[ContainerConfig](raw)
	if err != nil {
		return nil, err
	}

	if _, err := k.Validate(raw); err != nil {
		return nil, err
	}

	cli, _, err := k.cli()
	if err != nil {
		return nil, err
	}

	ns := cfg.Namespace
	if ns == "" {
		ns = k.defaultNamespace()
	}

	out.Redact(SecretValues(info.Secrets))

	env, err := runEnv(info, cfg.Env)
	if err != nil {
		return nil, err
	}

	job := k.jobSpec(info, cfg, ns, env)

	created, err := cli.BatchV1().Jobs(ns).Create(ctx, job, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("create Job in %s: %w", ns, err)
	}

	name := created.Name

	out.Printf("[kubernetes] Job %s/%s created from %s\n", ns, name, cfg.Image)

	// Deleted whatever happens: also its pod (foreground propagation).
	defer func() {
		delCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()

		policy := metav1.DeletePropagationBackground
		if err := cli.BatchV1().Jobs(ns).Delete(delCtx, name, metav1.DeleteOptions{PropagationPolicy: &policy}); err != nil && !apierrors.IsNotFound(err) {
			out.Printf("[kubernetes] deleting Job %s: %v\n", name, err)
		}
	}()

	var (
		pod       string
		logsDone  chan struct{}
		lastPhase corev1.PodPhase
		lastWhy   string
	)

	stopLogs := func() {}
	defer func() { stopLogs() }()

	t := time.NewTicker(kubePoll)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-t.C:
		}

		pods, err := cli.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: "batch.kubernetes.io/job-name=" + name})
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}

			out.Printf("[kubernetes] reading the pod: %v\n", err)

			continue
		}

		if len(pods.Items) == 0 {
			continue
		}

		p := pods.Items[0]

		if why := waitingReason(&p); why != "" && why != lastWhy {
			lastWhy = why
			out.Printf("[kubernetes] pod %s: %s\n", p.Name, why)

			if fatalWaiting(why) {
				return nil, fmt.Errorf("the pod cannot start: %s", why)
			}
		}

		if p.Status.Phase != lastPhase {
			lastPhase = p.Status.Phase
			if p.Status.Phase == corev1.PodPending && p.Spec.NodeName == "" {
				if msg := unschedulable(&p); msg != "" {
					out.Printf("[kubernetes] pod %s is not scheduled yet: %s\n", p.Name, msg)
				}
			}
		}

		// Logs once the container has started.
		if pod == "" && (p.Status.Phase == corev1.PodRunning || p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed) {
			pod = p.Name
			logCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
			stopLogs = cancel
			logsDone = make(chan struct{})

			go func() {
				defer close(logsDone)
				streamPodLogs(logCtx, cli, ns, pod, out)
			}()
		}

		switch p.Status.Phase {
		case corev1.PodSucceeded, corev1.PodFailed:
			if logsDone != nil {
				waitLogs(logsDone, stopLogs)
			}

			code, reason := exitOf(&p)
			if p.Status.Phase == corev1.PodSucceeded {
				return &code, nil
			}

			if reason == "OOMKilled" {
				return &code, fmt.Errorf("the container ran out of memory (OOMKilled, exit code %d)", code)
			}

			return &code, fmt.Errorf("exit code %d%s", code, prefixed(reason))
		}
	}
}

func (k *Kubernetes) jobSpec(info Info, cfg *ContainerConfig, ns string, env map[string]string) *batchv1.Job {
	zero := int32(0)
	grace := int64(stopGrace)

	c := corev1.Container{
		Name:       "job",
		Image:      cfg.Image,
		Command:    cfg.Command,
		Args:       cfg.Args,
		WorkingDir: cfg.Workdir,
	}

	switch cfg.Pull {
	case "always":
		c.ImagePullPolicy = corev1.PullAlways
	case "never":
		c.ImagePullPolicy = corev1.PullNever
	case "missing":
		c.ImagePullPolicy = corev1.PullIfNotPresent
	}

	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	for _, k := range keys {
		c.Env = append(c.Env, corev1.EnvVar{Name: k, Value: env[k]})
	}

	secretKeys := make([]string, 0, len(cfg.SecretEnv))
	for k := range cfg.SecretEnv {
		secretKeys = append(secretKeys, k)
	}

	sort.Strings(secretKeys)

	for _, k := range secretKeys {
		secret, key, _ := strings.Cut(cfg.SecretEnv[k], "/")
		c.Env = append(c.Env, corev1.EnvVar{Name: k, ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: secret}, Key: key},
		}})
	}

	if cfg.CPU != "" || cfg.Memory != "" {
		limits := corev1.ResourceList{}

		if cfg.CPU != "" {
			nano, _ := parseCPU(cfg.CPU)
			limits[corev1.ResourceCPU] = *resource.NewMilliQuantity(nano/1_000_000, resource.DecimalSI)
		}

		if cfg.Memory != "" {
			b, _ := parseBytes(cfg.Memory)
			limits[corev1.ResourceMemory] = *resource.NewQuantity(b, resource.BinarySI)
		}

		c.Resources = corev1.ResourceRequirements{Limits: limits, Requests: limits}
	}

	if cfg.User != "" {
		ids, _ := parseUID(cfg.User)
		c.SecurityContext = &corev1.SecurityContext{RunAsUser: &ids[0]}

		if ids[1] >= 0 {
			c.SecurityContext.RunAsGroup = &ids[1]
		}
	}

	sa := cfg.ServiceAccount
	if sa == "" {
		sa = k.opts.ServiceAccount
	}

	lbl := map[string]string{}
	for k, v := range labels(info, cfg.Labels) {
		lbl[k] = kubeLabelValue(v)
	}

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "kronik-" + shortID(info.RunID),
			Namespace: ns,
			Labels:    lbl,
			Annotations: map[string]string{
				"kronik.job-name": info.JobName,
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: &zero,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: lbl},
				Spec: corev1.PodSpec{
					RestartPolicy:                 corev1.RestartPolicyNever,
					Containers:                    []corev1.Container{c},
					ServiceAccountName:            sa,
					NodeSelector:                  cfg.NodeSelector,
					TerminationGracePeriodSeconds: &grace,
				},
			},
		},
	}

	if k.opts.TTL > 0 {
		ttl := int32(k.opts.TTL / time.Second)
		job.Spec.TTLSecondsAfterFinished = &ttl
	}

	return job
}

// kubeLabelValue fits a value into a label: 63 characters of
// [A-Za-z0-9-_.], starting and ending alphanumeric.
func kubeLabelValue(v string) string {
	ok := func(c byte) bool {
		return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.'
	}

	b := []byte(v)
	for i, c := range b {
		if !ok(c) {
			b[i] = '-'
		}
	}

	s := strings.Trim(string(b), "-_.")
	if len(s) > 63 {
		s = strings.Trim(s[:63], "-_.")
	}

	return s
}

func waitingReason(p *corev1.Pod) string {
	for _, cs := range p.Status.ContainerStatuses {
		if w := cs.State.Waiting; w != nil && w.Reason != "" && w.Reason != "ContainerCreating" {
			return w.Reason + prefixed(w.Message)
		}
	}

	return ""
}

// fatalWaiting are the reasons a pod never starts by itself.
func fatalWaiting(why string) bool {
	for _, r := range []string{"ErrImagePull", "ImagePullBackOff", "InvalidImageName", "CreateContainerConfigError", "CreateContainerError"} {
		if strings.HasPrefix(why, r) {
			return true
		}
	}

	return false
}

func unschedulable(p *corev1.Pod) string {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse {
			return c.Message
		}
	}

	return ""
}

func exitOf(p *corev1.Pod) (int, string) {
	for _, cs := range p.Status.ContainerStatuses {
		if t := cs.State.Terminated; t != nil {
			return int(t.ExitCode), t.Reason
		}
	}

	if p.Status.Phase == corev1.PodSucceeded {
		return 0, ""
	}

	return 1, p.Status.Reason
}

func streamPodLogs(ctx context.Context, cli kubernetes.Interface, ns, pod string, out *Output) {
	req := cli.CoreV1().Pods(ns).GetLogs(pod, &corev1.PodLogOptions{Container: "job", Follow: true})

	stream, err := req.Stream(ctx)
	if err != nil {
		if ctx.Err() == nil {
			out.Printf("[kubernetes] reading logs: %v\n", err)
		}

		return
	}
	defer func() { _ = stream.Close() }()

	r := bufio.NewReaderSize(stream, 64<<10)

	buf := make([]byte, 32<<10)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			_, _ = out.Write(buf[:n])
		}

		if err != nil {
			if !errors.Is(err, context.Canceled) && ctx.Err() == nil && err.Error() != "EOF" {
				out.Printf("[kubernetes] reading logs: %v\n", err)
			}

			return
		}
	}
}
