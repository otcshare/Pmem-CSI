package deployment

import (
	"fmt"
	"strings"

	pmemcsiv1alpha1 "github.com/intel/pmem-csi/pkg/apis/pmemcsi/v1alpha1"
	"github.com/intel/pmem-csi/pkg/pmem-csi-operator/utils"
	appsv1 "k8s.io/api/apps/v1"
	certv1beta1 "k8s.io/api/certificates/v1beta1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog"
)

const (
	controllerServicePort = 10000
	nodeControllerPort    = 10001
)

type PmemCSIDriver struct {
	*pmemcsiv1alpha1.Deployment
}

// Reconcile reconciles the driver deployment
func (d *PmemCSIDriver) Reconcile(r *ReconcileDeployment) (bool, error) {
	klog.Infof("Deployment name: %q, state: %q", d.ObjectMeta.Name, d.Status.Phase)
	switch d.Status.Phase {
	case pmemcsiv1alpha1.DeploymentPhaseNew, pmemcsiv1alpha1.DeploymentPhaseFailed:
		for _, dep := range r.deployments {
			if dep.Spec.DriverName == d.Spec.DriverName {
				d.Status.Phase = pmemcsiv1alpha1.DeploymentPhaseFailed
				return true, fmt.Errorf("driver name %q is already taken by deployment %q in namespace %q",
					d.Spec.DriverName, dep.Name, dep.Namespace)
			}
		}

		nsn := types.NamespacedName{
			Name:      d.Name,
			Namespace: d.Namespace,
		}
		r.deployments[nsn] = d.Deployment

		if err := d.initiateCertificateRequests(r); err != nil {
			return true, err
		}
		d.Status.Phase = pmemcsiv1alpha1.DeploymentPhasePending

	case pmemcsiv1alpha1.DeploymentPhasePending:
		ok, err := d.ensureCertificates(r)
		if err != nil {
			return true, err
		}
		if ok {
			d.Status.Phase = pmemcsiv1alpha1.DeploymentPhaseInitializing
		}

	case pmemcsiv1alpha1.DeploymentPhaseInitializing:
		if err := d.deployObjects(r); err != nil {
			return true, err
		}

		d.Status.Phase = pmemcsiv1alpha1.DeploymentPhaseRunning
		// Deployment successfull, so no more reconcile needed for this deployment
		return false, nil

	}
	return true, nil
}

func (d *PmemCSIDriver) initiateCertificateRequests(r *ReconcileDeployment) error {
	registryCsr, err := utils.NewCSR("pmem-registry", nil)
	if err != nil {
		return err
	}
	nodeControllerCsr, err := utils.NewCSR("pmem-node-controller", nil)
	if err != nil {
		return err
	}

	objects := []runtime.Object{
		d.getCSR(registryCsr),
		d.getCSR(nodeControllerCsr),
		d.getSecret(registryCsr),
		d.getSecret(nodeControllerCsr),
	}

	for _, obj := range objects {
		if err := r.Create(obj); err != nil {
			return err
		}
	}

	return nil
}

// ensureCertificates ensures the required CSRs are approved and the secrets
// gets updated with the tls certificate information
// Returns 'true' if certificates are ready, otherwise 'false' with error if any
func (d *PmemCSIDriver) ensureCertificates(r *ReconcileDeployment) (bool, error) {
	for _, csrName := range []string{"pmem-registry", "pmem-node-controller"} {
		secret := &corev1.Secret{
			TypeMeta: metav1.TypeMeta{
				Kind:       "Secret",
				APIVersion: "v1",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name:      d.Name + "-" + csrName,
				Namespace: d.Namespace,
			},
		}
		if err := r.Get(secret); err != nil {
			klog.Errorf("Failed to get secret %q: %v", csrName, err)
			return false, err
		}
		if len(secret.Data[corev1.TLSCertKey]) == 0 {
			csrObjectName := d.Name + "-" + d.Namespace + "-" + csrName
			csr := &certv1beta1.CertificateSigningRequest{
				TypeMeta: metav1.TypeMeta{
					Kind:       "certificates.k8s.io",
					APIVersion: "v1beta1",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name: csrObjectName,
				},
			}
			if err := r.Get(csr); err != nil {
				klog.Errorf("Failed to get certificate signing request %q: %v", csrObjectName, err)
				return false, err
			}
			approved := false
			for _, c := range csr.Status.Conditions {
				if c.Type == certv1beta1.CertificateApproved {
					approved = true
				}
			}
			if !approved {
				return false, nil
			}
			if len(csr.Status.Certificate) == 0 {
				// Certificate not yet ready, reconcile
				return false, nil
			}

			secret.Data[corev1.TLSCertKey] = csr.Status.Certificate
			if err := r.Update(secret); err != nil {
				return false, err
			}
		}
	}

	return true, nil
}

