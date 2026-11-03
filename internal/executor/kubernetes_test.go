package executor

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestKubernetesValidate(t *testing.T) {
	k := NewKubernetes(KubernetesOptions{Namespace: "jobs", AllowedNamespaces: []string{"batch"}, AllowedImages: []string{"alpine:*"}})

	for _, raw := range []string{
		`{"image":"busybox"}`,
		`{"image":"alpine:3","namespace":"kube-system"}`,
		`{"image":"alpine:3","mounts":[{"source":"/","target":"/h"}]}`,
		`{"image":"alpine:3","privileged":true}`,
		`{"image":"alpine:3","user":"root"}`,
		`{"image":"alpine:3","secret_env":{"TOKEN":"no-key"}}`,
	} {
		if _, err := k.Validate(json.RawMessage(raw)); err == nil {
			t.Errorf("Validate(%s) succeeded", raw)
		}
	}

	for _, raw := range []string{
		`{"image":"alpine:3"}`,
		`{"image":"alpine:3","namespace":"batch","user":"1000:1000","secret_env":{"TOKEN":"api/token"},"cpu":"250m","memory":"128Mi"}`,
	} {
		if _, err := k.Validate(json.RawMessage(raw)); err != nil {
			t.Errorf("Validate(%s): %v", raw, err)
		}
	}
}

// The Job's pod is driven by the fake clientset: created, then running,
// then failed with an exit code.
func TestKubernetesRunFake(t *testing.T) {
	cs := fake.NewClientset()

	// The fake does not run pods; make one for every Job created.
	cs.PrependReactor("create", "jobs", func(a k8stesting.Action) (bool, runtime.Object, error) {
		job := a.(k8stesting.CreateAction).GetObject().(interface{ GetName() string })

		go func() {
			time.Sleep(100 * time.Millisecond)

			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: job.GetName() + "-x", Namespace: "jobs",
					Labels: map[string]string{"batch.kubernetes.io/job-name": job.GetName()}},
				Status: corev1.PodStatus{Phase: corev1.PodFailed, ContainerStatuses: []corev1.ContainerStatus{{
					Name: "job", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 7, Reason: "Error"}},
				}}},
			}
			_, _ = cs.CoreV1().Pods("jobs").Create(context.Background(), pod, metav1.CreateOptions{})
		}()

		return false, nil, nil
	})

	k := NewKubernetesWith(KubernetesOptions{Namespace: "jobs", TTL: time.Minute}, cs, "jobs")

	cfg, err := k.Validate(json.RawMessage(`{"image":"alpine:3","command":["sh","-c","exit 7"],"cpu":"500m","memory":"64Mi","env":{"A":"1"},"secret_env":{"T":"s/k"}}`))
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	out := NewOutput(1 << 12)

	code, err := k.Run(ctx, Info{JobID: "j", JobName: "kube test", RunID: "01TESTRUNKUBE", Attempt: 1, ScheduledAt: time.Now()}, cfg, out)
	if err == nil || code == nil || *code != 7 {
		text, _, _ := out.Snapshot()
		t.Fatalf("code=%v err=%v\n%s", code, err, text)
	}

	// The Job was made as asked, then deleted.
	var created bool

	for _, a := range cs.Actions() {
		if a.GetVerb() == "create" && a.GetResource().Resource == "jobs" {
			created = true
			job := a.(k8stesting.CreateAction).GetObject()
			b, _ := json.Marshal(job)
			s := string(b)

			for _, want := range []string{`"backoffLimit":0`, `"restartPolicy":"Never"`, `"ttlSecondsAfterFinished":60`,
				`"cpu":"500m"`, `"memory":"64Mi"`, `"kronik.run":"01TESTRUNKUBE"`, `"secretKeyRef":{"name":"s","key":"k"}`, `"KRONIK_JOB_NAME"`} {
				if !strings.Contains(s, want) {
					t.Errorf("Job lacks %s:\n%s", want, s)
				}
			}
		}
	}

	if !created {
		t.Fatal("no Job created")
	}

	jobs, _ := cs.BatchV1().Jobs("jobs").List(context.Background(), metav1.ListOptions{})
	if len(jobs.Items) != 0 {
		t.Fatalf("Job left: %v", jobs.Items)
	}
}

// TestKubernetesRunCluster runs against a real cluster (kind) when
// KRONIK_TEST_KUBECONFIG names its kubeconfig.
func TestKubernetesRunCluster(t *testing.T) {
	kc := os.Getenv("KRONIK_TEST_KUBECONFIG")
	if kc == "" {
		t.Skip("KRONIK_TEST_KUBECONFIG is not set")
	}

	k := NewKubernetes(KubernetesOptions{Kubeconfig: kc, Namespace: "default", TTL: time.Minute})

	cfg, err := k.Validate(json.RawMessage(`{"image":"` + testImage + `","pull":"never","command":["sh","-c"],` +
		`"args":["echo hello from $KRONIK_JOB_NAME; exit 5"],"memory":"64Mi"}`))
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	out := NewOutput(1 << 14)

	code, err := k.Run(ctx, Info{JobID: "j", JobName: "kind-test", RunID: "01testrunkind", Attempt: 1, ScheduledAt: time.Now()}, cfg, out)

	text, _, _ := out.Snapshot()
	if err == nil || code == nil || *code != 5 || !strings.Contains(text, "hello from kind-test") {
		t.Fatalf("code=%v err=%v output:\n%s", code, err, text)
	}

	t.Logf("output:\n%s", text)
}
