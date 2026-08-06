// Package k8sbackend 是文档「四、核心设计：Agent 实例的编排」的生产路径实现：
// 每用户一个 StatefulSet（replicas 0/1）+ Headless Service + PVC。
//
// 与本地 process/docker 后端实现同一 InstanceBackend 语义：
//   - Create = 创建 StatefulSet + Headless Service，等 Pod Ready；
//   - Suspend = replicas 1 -> 0（Pod 销毁，PVC 保留）；
//   - Wake    = replicas 0 -> 1（新 Pod 挂回同一 PVC，数据无缝恢复）；
//   - Delete  = 删除 StatefulSet + （可选）PVC + 映射记录。
//
// 模型配置注入仍由控制面在 Pod Ready 后通过 HTTP 完成
// （Pod DNS: agent-<userID>-0.<svc>.<ns>.svc.cluster.local:18585），
// 凭证不写入 PVC（文档 6.2）。
package k8sbackend

import (
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"

	"cloude-agent/internal/backend"
)

const (
	agentPort       = 18585
	workspaceMount  = "/workspace"
	reconcileTTL    = 3 * time.Minute
	stsAPIVersion   = "apps/v1"
	headlessSvcName = "agent-svc"
)

// Config 是 K8s 后端的集群相关配置。
type Config struct {
	Namespace        string // 数据面命名空间（与文档 RBAC 划分一致）
	Image            string // Agent 实例镜像
	StorageClassName string // 可选；空则用默认 StorageClass（Longhorn/local-path）
	RemovePVCOnDelete bool  // 删除实例时是否同时删除 PVC（对应文档 Delete 语义）
}

// Backend 实现 backend.InstanceBackend（见主模块接口）。
type Backend struct {
	client kubernetes.Interface
	cfg    Config
}

func New(client kubernetes.Interface, cfg Config) (*Backend, error) {
	if cfg.Namespace == "" {
		cfg.Namespace = "cloude-agent"
	}
	if cfg.Image == "" {
		return nil, fmt.Errorf("k8sbackend: Image 未配置")
	}
	return &Backend{client: client, cfg: cfg}, nil
}

func (b *Backend) Name() string { return "k8s-statefulset" }

func (b *Backend) stsName(userID string) string { return "agent-" + userID }

func (b *Backend) pvcName(userID string) string {
	return fmt.Sprintf("workspace-%s-0", b.stsName(userID)) // StatefulSet 默认卷命名规则
}

// Endpoint 对应文档 5.1：控制面按 userID 直接拼出稳定 Pod DNS。
func (b *Backend) Endpoint(userID string) string {
	return fmt.Sprintf("http://%s-0.%s.%s.svc.cluster.local:%d",
		b.stsName(userID), headlessSvcName, b.cfg.Namespace, agentPort)
}

func (b *Backend) Create(ctx context.Context, userID string) (*backend.Info, error) {
	if err := b.ensureHeadlessService(ctx); err != nil {
		return nil, err
	}
	sts := b.buildStatefulSet(userID, 1)
	if _, err := b.client.AppsV1().StatefulSets(b.cfg.Namespace).Create(ctx, sts, metav1.CreateOptions{}); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return nil, fmt.Errorf("create statefulset: %w", err)
		}
	}
	if err := b.waitReady(ctx, userID); err != nil {
		return nil, err
	}
	return b.info(userID), nil
}