func (d *PmemCSIDriver) deployObjects(r *ReconcileDeployment) error {
	for _, obj := range d.getDeploymentObjects() {
		if err := r.Create(obj); err != nil {
			return err
		}
	}
	return nil
}

func (d *PmemCSIDriver) getDeploymentObjects() []runtime.Object {
	return []runtime.Object{
		d.getControllerServiceAccount(),
		d.getControllerProvisionerRole(),
		d.getControllerProvisionerRoleBinding(),
		d.getControllerProvisionerClusterRole(),
		d.getControllerProvisionerClusterRoleBinding(),
		d.getControllerService(),
		d.getControllerStatefulSet(),
		d.getNodeDaemonSet(),
	}
}

func (d *PmemCSIDriver) getOwnerReference() metav1.OwnerReference {
	blockOwnerDeletion := true
	isController := true
	return metav1.OwnerReference{
		APIVersion:         d.APIVersion,
		Kind:               d.Kind,
		Name:               d.GetName(),
		UID:                d.GetUID(),
		BlockOwnerDeletion: &blockOwnerDeletion,
		Controller:         &isController,
	}
}

func (d *PmemCSIDriver) getCSR(csr *utils.CSR) *certv1beta1.CertificateSigningRequest {
	return &certv1beta1.CertificateSigningRequest{
		TypeMeta: metav1.TypeMeta{
			Kind:       "certificates.k8s.io",
			APIVersion: "v1beta1",
		},
		ObjectMeta: metav1.ObjectMeta{
			// CSR is a cluster level object, hence use deployment name and namespace as
			// object name to make it unique
			Name: d.Name + "-" + d.Namespace + "-" + csr.CommonName(),
			OwnerReferences: []metav1.OwnerReference{
				d.getOwnerReference(),
			},
		},
		Spec: certv1beta1.CertificateSigningRequestSpec{
			Groups:  []string{"system:authenticated"},
			Request: csr.Encoded(),
			Usages: []certv1beta1.KeyUsage{
				certv1beta1.UsageServerAuth,
				certv1beta1.UsageClientAuth,
			},
		},
	}
}

func (d *PmemCSIDriver) getSecret(csr *utils.CSR) *corev1.Secret {
	return &corev1.Secret{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Secret",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      d.Name + "-" + csr.CommonName(),
			Namespace: d.Namespace,
			OwnerReferences: []metav1.OwnerReference{
				d.getOwnerReference(),
			},
		},
		Type: corev1.SecretTypeTLS,
		Data: map[string][]byte{
			corev1.TLSPrivateKeyKey: csr.EncodePrivateKey(),
			// This should be filled once the corresponding CSR is approved
			corev1.TLSCertKey: []byte{},
		},
	}
}

func (d *PmemCSIDriver) getControllerService() *corev1.Service {
	return &corev1.Service{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Service",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      d.Name,
			Namespace: d.Namespace,
			OwnerReferences: []metav1.OwnerReference{
				d.getOwnerReference(),
			},
		},
		Spec: corev1.ServiceSpec{
			Type: corev1.ServiceTypeClusterIP,
			Ports: []corev1.ServicePort{
				corev1.ServicePort{
					Port: controllerServicePort,
				},
			},
			Selector: map[string]string{
				"app": "pmem-csi-controller",
			},
		},
	}
}

func (d *PmemCSIDriver) getControllerServiceAccount() *corev1.ServiceAccount {
	return &corev1.ServiceAccount{
		TypeMeta: metav1.TypeMeta{
			Kind:       "ServiceAccount",
			APIVersion: "rbac.authorization.k8s.io/v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      d.Name,
			Namespace: d.Namespace,
			OwnerReferences: []metav1.OwnerReference{
				d.getOwnerReference(),
			},
		},
	}
}

