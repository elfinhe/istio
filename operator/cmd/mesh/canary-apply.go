// Copyright Istio Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package mesh

import (
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"strings"
	"time"

	jsonpatch "github.com/evanphx/json-patch"
	"github.com/spf13/cobra"
	networking "istio.io/api/networking/v1beta1"
	istiocn "istio.io/client-go/pkg/apis/networking/v1beta1"
	istioc "istio.io/client-go/pkg/clientset/versioned"
	"istio.io/istio/operator/pkg/helm"
	"istio.io/istio/operator/pkg/name"
	"istio.io/istio/operator/pkg/object"
	"istio.io/istio/operator/pkg/tpath"
	"istio.io/istio/operator/pkg/util"
	"istio.io/istio/operator/pkg/util/clog"
	"istio.io/pkg/log"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type canaryApplyArgs struct {
	// namespace is the namespace to apply manifests
	namespace string
	// inFilenames is an array of paths to the input IstioOperator CR files.
	inFilenames []string
	// kubeConfigPath is the path to kube config file.
	kubeConfigPath string
	// context is the cluster context in the kube config
	context string
	// readinessTimeout is maximum time to wait for all Istio resources to be ready. wait must be true for this setting
	// to take effect.
	readinessTimeout time.Duration
	// skipConfirmation determines whether the user is prompted for confirmation.
	// If set to true, the user is not prompted and a Yes response is assumed in all cases.
	skipConfirmation bool
	// force proceeds even if there are validation errors
	force bool
}

func addCanaryApplyFlags(cmd *cobra.Command, args *canaryApplyArgs) {
	cmd.PersistentFlags().StringVarP(&args.namespace, "namespace", "n", "", "The namespace that the manifests will be applied to")
	cmd.PersistentFlags().StringSliceVarP(&args.inFilenames, "filename", "f", nil, filenameFlagHelpStr)
	cmd.PersistentFlags().StringVarP(&args.kubeConfigPath, "kubeconfig", "c", "", KubeConfigFlagHelpStr)
	cmd.PersistentFlags().StringVar(&args.context, "context", "", ContextFlagHelpStr)
	cmd.PersistentFlags().DurationVar(&args.readinessTimeout, "readiness-timeout", 300*time.Second,
		"Maximum time to wait for Istio resources in each component to be ready.")
	cmd.PersistentFlags().BoolVarP(&args.skipConfirmation, "skip-confirmation", "y", false, skipConfirmationFlagHelpStr)
	cmd.PersistentFlags().BoolVar(&args.force, "force", false, ForceFlagHelpStr)
}

// CanaryApplyCmd generates an Istio canaryApply manifest and applies it to a cluster
func CanaryApplyCmd(logOpts *log.Options) *cobra.Command {
	rootArgs := &rootArgs{}
	iArgs := &canaryApplyArgs{}

	ic := &cobra.Command{
		Use:   "canary-apply",
		Short: "Similar to kubectl apply, but will run canary tests before applying deployments",
		Args:  cobra.ExactArgs(0),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCanaryApplyCmd(cmd, rootArgs, iArgs, logOpts)
		}}

	addFlags(ic, rootArgs)
	addCanaryApplyFlags(ic, iArgs)
	return ic
}

func runCanaryApplyCmd(cmd *cobra.Command, rootArgs *rootArgs, iArgs *canaryApplyArgs, logOpts *log.Options) error {
	l := clog.NewConsoleLogger(cmd.OutOrStdout(), cmd.ErrOrStderr(), installerScope)
	// Warn users if they use `istioctl canary-apply` without any config args.
	if !rootArgs.dryRun && !iArgs.skipConfirmation {
		if !confirm("This will canary-apply manifests into the cluster. Proceed? (y/N)", cmd.OutOrStdout()) {
			cmd.Print("Cancelled.\n")
			os.Exit(1)
		}
	}
	if err := configLogs(logOpts); err != nil {
		return fmt.Errorf("could not configure logs: %s", err)
	}
	if iArgs.namespace == "" {
		// TODO: use the local kubeconfig's current namespace
		iArgs.namespace = "default"
	}
	if err := CanaryApplyManifests(iArgs.inFilenames, iArgs.force, rootArgs.dryRun, iArgs.namespace,
		iArgs.kubeConfigPath, iArgs.context, iArgs.readinessTimeout, l); err != nil {
		return fmt.Errorf("failed to canary-apply manifests: %v", err)
	}

	return nil
}

