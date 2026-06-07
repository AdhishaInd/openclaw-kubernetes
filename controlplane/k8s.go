package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/remotecommand"
)

const (
	labelUser      = "openclaw.io/user"
	labelManagedBy = "openclaw.io/managed-by"
	annEmail       = "openclaw.io/email"
	annLastActive  = "openclaw.io/last-activity"
	annCronNext    = "openclaw.io/cron-next" // earliest enabled cron nextRunAtMs (epoch ms)
	annBusy        = "openclaw.io/busy"      // "1" while held up for cron/webhook work (reaper skips)
	managedByValue = "controlplane"
	sharedKeyField = "anthropic-key"
)

// K8s wraps a clientset plus the control-plane config.
type K8s struct {
	cs  kubernetes.Interface
	rc  *rest.Config
	cfg Config
}

func newK8s(cfg Config) (*K8s, error) {
	rc, err := rest.InClusterConfig()
	if err != nil {
		// fall back to local kubeconfig for development
		rc, err = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
			clientcmd.NewDefaultClientConfigLoadingRules(),
			&clientcmd.ConfigOverrides{},
		).ClientConfig()
		if err != nil {
			return nil, fmt.Errorf("no in-cluster or local kube config: %w", err)
		}
	}
	cs, err := kubernetes.NewForConfig(rc)
	if err != nil {
		return nil, err
	}
	return &K8s{cs: cs, rc: rc, cfg: cfg}, nil
}

// execInGateway runs a command in the gateway container of a running pod for the
// user and returns combined stdout.
func (k *K8s) execInGateway(ctx context.Context, id string, command ...string) (string, error) {
	pods, err := k.cs.CoreV1().Pods(k.cfg.UsersNS).List(ctx, metav1.ListOptions{LabelSelector: labelUser + "=" + id})
	if err != nil {
		return "", err
	}
	var podName string
	for _, p := range pods.Items {
		if p.Status.Phase == corev1.PodRunning {
			podName = p.Name
			break
		}
	}
	if podName == "" {
		return "", fmt.Errorf("no running pod for user %s", id)
	}
	req := k.cs.CoreV1().RESTClient().Post().Resource("pods").Name(podName).Namespace(k.cfg.UsersNS).
		SubResource("exec").VersionedParams(&corev1.PodExecOptions{
		Container: "gateway", Command: command, Stdout: true, Stderr: true,
	}, scheme.ParameterCodec)
	executor, err := remotecommand.NewSPDYExecutor(k.rc, "POST", req.URL())
	if err != nil {
		return "", err
	}
	var stdout, stderr bytes.Buffer
	if err := executor.StreamWithContext(ctx, remotecommand.StreamOptions{Stdout: &stdout, Stderr: &stderr}); err != nil {
		return stdout.String(), fmt.Errorf("%w: %s", err, stderr.String())
	}
	return stdout.String(), nil
}

// patchSecretData merge-patches string fields into a user's Secret.
func (k *K8s) patchSecretData(ctx context.Context, id string, data map[string]string) error {
	enc := make(map[string]string, len(data))
	for k2, v := range data {
		enc[k2] = base64.StdEncoding.EncodeToString([]byte(v))
	}
	b, _ := json.Marshal(map[string]any{"data": enc})
	_, err := k.cs.CoreV1().Secrets(k.cfg.UsersNS).Patch(
		ctx, secretName(id), types.MergePatchType, b, metav1.PatchOptions{})
	return err
}

var trycloudflareRe = regexp.MustCompile(`https://[a-z0-9-]+\.trycloudflare\.com`)

