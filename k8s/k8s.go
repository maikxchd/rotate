package k8s

import (
	"context"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/labels"

	"github.com/aws/aws-sdk-go/aws"

	"k8s.io/apimachinery/pkg/fields"

	"github.com/pkg/errors"

	"code.justin.tv/safety/k8s-rot8/rollback"
	"github.com/hashicorp/go-multierror"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	policyv1beta1 "k8s.io/api/policy/v1beta1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/json"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	"k8s.io/client-go/kubernetes"
)

func FetchNodeByName(clientset kubernetes.Interface, name string) (*v1.Node, error) {
	return clientset.CoreV1().Nodes().Get(name, metav1.GetOptions{})
}

// WaitForNodes waits in a loop for either the context to cancel or for a node to go healthy
func WaitForNodes(ctx context.Context, clientset kubernetes.Interface, role string, expected int, waitPeriod time.Duration) error {
	for {
		nodes, err := clientset.CoreV1().Nodes().List(metav1.ListOptions{

			LabelSelector: labels.SelectorFromSet(
				labels.Set{fmt.Sprintf("node-role.kubernetes.io/%s", role): "true"}).String(),
		})

		if err != nil {
			return errors.Wrap(err, "Failed to get list to nodes")
		}

		var actual []v1.Node
		for _, node := range nodes.Items {
			if !node.Spec.Unschedulable {
				for _, cond := range node.Status.Conditions {
					if cond.Type == v1.NodeReady {
						actual = append(actual, node)
						break
					}
				}
			}
		}

		if len(actual) >= expected {
			return nil
		}

		select {
		case <-ctx.Done():
			return errors.New("node not found before context finished")
		case <-time.After(waitPeriod):
		}
	}
}

// DeleteOrEvictPod deletes or evicts a pod depending on what is supported
func DeleteOrEvictPod(clientset kubernetes.Interface, pod corev1.Pod, policyGroupVersion string, options EvictOptions) error {
	var gracePeriod *int64
	if options.GracePeriodSeconds == 0 {
		gracePeriod = aws.Int64(options.GracePeriodSeconds)
	}
	deleteOpts := &metav1.DeleteOptions{GracePeriodSeconds: gracePeriod}

	if policyGroupVersion == "" {
		return clientset.CoreV1().Pods(pod.Namespace).Delete(pod.Name, deleteOpts)
	} else {
		eviction := &policyv1beta1.Eviction{
			TypeMeta: metav1.TypeMeta{
				APIVersion: policyGroupVersion,
				Kind:       EvictionKind,
			},
			ObjectMeta: metav1.ObjectMeta{
				Name:      pod.Name,
				Namespace: pod.Namespace,
			},
			DeleteOptions: deleteOpts,
		}

		return clientset.PolicyV1beta1().Evictions(eviction.Namespace).Evict(eviction)
	}
}

type EvictOptions struct {
	IgnoreDaemonSets   bool
	GracePeriodSeconds int64
	FailOnDeleteError  bool
}

func PodsToEvict(clientset kubernetes.Interface, nodeName string, options EvictOptions, filters []Filter) ([]v1.Pod, error) {
	podList, err := clientset.CoreV1().Pods(metav1.NamespaceAll).List(metav1.ListOptions{
		FieldSelector: fields.SelectorFromSet(fields.Set{"spec.nodeName": nodeName}).String(),
	})
	if err != nil {
		return nil, errors.Wrapf(err, "node:%s error finding pods on node", nodeName)
	}

	var result []v1.Pod
	for i := range podList.Items {
		pod := podList.Items[i]
		doAdd := true
		for _, filter := range filters {
			doAdd = doAdd && filter(clientset, pod, options)
		}
		if doAdd {
			result = append(result, pod)
		}
	}

	return result, nil
}

type Filter func(kubernetes.Interface, v1.Pod, EvictOptions) bool