// CanaryApplyManifests generates manifests from the given input files and --set flag
// overlays and applies them to the cluster
//  force   validation warnings are written to logger but command is not aborted
//  dryRun  all operations are done but nothing is written
func CanaryApplyManifests(inFilenames []string, force bool, dryRun bool, namespace,
	kubeConfigPath string, ctx string, waitTimeout time.Duration, l clog.Logger) error {
	restConfig, _, clientObj, err := K8sConfig(kubeConfigPath, ctx)
	ic, err := istioc.NewForConfig(restConfig)
	if err != nil {
		return err
	}

	ym, err := readYAMLs(inFilenames, os.Stdin)
	if err != nil {
		return err
	}

	l.LogAndPrintf("Read manifests from:\n%v", inFilenames)
	allObjects, err := object.ParseK8sObjectsFromYAMLManifest(ym)
	if err != nil {
		return err
	}

	l.LogAndPrint("\nObjects from manifests:")
	for _, obj := range allObjects.UnstructuredItems() {
		l.LogAndPrintf(fmt.Sprintf("%s/%s", obj.GetKind(), obj.GetName()))
	}

	deployObjs := object.KindObjects(allObjects, name.DeploymentStr)
	nonDeployObjs := object.ObjectsNotInLists(allObjects, deployObjs)

	l.LogAndPrint("\nApply non-deployment objects:")
	for _, obj := range nonDeployObjs.UnstructuredItems() {
		obj.SetNamespace(namespace)
		if err := applyObj(obj, clientObj, l); err != nil {
			return err
		}
	}

	l.LogAndPrint("\nApply deployments with canary:")
	canaryObjList := &unstructured.UnstructuredList{}
	canaryVsList := &istiocn.VirtualServiceList{}
	for _, deploy := range deployObjs.UnstructuredItems() {
		canaryDeploy := &appsv1.Deployment{}
		runtime.DefaultUnstructuredConverter.FromUnstructured(deploy.Object, &canaryDeploy)
		canaryDeploy.SetNamespace(namespace)
		deployTplLabels := canaryDeploy.Spec.Template.Labels
		canaryLabels := map[string]string{"canary.istio.io/deployment": canaryDeploy.GetName()}
		canaryDeploy.SetLabels(canaryLabels)
		canaryDeploy.Spec.Template.Labels = canaryLabels
		canaryDeploy.SetName(canaryDeploy.GetName() + "-canary")
		canaryDeployUnstructed, err := runtime.DefaultUnstructuredConverter.ToUnstructured(canaryDeploy)
		if err != nil {
			return err
		}

		labelSelector := &metav1.LabelSelector{MatchLabels: canaryLabels}
		if err := tpath.WriteNode(canaryDeployUnstructed, util.ToYAMLPath("spec.selector"), labelSelector); err != nil {
			return err
		}

		// Apply canary deployment
		canaryObj := unstructured.Unstructured{Object: canaryDeployUnstructed}
		canaryObjList.Items = append(canaryObjList.Items, canaryObj)
		if err := applyObj(canaryObj, clientObj, l); err != nil {
			return err
		}

		// Find all services
		svcList := &corev1.ServiceList{}
		err = clientObj.List(context.TODO(), svcList, client.InNamespace(namespace))
		if err != nil {
			return err
		}

		// Find deploys containing all service selector labels
		svcListForDeploy := &corev1.ServiceList{}
		for _, svc := range svcList.Items {
			if len(svc.Spec.Selector) != 0 && containsAll(deployTplLabels, svc.Spec.Selector) {
				svcListForDeploy.Items = append(svcListForDeploy.Items, svc)
			}
		}

		// For each service
		for _, svc := range svcListForDeploy.Items {
			// Create canary service
			canarySvc := svc.DeepCopy()
			canarySvc.Spec.Selector = canaryLabels
			canarySvc.TypeMeta.SetGroupVersionKind(schema.GroupVersionKind{
				Kind:    "Service",
				Group:   "",
				Version: "v1",
			})
			canarySvc.Status.Reset()
			canarySvc.ObjectMeta.SetSelfLink("")
			canarySvc.ObjectMeta.SetUID("")
			canarySvc.ObjectMeta.SetResourceVersion("")
			canarySvc.Spec.ClusterIP = ""
			canarySvc.SetName(canarySvc.GetName() + "-canary")
			canarySvcUnstructed, err := runtime.DefaultUnstructuredConverter.ToUnstructured(canarySvc)
			if err != nil {
				return err
			}
			canaryObj := unstructured.Unstructured{Object: canarySvcUnstructed}
			canaryObjList.Items = append(canaryObjList.Items, canaryObj)
			if err := applyObj(canaryObj, clientObj, l); err != nil {
				return err
			}

			// Parameters
			startWeight := int32(0)
			endWeight := int32(30)
			stepWeight := int32(1)
			intervalSeconds := 1
			bakingSeconds := 600

			vs := istiocn.VirtualService{}
			for canaryWeight := startWeight; canaryWeight <= endWeight; canaryWeight += stepWeight {
				l.LogAndPrintf("Shift canary traffic weight to %v", canaryWeight)
				vs = istiocn.VirtualService{
					ObjectMeta: metav1.ObjectMeta{
						Name:      svc.Name + "-canary",
						Namespace: svc.Namespace,
					},
					Spec: networking.VirtualService{
						Hosts: []string{svc.Name},
						Tcp: []*networking.TCPRoute{
							{
								Route: []*networking.RouteDestination{
									{
										Weight:      100 - canaryWeight,
										Destination: &networking.Destination{Host: svc.Name},
									},
									{
										Weight:      canaryWeight,
										Destination: &networking.Destination{Host: canarySvc.Name},
									},
								},
							},
						},
						Http: []*networking.HTTPRoute{
							{
								Route: []*networking.HTTPRouteDestination{
									{
										Weight:      100 - canaryWeight,
										Destination: &networking.Destination{Host: svc.Name},
									},
									{
										Weight:      canaryWeight,
										Destination: &networking.Destination{Host: canarySvc.Name},
									},
								},
							},
						},
						Tls: []*networking.TLSRoute{
							{
								Route: []*networking.RouteDestination{
									{
										Weight:      100 - canaryWeight,
										Destination: &networking.Destination{Host: svc.Name},
									},
									{
										Weight:      canaryWeight,
										Destination: &networking.Destination{Host: canarySvc.Name},
									},
								},
							},
						},
					},
				}
				err = applyIstioVirtualService(ic, vs, l)
				if err != nil {
					return err
				}
				sleepSeconds(time.Second * time.Duration(intervalSeconds))
			}
			canaryVsList.Items = append(canaryVsList.Items, vs)
			l.LogAndPrintf("Start baking canary deployment: %s", canaryDeploy.Name)
			sleepSeconds(time.Second * time.Duration(bakingSeconds))
		}
	}

	l.LogAndPrint("Start cleaning up canary virtual services")
	for _, cvs := range canaryVsList.Items {
		if err := deleteIstioVirtualService(ic, cvs, l); err != nil {
			return err
		}
	}

	l.LogAndPrint("Start cleaning up other canary resources")
	for _, cs := range canaryObjList.Items {
		if err := deleteObj(cs, clientObj, l); err != nil {
			return err
		}
	}
	return nil
}