func (d *PmemCSIDriver) getControllerProvisionerRole() *rbacv1.Role {
	return &rbacv1.Role{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Role",
			APIVersion: "rbac.authorization.k8s.io/v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      d.Name,
			Namespace: d.Namespace,
			OwnerReferences: []metav1.OwnerReference{
				d.getOwnerReference(),
			},
		},
		Rules: []rbacv1.PolicyRule{
			rbacv1.PolicyRule{
				APIGroups: []string{""},
				Resources: []string{"endpoints"},
				Verbs: []string{
					"get", "watch", "list", "delete", "update", "create",
				},
			},
			rbacv1.PolicyRule{
				APIGroups: []string{"coordination.k8s.io"},
				Resources: []string{"leases"},
				Verbs: []string{
					"get", "watch", "list", "delete", "update", "create",
				},
			},
		},
	}
}

func (d *PmemCSIDriver) getControllerProvisionerRoleBinding() *rbacv1.RoleBinding {
	return &rbacv1.RoleBinding{
		TypeMeta: metav1.TypeMeta{
			Kind:       "RoleBinding",
			APIVersion: "rbac.authorization.k8s.io/v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      d.Name,
			Namespace: d.Namespace,
			OwnerReferences: []metav1.OwnerReference{
				d.getOwnerReference(),
			},
		},
		Subjects: []rbacv1.Subject{
			{
				Kind:      "ServiceAccount",
				Name:      d.Name,
				Namespace: d.Namespace,
			},
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "Role",
			Name:     d.Name,
		},
	}
}

func (d *PmemCSIDriver) getControllerProvisionerClusterRole() *rbacv1.ClusterRole {
	return &rbacv1.ClusterRole{
		TypeMeta: metav1.TypeMeta{
			Kind:       "ClusterRole",
			APIVersion: "rbac.authorization.k8s.io/v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			// ClusterRole is a cluster level object, hence use deployment name and namespace as
			// object name to make it unique
			Name: d.Name + "-" + d.Namespace,
			OwnerReferences: []metav1.OwnerReference{
				d.getOwnerReference(),
			},
		},
		Rules: []rbacv1.PolicyRule{
			rbacv1.PolicyRule{
				APIGroups: []string{""},
				Resources: []string{"persistentvolumes"},
				Verbs: []string{
					"get", "watch", "list", "delete", "create",
				},
			},
			rbacv1.PolicyRule{
				APIGroups: []string{""},
				Resources: []string{"persistentvolumeclaims"},
				Verbs: []string{
					"get", "watch", "list", "update",
				},
			},
			rbacv1.PolicyRule{
				APIGroups: []string{"storage.k8s.io"},
				Resources: []string{"storageclasses"},
				Verbs: []string{
					"get", "watch", "list",
				},
			},
			rbacv1.PolicyRule{
				APIGroups: []string{""},
				Resources: []string{"events"},
				Verbs: []string{
					"watch", "list", "create", "update", "patch",
				},
			},
			rbacv1.PolicyRule{
				APIGroups: []string{"snapshot.storage.k8s.io"},
				Resources: []string{"volumesnapshots", "volumesnapshotcontents"},
				Verbs: []string{
					"get", "list",
				},
			},
			rbacv1.PolicyRule{
				APIGroups: []string{"storage.k8s.io"},
				Resources: []string{"csinodes"},
				Verbs: []string{
					"get", "list", "watch",
				},
			},
			rbacv1.PolicyRule{
				APIGroups: []string{""},
				Resources: []string{"nodes"},
				Verbs: []string{
					"get", "list", "watch",
				},
			},
		},
	}
}

func (d *PmemCSIDriver) getControllerProvisionerClusterRoleBinding() *rbacv1.ClusterRoleBinding {
	return &rbacv1.ClusterRoleBinding{
		TypeMeta: metav1.TypeMeta{
			Kind:       "ClusterRoleBinding",
			APIVersion: "rbac.authorization.k8s.io/v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			// ClusterRoleBinding is a cluster level object, hence use deployment
			// name and namespace as object name to make it unique
			Name: d.Name + "-" + d.Namespace,
			OwnerReferences: []metav1.OwnerReference{
				d.getOwnerReference(),
			},
		},
		Subjects: []rbacv1.Subject{
			{
				Kind:      "ServiceAccount",
				Name:      d.Name,
				Namespace: d.Namespace,
			},
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "ClusterRole",
			Name:     d.Name + "-" + d.Namespace,
		},
	}
}