func FilterDaemonSet(clientset kubernetes.Interface, pod v1.Pod, options EvictOptions) bool {
	// Note that we return false in cases where the pod is DaemonSet managed,
	// regardless of flags.
	//
	// The exception is for pods that are orphaned (the referencing
	// management resource - including DaemonSet - is not found).
	controllerRef := metav1.GetControllerOf(&pod)
	if controllerRef == nil || controllerRef.Kind != appsv1.SchemeGroupVersion.WithKind("DaemonSet").Kind {
		return true
	}
	// Any finished pod can be removed.
	if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
		return true
	}

	if _, err := clientset.AppsV1().DaemonSets(pod.Namespace).Get(controllerRef.Name, metav1.GetOptions{}); err != nil {
		// remove orphaned pods
		if apierrors.IsNotFound(err) {
			return true
		}
	}

	if !options.IgnoreDaemonSets {
		return false
	}

	return true
}

const (
	// EvictionKind represents the kind of evictions object
	EvictionKind = "Eviction"
	// EvictionSubresource represents the kind of evictions object as pod's subresource
	EvictionSubresource = "pods/eviction"
)

// CheckEvictionSupport uses Discovery API to find out if the server support
// eviction subresource If support, it will return its groupVersion; Otherwise,
// it will return an empty string
func CheckEvictionSupport(clientset kubernetes.Interface) (string, error) {
	discoveryClient := clientset.Discovery()
	groupList, err := discoveryClient.ServerGroups()
	if err != nil {
		return "", err
	}
	foundPolicyGroup := false
	var policyGroupVersion string
	for _, group := range groupList.Groups {
		if group.Name == "policy" {
			foundPolicyGroup = true
			policyGroupVersion = group.PreferredVersion.GroupVersion
			break
		}
	}
	if !foundPolicyGroup {
		return "", nil
	}
	resourceList, err := discoveryClient.ServerResourcesForGroupVersion("v1")
	if err != nil {
		return "", err
	}
	for _, resource := range resourceList.APIResources {
		if resource.Name == EvictionSubresource && resource.Kind == EvictionKind {
			return policyGroupVersion, nil
		}
	}
	return "", nil
}

// Adapted from kubectl cordon code

// CordonHelper wraps functionality to cordon/uncordon nodes
type CordonHelper struct {
	node    *corev1.Node
	desired bool
}

// UncordonNode removes the noschedule taint from a node
func UnCordonNode(clientset kubernetes.Interface, node *v1.Node) error {
	helper := NewCordonHelper(node, false)
	err, patchErr := helper.PatchOrReplace(clientset)
	if err != nil {
		return multierror.Append(err, patchErr)
	}
	return nil
}

func CordonNode(clientset kubernetes.Interface, node *v1.Node) (rollback.Step, error) {
	helper := NewCordonHelper(node, true)
	err, patchErr := helper.PatchOrReplace(clientset)
	if err != nil {
		return rollback.Step{}, multierror.Append(err, patchErr)
	}
	return rollback.Step{
		Name:        fmt.Sprintf("k8s-cordon-node-%s-rollback", node.Name),
		Fn:          func() error { return UnCordonNode(clientset, node) },
		StopOnError: true,
	}, nil
}

// NewCordonHelper returns a new CordonHelper
func NewCordonHelper(node *corev1.Node, desired bool) *CordonHelper {
	return &CordonHelper{
		node:    node,
		desired: desired,
	}
}

// PatchOrReplace uses given clientset to update the node status, either by patching or
// updating the given node object; it may return error if the object cannot be encoded as
// JSON, or if either patch or update calls fail; it will also return a second error
// whenever creating a patch has failed
func (c *CordonHelper) PatchOrReplace(clientset kubernetes.Interface) (error, error) {
	client := clientset.CoreV1().Nodes()

	oldData, err := json.Marshal(c.node)
	if err != nil {
		return err, nil
	}

	c.node.Spec.Unschedulable = c.desired

	newData, err := json.Marshal(c.node)
	if err != nil {
		return err, nil
	}

	patchBytes, patchErr := strategicpatch.CreateTwoWayMergePatch(oldData, newData, c.node)
	if patchErr == nil {
		_, err = client.Patch(c.node.Name, types.StrategicMergePatchType, patchBytes)
	} else {
		_, err = client.Update(c.node)
	}
	return err, patchErr
}