// discoverTunnelURL reads the in-cluster cloudflared pod's logs and extracts the
// public quick-tunnel URL it printed at startup.
func (k *K8s) discoverTunnelURL(ctx context.Context, systemNS string) (string, error) {
	pods, err := k.cs.CoreV1().Pods(systemNS).List(ctx, metav1.ListOptions{LabelSelector: "app=cloudflared"})
	if err != nil {
		return "", err
	}
	for _, p := range pods.Items {
		if p.Status.Phase != corev1.PodRunning {
			continue
		}
		raw, err := k.cs.CoreV1().Pods(systemNS).GetLogs(p.Name, &corev1.PodLogOptions{}).DoRaw(ctx)
		if err != nil {
			continue
		}
		if m := trycloudflareRe.Find(raw); m != nil {
			return string(m), nil
		}
	}
	return "", fmt.Errorf("no trycloudflare URL found in cloudflared logs yet")
}

// restart triggers a rolling restart of a user's Deployment (Recreate strategy)
// by bumping a pod-template annotation, so the gateway re-reads config on start.
func (k *K8s) restart(ctx context.Context, id string) error {
	patch := fmt.Sprintf(`{"spec":{"template":{"metadata":{"annotations":{"openclaw.io/restarted-at":%q}}}}}`,
		time.Now().UTC().Format(time.RFC3339Nano))
	_, err := k.cs.AppsV1().Deployments(k.cfg.UsersNS).Patch(
		ctx, deployName(id), types.MergePatchType, []byte(patch), metav1.PatchOptions{})
	return err
}

// getDeploy fetches a user's Deployment.
func (k *K8s) getDeploy(ctx context.Context, id string) (*appsv1.Deployment, error) {
	return k.cs.AppsV1().Deployments(k.cfg.UsersNS).Get(ctx, deployName(id), metav1.GetOptions{})
}

// setAnnotations merge-patches annotations on a user's Deployment. A nil value
// (empty string mapped via JSON null) removes the key.
func (k *K8s) setAnnotations(ctx context.Context, id string, anns map[string]*string) error {
	parts := make([]string, 0, len(anns))
	for key, val := range anns {
		if val == nil {
			parts = append(parts, fmt.Sprintf("%q:null", key))
		} else {
			parts = append(parts, fmt.Sprintf("%q:%q", key, *val))
		}
	}
	patch := fmt.Sprintf(`{"metadata":{"annotations":{%s}}}`, joinComma(parts))
	_, err := k.cs.AppsV1().Deployments(k.cfg.UsersNS).Patch(
		ctx, deployName(id), types.MergePatchType, []byte(patch), metav1.PatchOptions{})
	return err
}

func joinComma(s []string) string {
	out := ""
	for i, v := range s {
		if i > 0 {
			out += ","
		}
		out += v
	}
	return out
}

// userID is a DNS-safe, stable id derived from the email.
func userID(email string) string {
	sum := sha256.Sum256([]byte(email))
	return hex.EncodeToString(sum[:])[:16]
}

func deployName(id string) string  { return "oc-" + id }
func serviceName(id string) string { return "oc-" + id }
func secretName(id string) string  { return "oc-user-" + id }
func pvcName(id string) string     { return "oc-state-" + id }

func (k *K8s) serviceHost(id string) string {
	return fmt.Sprintf("%s.%s.svc:18789", serviceName(id), k.cfg.UsersNS)
}

func userLabels(id string) map[string]string {
	return map[string]string{
		labelUser:      id,
		labelManagedBy: managedByValue,
		"app":          "openclaw-user",
	}
}

func int32p(v int32) *int32 { return &v }
func boolp(v bool) *bool    { return &v }

