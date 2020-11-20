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
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	jsonpatch "github.com/evanphx/json-patch"
	"github.com/spf13/cobra"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

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
)

type applyArgs struct {
	// strategy describes the deployment strategy used for deployment
	strategy deploymentStrategy
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
	// suppress the prompt messages
	silent bool
}

type DeployStrategyType string

const (
	Canary DeployStrategyType = "canary"
	Shadow                    = "shadow"
)

type CleanupType string

const (
	CleanupNo   CleanupType = "no"
	CleanupYes              = "yes"
	CleanupOnly             = "only"
)

var (
	newObjList     unstructured.UnstructuredList
	newVsList      istiocn.VirtualServiceList
	fileExtensions = []string{".json", ".yaml", ".yml"}
	dryRunStr      = " (dry run)"
)

type deploymentStrategy struct {
	endWeight       int32
	stepWeight      int32
	intervalSeconds int
	bakingSeconds   int
	deployStrategy  DeployStrategyType
	cleanup         CleanupType
	dump            bool
	dumpWeight      int32
}

func addApplyFlags(cmd *cobra.Command, args *applyArgs) {
	cmd.PersistentFlags().StringVarP(&args.namespace, "namespace", "n", "", "The namespace that the manifests will be applied to")
	cmd.PersistentFlags().StringSliceVarP(&args.inFilenames, "filename", "f", nil, filenameFlagHelpStr)
	cmd.PersistentFlags().StringVarP(&args.kubeConfigPath, "kubeconfig", "c", "", KubeConfigFlagHelpStr)
	cmd.PersistentFlags().StringVar(&args.context, "context", "", ContextFlagHelpStr)
	cmd.PersistentFlags().BoolVarP(&args.skipConfirmation, "skip-confirmation", "y", false, skipConfirmationFlagHelpStr)
	cmd.PersistentFlags().Int32Var(&args.strategy.endWeight, "end-weight", 30, "")
	cmd.PersistentFlags().Int32Var(&args.strategy.stepWeight, "step-weight", 6, "")
	cmd.PersistentFlags().IntVar(&args.strategy.intervalSeconds, "interval-seconds", 5, "")
	cmd.PersistentFlags().IntVar(&args.strategy.bakingSeconds, "baking-seconds", 60, "")
	cmd.PersistentFlags().StringVar((*string)(&args.strategy.deployStrategy), "deploy-strategy", Shadow, "The strategy to apply deployments")
	cmd.PersistentFlags().StringVar((*string)(&args.strategy.cleanup), "cleanup", CleanupYes, "The way to clean up resources created by this command")
	cmd.PersistentFlags().BoolVar(&args.strategy.dump, "dump", true, "Dump the manifests for deployment strategies")
	cmd.PersistentFlags().Int32Var(&args.strategy.dumpWeight, "weight", 10, "")
	cmd.PersistentFlags().BoolVar(&args.silent, "silent", false, "Suppress the prompt messages")
}

// StrategicApplyCmd generates shadow/canary deployments and applies it to a cluster
func StrategicApplyCmd(logOpts *log.Options) *cobra.Command {
	rootArgs := &rootArgs{}
	iArgs := &applyArgs{}

	ic := &cobra.Command{
		Use:   "strategic-deploy",
		Short: "Shadow/canary analysis for deployments",
		Args:  cobra.ExactArgs(0),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runStrategicApplyCmd(cmd, rootArgs, iArgs, logOpts)
		}}

	addFlags(ic, rootArgs)
	addApplyFlags(ic, iArgs)
	return ic
}