func (d *PmemCSIDriver) getControllerStatefulSet() *appsv1.StatefulSet {
	replicas := int32(1)
	ss := &appsv1.StatefulSet{
		TypeMeta: metav1.TypeMeta{
			Kind:       "StatefulSet",
			APIVersion: "apps/v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      d.Name + "-controller",
			Namespace: d.Namespace,
			OwnerReferences: []metav1.OwnerReference{
				d.getOwnerReference(),
			},
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app": "pmem-csi-controller",
				},
			},
			ServiceName: d.Name,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app": "pmem-csi-controller",
					},
				},
				Spec: corev1.PodSpec{
					ServiceAccountName: d.Name,
					Containers: []corev1.Container{
						d.getControllerContainer(),
						d.getProvisionerContainer(),
					},
					Volumes: []corev1.Volume{
						{
							Name: "plugin-socket-dir",
							VolumeSource: corev1.VolumeSource{
								EmptyDir: &corev1.EmptyDirVolumeSource{},
							},
						},
						{
							Name: "registry-cert",
							VolumeSource: corev1.VolumeSource{
								Secret: &corev1.SecretVolumeSource{
									SecretName: d.Name + "-pmem-registry",
									Items: []corev1.KeyToPath{
										{
											Key:  "tls.crt",
											Path: "pmem-csi-registry.crt",
										},
										{
											Key:  "tls.key",
											Path: "pmem-csi-registry.key",
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}

	return ss
}

func (d *PmemCSIDriver) getNodeDaemonSet() *appsv1.DaemonSet {
	directoryOrCreate := corev1.HostPathDirectoryOrCreate
	ds := &appsv1.DaemonSet{
		TypeMeta: metav1.TypeMeta{
			Kind:       "DaemonSet",
			APIVersion: "apps/v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      d.Name + "-node",
			Namespace: d.Namespace,
			OwnerReferences: []metav1.OwnerReference{
				d.getOwnerReference(),
			},
		},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app": "pmem-csi-node",
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app": "pmem-csi-node",
					},
				},
				Spec: corev1.PodSpec{
					NodeSelector: map[string]string{
						"storage": "pmem",
					},
					HostNetwork: true,
					Containers: []corev1.Container{
						d.getNodeDriverContainer(),
						d.getNodeRegistrarContainer(),
					},
					Volumes: []corev1.Volume{
						{
							Name: "registration-dir",
							VolumeSource: corev1.VolumeSource{
								HostPath: &corev1.HostPathVolumeSource{
									Path: "/var/lib/kubelet/plugins_registry/",
									Type: &directoryOrCreate,
								},
							},
						},
						{
							Name: "mountpoint-dir",
							VolumeSource: corev1.VolumeSource{
								HostPath: &corev1.HostPathVolumeSource{
									Path: "/var/lib/kubelet/plugins/kubernetes.io/csi",
									Type: &directoryOrCreate,
								},
							},
						},
						{
							Name: "pods-dir",
							VolumeSource: corev1.VolumeSource{
								HostPath: &corev1.HostPathVolumeSource{
									Path: "/var/lib/kubelet/pods",
									Type: &directoryOrCreate,
								},
							},
						},
						{
							Name: "pmem-state-dir",
							VolumeSource: corev1.VolumeSource{
								HostPath: &corev1.HostPathVolumeSource{
									Path: "/var/lib/" + d.Spec.DriverName,
									Type: &directoryOrCreate,
								},
							},
						},
						{
							Name: "sys-dir",
							VolumeSource: corev1.VolumeSource{
								HostPath: &corev1.HostPathVolumeSource{
									Path: "/sys",
									Type: &directoryOrCreate,
								},
							},
						},
						{
							Name: "dev-dir",
							VolumeSource: corev1.VolumeSource{
								HostPath: &corev1.HostPathVolumeSource{
									Path: "/dev",
									Type: &directoryOrCreate,
								},
							},
						},
						{
							Name: "controller-cert",
							VolumeSource: corev1.VolumeSource{
								Secret: &corev1.SecretVolumeSource{
									SecretName: d.Name + "-pmem-node-controller",
									Items: []corev1.KeyToPath{
										{
											Key:  "tls.crt",
											Path: "pmem-csi-node-controller.crt",
										},
										{
											Key:  "tls.key",
											Path: "pmem-csi-node-controller.key",
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}

	if d.Spec.DeviceMode == "lvm" {
		ds.Spec.Template.Spec.InitContainers = []corev1.Container{
			d.getNamespaceInitContainer(),
			d.getVolumeGroupInitContainer(),
		}
	}

	return ds
}

func (d *PmemCSIDriver) getControllerArgs() []string {
	args := []string{
		fmt.Sprintf("-v=%d", d.Spec.LogLevel),
		"-mode=controller",
		"-drivername=" + d.Spec.DriverName,
		"-endpoint=unix:///csi/csi-controller.sock",
		fmt.Sprintf("-registryEndpoint=tcp://0.0.0.0:%d", controllerServicePort),
		"-nodeid=$(KUBE_NODE_NAME)",
		"-caFile=/var/run/secrets/kubernetes.io/serviceaccount/ca.crt",
		"-certFile=/certs/pmem-csi-registry.crt",
		"-keyFile=/certs/pmem-csi-registry.key",
	}

	return args
}

func (d *PmemCSIDriver) getNodeDriverArgs() []string {
	// Form service port environment variable from Service name
	// In our case Service name is deployment name
	// Ref :- k8s.io/kubernetes/pkg/kubelet/envvars/envvars.go
	pmemServiceEndpointEnv := fmt.Sprintf(strings.ToUpper(strings.Replace(d.Name, "-", "_", -1))+"_PORT_%d_TCP", controllerServicePort)
	args := []string{
		fmt.Sprintf("-deviceManager=%s", d.Spec.DeviceMode),
		fmt.Sprintf("-v=%d", d.Spec.LogLevel),
		"-drivername=" + d.Spec.DriverName,
		"-mode=node",
		"-endpoint=unix:///var/lib/" + d.Spec.DriverName + "/csi.sock",
		"-nodeid=$(KUBE_NODE_NAME)",
		fmt.Sprintf("-controllerEndpoint=tcp://$(KUBE_POD_IP):%d", nodeControllerPort),
		fmt.Sprintf("-registryEndpoint=" + "$(" + pmemServiceEndpointEnv + ")"),
		"-caFile=/var/run/secrets/kubernetes.io/serviceaccount/ca.crt",
		"-statePath=/var/lib/" + d.Spec.DriverName,
		"-certFile=/certs/pmem-csi-node-controller.crt",
		"-keyFile=/certs/pmem-csi-node-controller.key",
	}

	return args
}

func (d *PmemCSIDriver) getControllerContainer() corev1.Container {
	return corev1.Container{
		Name:            "pmem-driver",
		Image:           d.Spec.Image,
		ImagePullPolicy: d.Spec.PullPolicy,
		Args:            d.getControllerArgs(),
		Env: []corev1.EnvVar{
			{
				Name: "KUBE_NODE_NAME",
				ValueFrom: &corev1.EnvVarSource{
					FieldRef: &corev1.ObjectFieldSelector{
						APIVersion: "v1",
						FieldPath:  "spec.nodeName",
					},
				},
			},
			{
				Name:  "TERMINATION_LOG_PATH",
				Value: "/tmp/termination-log",
			},
		},
		VolumeMounts: []corev1.VolumeMount{
			{
				Name:      "registry-cert",
				MountPath: "/certs",
			},
			{
				Name:      "plugin-socket-dir",
				MountPath: "/csi",
			},
		},
		Resources: *d.Spec.ControllerResources,
	}
}

func (d *PmemCSIDriver) getNodeDriverContainer() corev1.Container {
	bidirectional := corev1.MountPropagationBidirectional
	true := true
	return corev1.Container{
		Name:            "pmem-driver",
		Image:           d.Spec.Image,
		ImagePullPolicy: d.Spec.PullPolicy,
		Args:            d.getNodeDriverArgs(),
		Env: []corev1.EnvVar{
			{
				Name: "KUBE_NODE_NAME",
				ValueFrom: &corev1.EnvVarSource{
					FieldRef: &corev1.ObjectFieldSelector{
						APIVersion: "v1",
						FieldPath:  "spec.nodeName",
					},
				},
			},
			{
				Name: "KUBE_POD_IP",
				ValueFrom: &corev1.EnvVarSource{
					FieldRef: &corev1.ObjectFieldSelector{
						APIVersion: "v1",
						FieldPath:  "status.podIP",
					},
				},
			},
			{
				Name:  "TERMINATION_LOG_PATH",
				Value: "/tmp/termination-log",
			},
		},
		VolumeMounts: []corev1.VolumeMount{
			{
				Name:             "mountpoint-dir",
				MountPath:        "/var/lib/kubelet/plugins/kubernetes.io/csi",
				MountPropagation: &bidirectional,
			},
			{
				Name:             "pods-dir",
				MountPath:        "/var/lib/kubelet/pods",
				MountPropagation: &bidirectional,
			},
			{
				Name:      "controller-cert",
				MountPath: "/certs",
			},
			{
				Name:      "pmem-state-dir",
				MountPath: "/var/lib/" + d.Spec.DriverName,
			},
			{
				Name:      "dev-dir",
				MountPath: "/dev",
			},
		},
		Resources: *d.Spec.NodeResources,
		SecurityContext: &corev1.SecurityContext{
			Privileged: &true,
		},
	}
}

func (d *PmemCSIDriver) getProvisionerContainer() corev1.Container {
	return corev1.Container{
		Name:            "provisioner",
		Image:           d.Spec.ProvisionerImage,
		ImagePullPolicy: d.Spec.PullPolicy,
		Args: []string{
			"--timeout=5m",
			fmt.Sprintf("--v=%d", d.Spec.LogLevel),
			"--csi-address=/csi/csi-controller.sock",
			"--feature-gates=Topology=true",
			"--strict-topology=true",
		},
		VolumeMounts: []corev1.VolumeMount{
			{
				Name:      "plugin-socket-dir",
				MountPath: "/csi",
			},
		},
		Resources: *d.Spec.ControllerResources,
	}
}

func (d *PmemCSIDriver) getNamespaceInitContainer() corev1.Container {
	true := true
	return corev1.Container{
		Name:            "pmem-ns-init",
		Image:           d.Spec.Image,
		ImagePullPolicy: d.Spec.PullPolicy,
		Command: []string{
			"/usr/local/bin/pmem-ns-init",
		},
		Args: []string{
			fmt.Sprintf("--v=%d", d.Spec.LogLevel),
		},
		Env: []corev1.EnvVar{
			{
				Name:  "TERMINATION_LOG_PATH",
				Value: "/tmp/pmem-ns-init-termination-log",
			},
		},
		VolumeMounts: []corev1.VolumeMount{
			{
				Name:      "sys-dir",
				MountPath: "/sys",
			},
		},
		Resources: *d.Spec.NodeResources,
		SecurityContext: &corev1.SecurityContext{
			Privileged: &true,
		},
	}
}

func (d *PmemCSIDriver) getVolumeGroupInitContainer() corev1.Container {
	true := true
	return corev1.Container{
		Name:            "pmem-vgm",
		Image:           d.Spec.Image,
		ImagePullPolicy: d.Spec.PullPolicy,
		Command: []string{
			"/usr/local/bin/pmem-vgm",
		},
		Args: []string{
			fmt.Sprintf("--v=%d", d.Spec.LogLevel),
		},
		Env: []corev1.EnvVar{
			{
				Name:  "TERMINATION_LOG_PATH",
				Value: "/tmp/pmem-vgm-termination-log",
			},
		},
		Resources: *d.Spec.NodeResources,
		SecurityContext: &corev1.SecurityContext{
			Privileged: &true,
		},
	}
}

func (d *PmemCSIDriver) getNodeRegistrarContainer() corev1.Container {
	return corev1.Container{
		Name:            "driver-registrar",
		Image:           d.Spec.NodeRegistrarImage,
		ImagePullPolicy: d.Spec.PullPolicy,
		Args: []string{
			fmt.Sprintf("--v=%d", d.Spec.LogLevel),
			fmt.Sprintf("--kubelet-registration-path=/var/lib/%s/csi.sock", d.Spec.DriverName),
			"--csi-address=/pmem-csi/csi.sock",
		},
		VolumeMounts: []corev1.VolumeMount{
			{
				Name:      "pmem-state-dir",
				MountPath: "/pmem-csi",
			},
			{
				Name:      "registration-dir",
				MountPath: "/registration",
			},
		},
		Resources: *d.Spec.NodeResources,
	}
}