// getSecret returns the per-user secret, or nil if it does not exist.
func (k *K8s) getSecret(ctx context.Context, id string) (*corev1.Secret, error) {
	s, err := k.cs.CoreV1().Secrets(k.cfg.UsersNS).Get(ctx, secretName(id), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	return s, err
}

// createUserResources provisions Secret, PVC, Deployment (replicas 0) and Service.
// It is idempotent: AlreadyExists is treated as success.
func (k *K8s) createUserResources(ctx context.Context, id, email, passwordHash, gatewayToken string) error {
	labels := userLabels(id)

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: secretName(id), Labels: labels,
			Annotations: map[string]string{annEmail: email},
		},
		StringData: map[string]string{
			"password-hash": passwordHash,
			"gateway-token": gatewayToken,
		},
	}
	if err := create(k.cs.CoreV1().Secrets(k.cfg.UsersNS).Create, ctx, secret); err != nil {
		return fmt.Errorf("secret: %w", err)
	}

	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: pvcName(id), Labels: labels},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("2Gi")},
			},
		},
	}
	if err := create(k.cs.CoreV1().PersistentVolumeClaims(k.cfg.UsersNS).Create, ctx, pvc); err != nil {
		return fmt.Errorf("pvc: %w", err)
	}

	dep := k.deploymentTemplate(id)
	if err := create(k.cs.AppsV1().Deployments(k.cfg.UsersNS).Create, ctx, dep); err != nil {
		return fmt.Errorf("deployment: %w", err)
	}

	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: serviceName(id), Labels: labels},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{labelUser: id},
			Ports: []corev1.ServicePort{
				{Name: "gateway", Port: 18789, TargetPort: intstr.FromInt(18789)},
				{Name: "tg-webhook", Port: 8787, TargetPort: intstr.FromInt(8787)},
			},
		},
	}
	if err := create(k.cs.CoreV1().Services(k.cfg.UsersNS).Create, ctx, svc); err != nil {
		return fmt.Errorf("service: %w", err)
	}
	return nil
}

// create is a tiny generic helper that swallows AlreadyExists.
func create[T any](fn func(context.Context, T, metav1.CreateOptions) (T, error), ctx context.Context, obj T) error {
	_, err := fn(ctx, obj, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return nil
	}
	return err
}

func (k *K8s) deploymentTemplate(id string) *appsv1.Deployment {
	labels := userLabels(id)
	onboard := `set -e
cd /app
if [ "$(node openclaw.mjs config get gateway.mode 2>/dev/null)" = "local" ]; then
  echo "already configured"; exit 0
fi
cat /shared/` + sharedKeyField + ` | node openclaw.mjs models auth paste-api-key --provider anthropic --profile-id anthropic:manual
node openclaw.mjs models set "$DEFAULT_MODEL"
node openclaw.mjs config set gateway.mode local
node openclaw.mjs config set gateway.controlUi.allowedOrigins "[\"$PUBLIC_ORIGIN\"]" --strict-json
# Token-only Control UI auth (no per-browser device pairing). Safe here: the control
# plane authenticates each user and supplies the per-user token, pods are single-tenant
# and network-isolated. Device-pairing approval is unreachable from the control plane
# (the in-pod CLI can't see the live gateway's pending requests), so this is the only
# workable option for self-serve.
node openclaw.mjs config set gateway.controlUi.dangerouslyDisableDeviceAuth true
# Disable the in-pod cron scheduler: the control plane is the sole cron driver
# (it wakes the pod and force-runs due jobs), which is what makes cron work with
# scale-to-zero without missed or double runs.
node openclaw.mjs config set cron.enabled false
echo "onboarded"`

	env := []corev1.EnvVar{
		{Name: "PUBLIC_ORIGIN", Value: k.cfg.PublicOrigin},
		{Name: "DEFAULT_MODEL", Value: k.cfg.DefaultModel},
		{Name: "OPENCLAW_STATE_DIR", Value: "/home/node/.openclaw"},
	}
	stateMount := corev1.VolumeMount{Name: "state", MountPath: "/home/node/.openclaw"}
	tmpMount := corev1.VolumeMount{Name: "tmp", MountPath: "/tmp"}

	// The gateway requires auth to bind to a non-loopback address. Inject the
	// per-user token; the activating proxy presents the same value as a Bearer header.
	gatewayEnv := append([]corev1.EnvVar{{
		Name: "OPENCLAW_GATEWAY_TOKEN",
		ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: secretName(id)},
			Key:                  "gateway-token",
		}},
	}}, env...)

	hardened := &corev1.SecurityContext{
		AllowPrivilegeEscalation: boolp(false),
		RunAsNonRoot:             boolp(true),
		RunAsUser:                int64p(1000),
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
	}

	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name: deployName(id), Labels: labels,
			Annotations: map[string]string{annLastActive: time.Now().UTC().Format(time.RFC3339)},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: int32p(0),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{labelUser: id}},
			Strategy: appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					SecurityContext: &corev1.PodSecurityContext{FSGroup: int64p(1000)},
					InitContainers: []corev1.Container{{
						Name:            "onboard",
						Image:           k.cfg.OpenclawImage,
						WorkingDir:      "/app",
						Command:         []string{"/bin/sh", "-c", onboard},
						Env:             env,
						VolumeMounts:    []corev1.VolumeMount{stateMount, {Name: "shared", MountPath: "/shared", ReadOnly: true}, tmpMount},
						SecurityContext: hardened,
					}},
					Containers: []corev1.Container{{
						Name:            "gateway",
						Image:           k.cfg.OpenclawImage,
						WorkingDir:      "/app",
						Command:         []string{"node", "openclaw.mjs", "gateway", "--bind", "lan", "--port", "18789"},
						Env:             gatewayEnv,
						Ports:           []corev1.ContainerPort{{Name: "gateway", ContainerPort: 18789}},
						VolumeMounts:    []corev1.VolumeMount{stateMount, tmpMount},
						SecurityContext: hardened,
						ReadinessProbe: &corev1.Probe{
							ProbeHandler:        corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/healthz", Port: intstr.FromInt(18789)}},
							InitialDelaySeconds: 3, PeriodSeconds: 3,
						},
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("256Mi")},
							Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("1Gi")},
						},
					}},
					Volumes: []corev1.Volume{
						{Name: "state", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: pvcName(id)}}},
						{Name: "shared", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: k.cfg.SharedKeySecret}}},
						{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
					},
				},
			},
		},
	}
}