func runStrategicApplyCmd(cmd *cobra.Command, rootArgs *rootArgs, iArgs *applyArgs, logOpts *log.Options) error {
	l := clog.NewConsoleLogger(cmd.OutOrStdout(), cmd.ErrOrStderr(), installerScope)
	// Warn users if no arg
	if !rootArgs.dryRun && !iArgs.skipConfirmation {
		if !confirm("This will strategic-deploy manifests into the cluster. Proceed? (y/N)", cmd.OutOrStdout()) {
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

	// Set dump
	if iArgs.strategy.dump {
		iArgs.strategy.stepWeight = iArgs.strategy.dumpWeight
		iArgs.strategy.endWeight = iArgs.strategy.dumpWeight
		iArgs.strategy.bakingSeconds = 0
		iArgs.strategy.intervalSeconds = 0
		rootArgs.dryRun = true
		iArgs.silent = true
	}

	if err := StrategicApplyManifests(&iArgs.strategy, iArgs.inFilenames, iArgs.namespace,
		rootArgs.dryRun, iArgs.silent, iArgs.kubeConfigPath, iArgs.context, l); err != nil {
		return fmt.Errorf("failed to strategic-deploy manifests: %v", err)
	}

	return nil
}

// StrategicApplyManifests generates manifests from the given input files and --set flag
// overlays and applies them to the cluster
//  dryRun  all operations are done but nothing is written
func StrategicApplyManifests(ds *deploymentStrategy, inFilenames []string, namespace string,
	dryRun, silent bool, kubeConfigPath, ctx string, l clog.Logger) error {
	restConfig, _, clientObj, err := K8sConfig(kubeConfigPath, ctx)
	ic, err := istioc.NewForConfig(restConfig)
	if err != nil {
		return err
	}

	// Register cleanup process for interrupt and terminate signals
	signals := make(chan os.Signal)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)

	if ds.cleanup == CleanupYes {
		go func() {
			<-signals
			if err := cleanup(ic, clientObj, dryRun, silent, l); err != nil {
				if !silent {
					l.LogAndError("Clean up failed: %s", err)
				}
			}
			os.Exit(1)
		}()
	}

	var filenames []string
	for _, path := range inFilenames {
		if path == "-" {
			filenames = append(filenames, path)
		}
		info, err := os.Stat(path)
		if err != nil || info.Size() == 0 {
			continue
		}
		if info.IsDir() {
			err := filepath.Walk(path, func(path string, info os.FileInfo, err error) error {
				if err != nil {
					return err
				}
				if info.IsDir() {
					return nil
				}
				if !isValidFile(path) {
					if !silent {
						l.LogAndErrorf("Skipping file %v, recognized file extensions are: %v\n", path, fileExtensions)
					}
					return nil
				}
				filenames = append(filenames, path)
				return nil
			})
			if err != nil {
				return err
			}
		} else {
			filenames = append(filenames, path)
		}
	}

	ym, err := readYAMLs(filenames, os.Stdin)
	if err != nil {
		return err
	}

	allObjects, err := object.ParseK8sObjectsFromYAMLManifest(ym)
	if err != nil {
		return err
	}

	if !silent {
		l.LogAndError("\nObjects from manifests:")
	}
	for _, obj := range allObjects.UnstructuredItems() {
		if !silent {
			l.LogAndErrorf(fmt.Sprintf("%s/%s", obj.GetKind(), obj.GetName()))
		}
	}

	deployObjs := object.KindObjects(allObjects, name.DeploymentStr)
	nonDeployObjs := object.ObjectsNotInLists(allObjects, deployObjs)

	if ds.cleanup != CleanupOnly {
		if !silent {
			l.LogAndError("\nApply non-deployment objects:")
		}
		for _, obj := range nonDeployObjs.UnstructuredItems() {
			obj.SetNamespace(namespace)
			if err := applyObj(obj, clientObj, dryRun, silent, l); err != nil {
				return err
			}
		}
		if !silent {
			l.LogAndErrorf("\nApply deployments with %s:", ds.deployStrategy)
		}
	}

	for _, deploy := range deployObjs.UnstructuredItems() {
		newDeploy := &appsv1.Deployment{}
		if err = runtime.DefaultUnstructuredConverter.FromUnstructured(deploy.Object, &newDeploy); err != nil {
			return err
		}
		newDeploy.SetNamespace(namespace)
		deployTplLabels := newDeploy.Spec.Template.Labels
		newLabels := map[string]string{"istio.io/deployment": newDeploy.GetName()}
		newDeploy.SetLabels(newLabels)
		newDeploy.Spec.Template.Labels = newLabels
		newDeploy.SetName(newDeploy.GetName() + "-" + string(ds.deployStrategy))
		newDeployUnstructed, err := runtime.DefaultUnstructuredConverter.ToUnstructured(newDeploy)
		if err != nil {
			return err
		}

		labelSelector := &metav1.LabelSelector{MatchLabels: newLabels}
		if err := tpath.WriteNode(newDeployUnstructed, util.ToYAMLPath("spec.selector"), labelSelector); err != nil {
			return err
		}

		newObj := unstructured.Unstructured{Object: newDeployUnstructed}
		newObjList.Items = append(newObjList.Items, newObj)
		if ds.cleanup != CleanupOnly {
			if err := applyObj(newObj, clientObj, dryRun, silent, l); err != nil {
				return err
			}
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
			newSvc := svc.DeepCopy()
			newSvc.Spec.Selector = newLabels
			newSvc.TypeMeta.SetGroupVersionKind(schema.GroupVersionKind{
				Kind:    "Service",
				Group:   "",
				Version: "v1",
			})
			newSvc.Status.Reset()
			newSvc.ObjectMeta.SetSelfLink("")
			newSvc.ObjectMeta.SetUID("")
			newSvc.ObjectMeta.SetResourceVersion("")
			newSvc.Spec.ClusterIP = ""
			newSvc.SetName(newSvc.GetName() + "-" + string(ds.deployStrategy))
			newSvcUnstructed, err := runtime.DefaultUnstructuredConverter.ToUnstructured(newSvc)
			if err != nil {
				return err
			}
			newObj := unstructured.Unstructured{Object: newSvcUnstructed}
			newObjList.Items = append(newObjList.Items, newObj)
			if ds.cleanup != CleanupOnly {
				if err := applyObj(newObj, clientObj, dryRun, silent, l); err != nil {
					return err
				}
			}

			if ds.cleanup == CleanupOnly {
				switch {
				case ds.deployStrategy == Shadow || ds.deployStrategy == Canary:
					vs := istiocn.VirtualService{
						TypeMeta: metav1.TypeMeta{
							Kind:       "VirtualService",
							APIVersion: "networking.istio.io/v1beta1",
						},
						ObjectMeta: metav1.ObjectMeta{
							Name:      svc.Name + "-" + string(ds.deployStrategy),
							Namespace: svc.Namespace,
						},
					}
					newVsList.Items = append(newVsList.Items, vs)
				default:
					return fmt.Errorf("unsupported deployment strategy type: %s", ds.deployStrategy)
				}
			} else { // Not cleanup only, apply the virtual service to shift/mirror traffic
				if !silent {
					l.LogAndErrorf("Start shifting/mirroring traffic to %s", newSvc.GetName())
				}
				var vs istiocn.VirtualService
				vsCreated := false
				for currentWeight := int32(0); currentWeight <= ds.endWeight; currentWeight += ds.stepWeight {
					if !silent {
						l.LogAndErrorf("Shift traffic weight to %v", currentWeight)
					}
					switch {
					case ds.deployStrategy == Shadow:
						vs = istiocn.VirtualService{
							TypeMeta: metav1.TypeMeta{
								Kind:       "VirtualService",
								APIVersion: "networking.istio.io/v1beta1",
							},
							ObjectMeta: metav1.ObjectMeta{
								Name:      svc.Name + "-" + string(ds.deployStrategy),
								Namespace: svc.Namespace,
							},
							Spec: networking.VirtualService{
								Hosts: []string{svc.Name},
								Http: []*networking.HTTPRoute{
									{
										Route: []*networking.HTTPRouteDestination{
											{
												Weight:      100,
												Destination: &networking.Destination{Host: svc.Name},
											},
										},
										Mirror:           &networking.Destination{Host: newSvc.Name},
										MirrorPercentage: &networking.Percent{Value: float64(currentWeight)},
									},
								},
							},
						}
					case ds.deployStrategy == Canary:
						vs = istiocn.VirtualService{
							TypeMeta: metav1.TypeMeta{
								Kind:       "VirtualService",
								APIVersion: "networking.istio.io/v1beta1",
							},
							ObjectMeta: metav1.ObjectMeta{
								Name:      svc.Name + "-" + string(ds.deployStrategy),
								Namespace: svc.Namespace,
							},
							Spec: networking.VirtualService{
								Hosts: []string{svc.Name},
								Tcp: []*networking.TCPRoute{
									{
										Route: []*networking.RouteDestination{
											{
												Weight:      100 - currentWeight,
												Destination: &networking.Destination{Host: svc.Name},
											},
											{
												Weight:      currentWeight,
												Destination: &networking.Destination{Host: newSvc.Name},
											},
										},
									},
								},
								Http: []*networking.HTTPRoute{
									{
										Route: []*networking.HTTPRouteDestination{
											{
												Weight:      100 - currentWeight,
												Destination: &networking.Destination{Host: svc.Name},
											},
											{
												Weight:      currentWeight,
												Destination: &networking.Destination{Host: newSvc.Name},
											},
										},
									},
								},
							},
						}
					default:
						return fmt.Errorf("unsupported deployment strategy type: %s", ds.deployStrategy)
					}

					err = applyIstioVirtualService(ic, vs, dryRun, silent, l)
					if err != nil {
						return err
					}
					vsCreated = true
					if !(ds.dump || dryRun) {
						sleepSeconds(time.Second * time.Duration(ds.intervalSeconds))
					}
				}
				if vsCreated {
					newVsList.Items = append(newVsList.Items, vs)
					if !silent {
						l.LogAndErrorf("Start baking %s deployment: %s", ds.deployStrategy, newDeploy.Name)
					}
					if !(ds.dump || dryRun) {
						sleepSeconds(time.Second * time.Duration(ds.bakingSeconds))
					}
				}
			}
		}
	}

	if ds.dump {
		var output string
		if ds.cleanup != CleanupOnly {
			for _, o := range nonDeployObjs {
				y, err := o.YAML()
				if err != nil {
					return err
				}
				output += string(y) + helm.YAMLSeparator
			}
		}
		for _, obj := range newObjList.Items {
			o := object.NewK8sObject(&obj, nil, nil)
			y, err := o.YAML()
			if err != nil {
				return err
			}
			output += string(y) + helm.YAMLSeparator
		}
		for _, vs := range newVsList.Items {
			m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(vs.DeepCopyObject())
			if err != nil {
				return err
			}
			u := &unstructured.Unstructured{Object: m}
			o := object.NewK8sObject(u, nil, nil)
			y, err := o.YAML()
			if err != nil {
				return err
			}
			output += string(y) + helm.YAMLSeparator
		}
		l.LogAndPrint(output)
	} else if ds.cleanup == CleanupYes || ds.cleanup == CleanupOnly {
		if err = cleanup(ic, clientObj, dryRun, silent, l); err != nil {
			return err
		}
	}
	return nil
}

func isValidFile(f string) bool {
	ext := filepath.Ext(f)
	for _, e := range fileExtensions {
		if e == ext {
			return true
		}
	}
	return false
}

func cleanup(ic *istioc.Clientset, clientObj client.Client, dryRun, silent bool, l clog.Logger) error {
	if !silent {
		l.LogAndError("\nStart cleaning up virtual services")
	}
	for _, cvs := range newVsList.Items {
		if err := deleteIstioVirtualService(ic, cvs, dryRun, silent, l); err != nil {
			return err
		}
	}

	if !silent {
		l.LogAndError("\nStart cleaning up other resources")
	}
	for _, cs := range newObjList.Items {
		if err := deleteObj(cs, clientObj, dryRun, silent, l); err != nil {
			return err
		}
	}
	return nil
}

func applyIstioVirtualService(ic *istioc.Clientset, vs istiocn.VirtualService, dryRun, silent bool, l clog.Logger) error {
	var dryRunOpt []string
	dryRunMessage := ""
	if dryRun {
		dryRunMessage = dryRunStr
		dryRunOpt = []string{metav1.DryRunAll}
	}
	objectStr := fmt.Sprintf("VirtualService/%s", vs.Name)
	existingObj, err := ic.NetworkingV1beta1().VirtualServices(vs.Namespace).Get(context.TODO(), vs.Name, metav1.GetOptions{})
	switch {
	case errors.IsNotFound(err):
		_, err := ic.NetworkingV1beta1().VirtualServices(vs.Namespace).Create(context.TODO(), &vs, metav1.CreateOptions{DryRun: dryRunOpt})
		if err != nil {
			if !silent {
				l.LogAndErrorf("%s create error%s", objectStr, dryRunMessage)
			}
			return fmt.Errorf("failed to create %q: %w", objectStr, err)
		}
		if !silent {
			l.LogAndErrorf("%s created%s", objectStr, dryRunMessage)
		}
	case err == nil:
		vs.ResourceVersion = existingObj.GetResourceVersion()
		newObj, err := ic.NetworkingV1beta1().VirtualServices(vs.Namespace).Update(context.TODO(), &vs, metav1.UpdateOptions{DryRun: dryRunOpt})
		if err != nil {
			if !silent {
				l.LogAndErrorf("%s update error%s", objectStr, dryRunMessage)
			}
			return fmt.Errorf("failed to update %q: %w", objectStr, err)
		}
		if eq, err := compareObjects(newObj, existingObj); err != nil {
			return err
		} else if eq {
			if !silent {
				l.LogAndErrorf("%s unchanged%s", objectStr, dryRunMessage)
			}
		} else {
			if !silent {
				l.LogAndErrorf("%s configured%s", objectStr, dryRunMessage)
			}
		}
	default:
		return err
	}
	return nil
}

func deleteIstioVirtualService(ic *istioc.Clientset, vs istiocn.VirtualService, dryRun, silent bool, l clog.Logger) error {
	var dryRunOpt []string
	dryRunMessage := ""
	if dryRun {
		dryRunMessage = dryRunStr
		dryRunOpt = []string{metav1.DryRunAll}
	}
	objectStr := fmt.Sprintf("VirtualService/%s", vs.Name)
	_, err := ic.NetworkingV1beta1().VirtualServices(vs.Namespace).Get(context.TODO(), vs.Name, metav1.GetOptions{})
	switch {
	case errors.IsNotFound(err):
		if !silent {
			l.LogAndErrorf("%s not found%s", objectStr, dryRunMessage)
		}
	case err == nil:
		err := ic.NetworkingV1beta1().VirtualServices(vs.Namespace).Delete(context.TODO(), vs.GetName(), metav1.DeleteOptions{DryRun: dryRunOpt})
		if err != nil {
			if !silent {
				l.LogAndErrorf("%s delete error%s", objectStr, dryRunMessage)
			}
			return fmt.Errorf("failed to delete %q: %w", objectStr, err)
		}
		if !silent {
			l.LogAndErrorf("%s deleted%s", objectStr, dryRunMessage)
		}
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

func deleteObj(obj unstructured.Unstructured, clientObj client.Client, dryRun, silent bool, l clog.Logger) error {
	var dryRunOpt client.DeleteOptions
	dryRunMessage := ""
	if dryRun {
		dryRunMessage = dryRunStr
		dryRunOpt = client.DeleteOptions{DryRun: []string{metav1.DryRunAll}}
	}

	objectStr := fmt.Sprintf("%s/%s", obj.GetKind(), obj.GetName())
	existingObj := &unstructured.Unstructured{}
	existingObj.SetGroupVersionKind(obj.GetObjectKind().GroupVersionKind())
	objectKey, _ := client.ObjectKeyFromObject(&obj)
	err := clientObj.Get(context.TODO(), objectKey, existingObj)
	switch {
	case errors.IsNotFound(err):
		if !silent {
			l.LogAndErrorf("%s not found%s", objectStr, dryRunMessage)
		}
	case err == nil:
		err := clientObj.Delete(context.TODO(), existingObj, &dryRunOpt)
		if err != nil {
			if !silent {
				l.LogAndErrorf("%s delete error%s", objectStr, dryRunMessage)
			}
			return fmt.Errorf("failed to delete %q: %w", objectStr, err)
		}
		if !silent {
			l.LogAndErrorf("%s deleted%s", objectStr, dryRunMessage)
		}
	default:
		return err
	}
	return nil
}

func applyObj(obj unstructured.Unstructured, clientObj client.Client, dryRun, silent bool, l clog.Logger) error {
	var dryRunOptC client.CreateOptions
	var dryRunOptU client.UpdateOptions
	dryRunMessage := ""
	if dryRun {
		dryRunMessage = dryRunStr
		dryRunOptC = client.CreateOptions{DryRun: []string{metav1.DryRunAll}}
		dryRunOptU = client.UpdateOptions{DryRun: []string{metav1.DryRunAll}}
	}
	objectStr := fmt.Sprintf("%s/%s", obj.GetKind(), obj.GetName())
	existingObj := &unstructured.Unstructured{}
	existingObj.SetGroupVersionKind(obj.GetObjectKind().GroupVersionKind())
	objectKey, _ := client.ObjectKeyFromObject(&obj)
	err := clientObj.Get(context.TODO(), objectKey, existingObj)
	switch {
	case errors.IsNotFound(err):
		err = clientObj.Create(context.TODO(), &obj, &dryRunOptC)
		if err != nil {
			if !silent {
				l.LogAndErrorf("%s create error%s", objectStr, dryRunMessage)
			}
			return fmt.Errorf("failed to create %q: %w", objectStr, err)
		}
		if !silent {
			l.LogAndErrorf("%s created%s", objectStr, dryRunMessage)
		}
	case err == nil:
		// TODO: repleace with k8s service-side apply
		newObj := existingObj.DeepCopy()
		if err := applyPatch(newObj, &obj); err != nil {
			return err
		}
		err := clientObj.Update(context.TODO(), newObj, &dryRunOptU)
		if err != nil {
			if !silent {
				l.LogAndErrorf("%s update error%s", objectStr, dryRunMessage)
			}
			return fmt.Errorf("failed to update %q: %w", objectStr, err)
		}
		if eq, err := compareObjects(newObj, existingObj); err != nil {
			return err
		} else if eq {
			if !silent {
				l.LogAndErrorf("%s unchanged%s", objectStr, dryRunMessage)
			}
		} else {
			if !silent {
				l.LogAndErrorf("%s configured%s", objectStr, dryRunMessage)
			}
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
	}
	return ym, nil
}