func (b *Backend) Start(ctx context.Context, userID string) (*backend.Info, error) {
	sts, err := b.client.AppsV1().StatefulSets(b.cfg.Namespace).Get(ctx, b.stsName(userID), metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	if sts.Spec.Replicas == nil || *sts.Spec.Replicas == 0 {
		one := int32(1)
		sts.Spec.Replicas = &one
		if _, err := b.client.AppsV1().StatefulSets(b.cfg.Namespace).Update(ctx, sts, metav1.UpdateOptions{}); err != nil {
			return nil, err
		}
	}
	if err := b.waitReady(ctx, userID); err != nil {
		return nil, err
	}
	return b.info(userID), nil
}

func (b *Backend) Stop(ctx context.Context, userID string) error {
	sts, err := b.client.AppsV1().StatefulSets(b.cfg.Namespace).Get(ctx, b.stsName(userID), metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	zero := int32(0)
	sts.Spec.Replicas = &zero
	_, err = b.client.AppsV1().StatefulSets(b.cfg.Namespace).Update(ctx, sts, metav1.UpdateOptions{})
	return err
}

func (b *Backend) Delete(ctx context.Context, userID string) error {
	propagation := metav1.DeletePropagationBackground
	if err := b.client.AppsV1().StatefulSets(b.cfg.Namespace).Delete(ctx, b.stsName(userID), metav1.DeleteOptions{PropagationPolicy: &propagation}); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	if b.cfg.RemovePVCOnDelete {
		if err := b.client.CoreV1().PersistentVolumeClaims(b.cfg.Namespace).Delete(ctx, b.pvcName(userID), metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

func (b *Backend) info(userID string) *backend.Info {
	return &backend.Info{Workspace: b.pvcName(userID), Endpoint: b.Endpoint(userID), Port: agentPort}
}

func (b *Backend) ensureHeadlessService(ctx context.Context) error {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      headlessSvcName,
			Namespace: b.cfg.Namespace,
			Labels:    map[string]string{"app": "cloude-agent", "role": "data-plane"},
		},
		Spec: corev1.ServiceSpec{
			ClusterIP: corev1.ClusterIPNone, // Headless：为每个 Pod 提供稳定 DNS
			Selector:  map[string]string{"app": "cloude-agent", "role": "data-plane"},
			Ports: []corev1.ServicePort{
				{Name: "agent", Port: agentPort, TargetPort: intstr.FromInt(agentPort)},
			},
		},
	}
	_, err := b.client.CoreV1().Services(b.cfg.Namespace).Create(ctx, svc, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

func (b *Backend) buildStatefulSet(userID string, replicas int32) *appsv1.StatefulSet {
	labels := map[string]string{"app": "cloude-agent", "role": "data-plane", "user-id": userID}
	runAsNonRoot := true
	allowPrivilegeEscalation := false
	readOnlyRootFS := true
	fsGroup := int64(1000)
	runAsUser := int64(1000)
	runAsGroup := int64(1000)
	terminationGrace := int64(30)
	replicasPtr := replicas

	pvcSpec := corev1.PersistentVolumeClaimSpec{AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}}
	if b.cfg.StorageClassName != "" {
		pvcSpec.StorageClassName = &b.cfg.StorageClassName
	}
	pvcSpec.Resources = corev1.VolumeResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("10Gi")},
	}

	return &appsv1.StatefulSet{
		TypeMeta:   metav1.TypeMeta{Kind: "StatefulSet", APIVersion: stsAPIVersion},
		ObjectMeta: metav1.ObjectMeta{Name: b.stsName(userID), Namespace: b.cfg.Namespace, Labels: labels},
		Spec: appsv1.StatefulSetSpec{
			Replicas:            &replicasPtr,
			ServiceName:         headlessSvcName,
			PodManagementPolicy: appsv1.OrderedReadyPodManagement,
			Selector:            &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					TerminationGracePeriodSeconds: &terminationGrace,
					SecurityContext: &corev1.PodSecurityContext{
						RunAsUser:    &runAsUser,
						RunAsGroup:   &runAsGroup,
						FSGroup:      &fsGroup,
						RunAsNonRoot: &runAsNonRoot,
					},
					Containers: []corev1.Container{{
						Name:            "agent",
						Image:           b.cfg.Image,
						ImagePullPolicy: corev1.PullIfNotPresent,
						Args:            []string{"--listen", "0.0.0.0:18585", "--workspace", workspaceMount},
						Ports:           []corev1.ContainerPort{{ContainerPort: agentPort, Name: "agent"}},
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: &allowPrivilegeEscalation,
							ReadOnlyRootFilesystem:    &readOnlyRootFS,
							Capabilities: &corev1.Capabilities{
								Drop: []corev1.Capability{"ALL"},
							},
						},
						VolumeMounts: []corev1.VolumeMount{{Name: "workspace", MountPath: workspaceMount}},
						ReadinessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								HTTPGet: &corev1.HTTPGetAction{Path: "/health", Port: intstr.FromInt(agentPort)},
							},
							InitialDelaySeconds: 3,
							PeriodSeconds:       5,
						},
						LivenessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								HTTPGet: &corev1.HTTPGetAction{Path: "/health", Port: intstr.FromInt(agentPort)},
							},
							InitialDelaySeconds: 10,
							PeriodSeconds:       15,
						},
					}},
				},
			},
			VolumeClaimTemplates: []corev1.PersistentVolumeClaim{{
				ObjectMeta: metav1.ObjectMeta{Name: "workspace", Labels: labels},
				Spec:       pvcSpec,
			}},
		},
	}
}

// waitReady 轮询 StatefulSet 就绪副本（对应文档「等 Pod Ready」）。
func (b *Backend) waitReady(ctx context.Context, userID string) error {
	deadline := time.Now().Add(reconcileTTL)
	for time.Now().Before(deadline) {
		sts, err := b.client.AppsV1().StatefulSets(b.cfg.Namespace).Get(ctx, b.stsName(userID), metav1.GetOptions{})
		if err == nil && sts.Status.ReadyReplicas >= 1 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return fmt.Errorf("statefulset %s 未在 %s 内就绪", b.stsName(userID), reconcileTTL)
}