func applyIstioVirtualService(ic *istioc.Clientset, vs istiocn.VirtualService, l clog.Logger) error {
	objectStr := fmt.Sprintf("VirtualService/%s", vs.Name)
	existingObj, err := ic.NetworkingV1beta1().VirtualServices(vs.Namespace).Get(context.TODO(), vs.Name, metav1.GetOptions{})
	switch {
	case errors.IsNotFound(err):
		_, err := ic.NetworkingV1beta1().VirtualServices(vs.Namespace).Create(context.TODO(), &vs, metav1.CreateOptions{})
		if err != nil {
			l.LogAndPrintf("%s create error", objectStr)
			return fmt.Errorf("failed to create %q: %w", objectStr, err)
		}
		l.LogAndPrintf("%s created", objectStr)
	case err == nil:
		vs.ResourceVersion = existingObj.GetResourceVersion()
		newObj, err := ic.NetworkingV1beta1().VirtualServices(vs.Namespace).Update(context.TODO(), &vs, metav1.UpdateOptions{})
		if err != nil {
			l.LogAndPrintf("%s update error", objectStr)
			return fmt.Errorf("failed to update %q: %w", objectStr, err)
		}
		if eq, err := compareObjects(newObj, existingObj); err != nil {
			return err
		} else if eq {
			l.LogAndPrintf("%s unchanged", objectStr)
		} else {
			l.LogAndPrintf("%s configured", objectStr)
		}
	default:
		return err
	}
	return nil
}

func deleteIstioVirtualService(ic *istioc.Clientset, vs istiocn.VirtualService, l clog.Logger) error {
	objectStr := fmt.Sprintf("VirtualService/%s", vs.Name)
	_, err := ic.NetworkingV1beta1().VirtualServices(vs.Namespace).Get(context.TODO(), vs.Name, metav1.GetOptions{})
	switch {
	case errors.IsNotFound(err):
		l.LogAndPrintf("%s not found", objectStr)
	case err == nil:
		err := ic.NetworkingV1beta1().VirtualServices(vs.Namespace).Delete(context.TODO(), vs.GetName(), metav1.DeleteOptions{})
		if err != nil {
			l.LogAndPrintf("%s delete error", objectStr)
			return fmt.Errorf("failed to delete %q: %w", objectStr, err)
		}
		l.LogAndPrintf("%s deleted", objectStr)
	default:
		return err
	}
	return nil
}