func int64p(v int64) *int64 { return &v }

// scaleTo sets the user Deployment replica count.
func (k *K8s) scaleTo(ctx context.Context, id string, replicas int32) error {
	patch := fmt.Sprintf(`{"spec":{"replicas":%d}}`, replicas)
	_, err := k.cs.AppsV1().Deployments(k.cfg.UsersNS).Patch(
		ctx, deployName(id), types.StrategicMergePatchType, []byte(patch), metav1.PatchOptions{})
	return err
}

// touchActivity stamps the last-activity annotation with the current time.
func (k *K8s) touchActivity(ctx context.Context, id string) error {
	patch := fmt.Sprintf(`{"metadata":{"annotations":{%q:%q}}}`, annLastActive, time.Now().UTC().Format(time.RFC3339))
	_, err := k.cs.AppsV1().Deployments(k.cfg.UsersNS).Patch(
		ctx, deployName(id), types.MergePatchType, []byte(patch), metav1.PatchOptions{})
	return err
}

// ready reports whether the user Deployment has at least one ready replica.
func (k *K8s) ready(ctx context.Context, id string) (bool, error) {
	d, err := k.cs.AppsV1().Deployments(k.cfg.UsersNS).Get(ctx, deployName(id), metav1.GetOptions{})
	if err != nil {
		return false, err
	}
	return d.Status.ReadyReplicas >= 1, nil
}

// listManagedDeployments returns all control-plane-managed user Deployments.
func (k *K8s) listManagedDeployments(ctx context.Context) ([]appsv1.Deployment, error) {
	l, err := k.cs.AppsV1().Deployments(k.cfg.UsersNS).List(ctx, metav1.ListOptions{
		LabelSelector: labelManagedBy + "=" + managedByValue,
	})
	if err != nil {
		return nil, err
	}
	return l.Items, nil
}