func containsAll(am, bm map[string]string) bool {
	for k, v := range bm {
		if val, ok := am[k]; ok {
			if val != v {
				return false
			}
		} else {
			return false
		}
	}
	return true
}

func deleteObj(obj unstructured.Unstructured, clientObj client.Client, l clog.Logger) error {
	objectStr := fmt.Sprintf("%s/%s", obj.GetKind(), obj.GetName())
	existingObj := &unstructured.Unstructured{}
	existingObj.SetGroupVersionKind(obj.GetObjectKind().GroupVersionKind())
	objectKey, _ := client.ObjectKeyFromObject(&obj)
	err := clientObj.Get(context.TODO(), objectKey, existingObj)
	switch {
	case errors.IsNotFound(err):
		l.LogAndPrintf("%s not found", objectStr)
	case err == nil:
		err := clientObj.Delete(context.TODO(), existingObj)
		if err != nil {
			l.LogAndPrintf("%s delete error", objectStr)
			return fmt.Errorf("failed to delete %q: %w", objectStr, err)
		}
		l.LogAndPrintf("%s deleted", objectStr)
	default:
		return err
	}
	return nil
}

func applyObj(obj unstructured.Unstructured, clientObj client.Client, l clog.Logger) error {
	objectStr := fmt.Sprintf("%s/%s", obj.GetKind(), obj.GetName())
	existingObj := &unstructured.Unstructured{}
	existingObj.SetGroupVersionKind(obj.GetObjectKind().GroupVersionKind())
	objectKey, _ := client.ObjectKeyFromObject(&obj)
	err := clientObj.Get(context.TODO(), objectKey, existingObj)
	switch {
	case errors.IsNotFound(err):
		err = clientObj.Create(context.TODO(), &obj)
		if err != nil {
			l.LogAndPrintf("%s create error", objectStr)
			return fmt.Errorf("failed to create %q: %w", objectStr, err)
		}
		l.LogAndPrintf("%s created", objectStr)
	case err == nil:
		// TODO: repleace with k8s service-side apply
		newObj := existingObj.DeepCopy()
		if err := applyPatch(newObj, &obj); err != nil {
			return err
		}
		err := clientObj.Update(context.TODO(), newObj)
		if err != nil {
			l.LogAndPrintf("%s update error", objectStr)
			return fmt.Errorf("failed to update %q: %w", objectStr, err)
		}
		if eq, err := compareObjects(newObj, existingObj); err != nil {
			return err
		} else if eq {
			l.LogAndPrintf("%s unchanged", objectStr)
		} else {
			l.LogAndPrintf("%s configured", objectStr)
		}
	default:
		return err
	}
	return nil
}

// applyPatch applies an overlay using JSON patch strategy over the current Object in place.
func applyPatch(current, overlay runtime.Object) error {
	cj, err := runtime.Encode(unstructured.UnstructuredJSONScheme, current)
	if err != nil {
		return err
	}
	uj, err := runtime.Encode(unstructured.UnstructuredJSONScheme, overlay)
	if err != nil {
		return err
	}
	merged, err := jsonpatch.MergePatch(cj, uj)
	if err != nil {
		return err
	}
	return runtime.DecodeInto(unstructured.UnstructuredJSONScheme, merged, current)
}

// compareObjects compares if two objects are identical.
func compareObjects(c, u runtime.Object) (bool, error) {
	cj, err := runtime.Encode(unstructured.UnstructuredJSONScheme, c)
	if err != nil {
		return false, err
	}
	uj, err := runtime.Encode(unstructured.UnstructuredJSONScheme, u)
	if err != nil {
		return false, err
	}
	return jsonpatch.Equal(cj, uj), nil
}

func readYAMLs(filenames []string, stdinReader io.Reader) (string, error) {
	var ym string
	var stdin bool
	for _, fn := range filenames {
		var b []byte
		var err error
		if fn == "-" {
			if stdin {
				continue
			}
			stdin = true
			b, err = ioutil.ReadAll(stdinReader)
		} else {
			b, err = ioutil.ReadFile(strings.TrimSpace(fn))
		}
		if err != nil {
			return "", err
		}
		ym += string(b) + helm.YAMLSeparator
		if err != nil {
			return "", err
		}
	}
	return ym, nil
}
