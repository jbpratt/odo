package component

import (
	"fmt"
	"io"
	"os"
	"reflect"
	"strings"
	"time"

	"github.com/devfile/library/pkg/devfile/generator"
	componentlabels "github.com/openshift/odo/pkg/component/labels"
	"github.com/openshift/odo/pkg/envinfo"
	"github.com/openshift/odo/pkg/util"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/fatih/color"
	"github.com/pkg/errors"
	"k8s.io/klog"

	devfilev1 "github.com/devfile/api/v2/pkg/apis/workspaces/v1alpha2"
	parsercommon "github.com/devfile/library/pkg/devfile/parser/data/v2/common"
	"github.com/openshift/odo/pkg/component"
	"github.com/openshift/odo/pkg/config"
	"github.com/openshift/odo/pkg/devfile/adapters/common"
	"github.com/openshift/odo/pkg/devfile/adapters/kubernetes/storage"
	"github.com/openshift/odo/pkg/devfile/adapters/kubernetes/utils"
	"github.com/openshift/odo/pkg/kclient"
	"github.com/openshift/odo/pkg/log"
	"github.com/openshift/odo/pkg/occlient"
	odoutil "github.com/openshift/odo/pkg/odo/util"
	storagepkg "github.com/openshift/odo/pkg/storage"
	storagelabels "github.com/openshift/odo/pkg/storage/labels"
	"github.com/openshift/odo/pkg/sync"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
)

const supervisorDStatusWaitTimeInterval = 1

// New instantiates a component adapter
func New(adapterContext common.AdapterContext, client kclient.Client) Adapter {

	adapter := Adapter{Client: client}
	adapter.GenericAdapter = common.NewGenericAdapter(&adapter, adapterContext)
	adapter.GenericAdapter.InitWith(&adapter)
	return adapter
}

// getPod lazily records and retrieves the pod associated with the component associated with this adapter. If refresh parameter
// is true, then the pod is refreshed from the cluster regardless of its current local state
func (a *Adapter) getPod(refresh bool) (*corev1.Pod, error) {
	if refresh || a.pod == nil {
		podSelector := fmt.Sprintf("component=%s", a.ComponentName)

		// Wait for Pod to be in running state otherwise we can't sync data to it.
		pod, err := a.Client.WaitAndGetPodWithEvents(podSelector, corev1.PodRunning, "Waiting for component to start")
		if err != nil {
			return nil, errors.Wrapf(err, "error while waiting for pod %s", podSelector)
		}
		a.pod = pod
	}
	return a.pod, nil
}

func (a *Adapter) ComponentInfo(command devfilev1.Command) (common.ComponentInfo, error) {
	pod, err := a.getPod(false)
	if err != nil {
		return common.ComponentInfo{}, err
	}
	return common.ComponentInfo{
		PodName:       pod.Name,
		ContainerName: command.Exec.Component,
	}, nil
}

func (a *Adapter) SupervisorComponentInfo(command devfilev1.Command) (common.ComponentInfo, error) {
	pod, err := a.getPod(false)
	if err != nil {
		return common.ComponentInfo{}, err
	}
	for _, container := range pod.Spec.Containers {
		if container.Name == command.Exec.Component && !reflect.DeepEqual(container.Command, []string{common.SupervisordBinaryPath}) {
			return common.ComponentInfo{
				ContainerName: command.Exec.Component,
				PodName:       pod.Name,
			}, nil
		}
	}
	return common.ComponentInfo{}, nil
}

// Adapter is a component adapter implementation for Kubernetes
type Adapter struct {
	Client kclient.Client
	*common.GenericAdapter

	devfileBuildCmd  string
	devfileRunCmd    string
	devfileDebugCmd  string
	devfileDebugPort int
	pod              *corev1.Pod
}

// Push updates the component if a matching component exists or creates one if it doesn't exist
// Once the component has started, it will sync the source code to it.
func (a Adapter) Push(parameters common.PushParameters) (err error) {
	componentExists, err := utils.ComponentExists(a.Client, a.ComponentName)
	if err != nil {
		return errors.Wrapf(err, "unable to determine if component %s exists", a.ComponentName)
	}

	a.devfileBuildCmd = parameters.DevfileBuildCmd
	a.devfileRunCmd = parameters.DevfileRunCmd
	a.devfileDebugCmd = parameters.DevfileDebugCmd
	a.devfileDebugPort = parameters.DebugPort

	podChanged := false
	var podName string

	// If the component already exists, retrieve the pod's name before it's potentially updated
	if componentExists {
		pod, err := a.getPod(true)
		if err != nil {
			return errors.Wrapf(err, "unable to get pod for component %s", a.ComponentName)
		}
		podName = pod.GetName()
	}

	// Validate the devfile build and run commands
	log.Info("\nValidation")
	s := log.Spinner("Validating the devfile")
	err = util.ValidateK8sResourceName("component name", a.ComponentName)
	if err != nil {
		return err
	}

	err = util.ValidateK8sResourceName("component namespace", parameters.EnvSpecificInfo.GetNamespace())
	if err != nil {
		return err
	}

	pushDevfileCommands, err := common.ValidateAndGetPushDevfileCommands(a.Devfile.Data, a.devfileBuildCmd, a.devfileRunCmd)
	if err != nil {
		s.End(false)
		return errors.Wrap(err, "failed to validate devfile build and run commands")
	}
	s.End(true)

	log.Infof("\nCreating Kubernetes resources for component %s", a.ComponentName)

	previousMode := parameters.EnvSpecificInfo.GetRunMode()
	currentMode := envinfo.Run

	if parameters.Debug {
		pushDevfileDebugCommands, err := common.ValidateAndGetDebugDevfileCommands(a.Devfile.Data, a.devfileDebugCmd)
		if err != nil {
			return fmt.Errorf("debug command is not valid")
		}
		pushDevfileCommands[devfilev1.DebugCommandGroupKind] = pushDevfileDebugCommands
		currentMode = envinfo.Debug
	}

	if currentMode != previousMode {
		parameters.RunModeChanged = true
	}

	err = a.createOrUpdateComponent(componentExists, parameters.EnvSpecificInfo)
	if err != nil {
		return errors.Wrap(err, "unable to create or update component")
	}

	deployment, err := a.Client.WaitForDeploymentRollout(a.ComponentName)
	if err != nil {
		return errors.Wrap(err, "error while waiting for deployment rollout")
	}

	// Wait for Pod to be in running state otherwise we can't sync data or exec commands to it.
	pod, err := a.getPod(true)
	if err != nil {
		return errors.Wrapf(err, "unable to get pod for component %s", a.ComponentName)
	}

	// list the latest state of the PVCs
	pvcs, err := a.Client.ListPVCs(fmt.Sprintf("%v=%v", "component", a.ComponentName))
	if err != nil {
		return err
	}

	// update the owner reference of the PVCs with the deployment
	for i := range pvcs {
		if pvcs[i].OwnerReferences != nil || pvcs[i].DeletionTimestamp != nil {
			continue
		}
		err = a.Client.UpdateStorageOwnerReference(&pvcs[i], generator.GetOwnerReference(deployment))
		if err != nil {
			return err
		}
	}

	parameters.EnvSpecificInfo.SetDevfileObj(a.Devfile)
	err = component.ApplyConfig(nil, &a.Client, config.LocalConfigInfo{}, parameters.EnvSpecificInfo, color.Output, componentExists, false)
	if err != nil {
		odoutil.LogErrorAndExit(err, "Failed to update config to component deployed.")
	}

	// Compare the name of the pod with the one before the rollout. If they differ, it means there's a new pod and a force push is required
	if componentExists && podName != pod.GetName() {
		podChanged = true
	}

	// Find at least one pod with the source volume mounted, error out if none can be found
	containerName, syncFolder, err := getFirstContainerWithSourceVolume(pod.Spec.Containers)
	if err != nil {
		return errors.Wrapf(err, "error while retrieving container from pod %s with a mounted project volume", podName)
	}

	log.Infof("\nSyncing to component %s", a.ComponentName)
	// Get a sync adapter. Check if project files have changed and sync accordingly
	syncAdapter := sync.New(a.AdapterContext, &a)
	compInfo := common.ComponentInfo{
		ContainerName: containerName,
		PodName:       pod.GetName(),
		SyncFolder:    syncFolder,
	}
	syncParams := common.SyncParameters{
		PushParams:      parameters,
		CompInfo:        compInfo,
		ComponentExists: componentExists,
		PodChanged:      podChanged,
	}
	execRequired, err := syncAdapter.SyncFiles(syncParams)
	if err != nil {
		return errors.Wrapf(err, "Failed to sync to component with name %s", a.ComponentName)
	}

	// PostStart events from the devfile will only be executed when the component
	// didn't previously exist
	postStartEvents := a.Devfile.Data.GetEvents().PostStart
	if !componentExists && len(postStartEvents) > 0 {
		err = a.ExecDevfileEvent(postStartEvents, common.PostStart, parameters.Show)
		if err != nil {
			return err

		}
	}

	if execRequired || parameters.RunModeChanged {
		log.Infof("\nExecuting devfile commands for component %s", a.ComponentName)
		err = a.ExecDevfile(pushDevfileCommands, componentExists, parameters)
		if err != nil {
			return err
		}

		runCommand := pushDevfileCommands[devfilev1.RunCommandGroupKind]
		if parameters.Debug {
			runCommand = pushDevfileCommands[devfilev1.DebugCommandGroupKind]
		}

		// wait for a second
		wait := time.After(supervisorDStatusWaitTimeInterval * time.Second)
		<-wait

		err := a.CheckSupervisordCtlStatus(runCommand)
		if err != nil {
			return err
		}
	} else {
		// no file was modified/added/deleted/renamed, thus return without syncing files
		log.Success("No file changes detected, skipping build. Use the '-f' flag to force the build.")
	}

	return nil
}

// CheckSupervisordCtlStatus checks the supervisord status according to the given command
// if the command is not in a running state, we fetch the last 20 lines of the component's log and display it
func (a Adapter) CheckSupervisordCtlStatus(command devfilev1.Command) error {
	statusInContainer := getSupervisordStatusInContainer(a.pod.Name, command.Exec.Component, a)

	supervisordProgramName := "devrun"

	// if the command is a debug one, we check against `debugrun`
	if command.Exec.Group.Kind == devfilev1.DebugCommandGroupKind {
		supervisordProgramName = "debugrun"
	}

	for _, status := range statusInContainer {
		if strings.EqualFold(status.program, supervisordProgramName) {
			if strings.EqualFold(status.status, "running") {
				return nil
			} else {
				numberOfLines := 20
				log.Warningf("devfile command \"%s\" exited with error status within %d sec", command.Id, supervisorDStatusWaitTimeInterval)
				log.Infof("Last %d lines of the component's log:", numberOfLines)

				rd, err := a.Log(false, command)
				if err != nil {
					return err
				}

				err = util.DisplayLog(false, rd, os.Stderr, a.ComponentName, numberOfLines)
				if err != nil {
					return err
				}

				log.Info("To get the full log output, please run 'odo log'")

				return nil
			}
		}
	}
	return fmt.Errorf("the supervisord program %s not found", supervisordProgramName)
}

// Test runs the devfile test command
func (a Adapter) Test(testCmd string, show bool) (err error) {
	pod, err := a.Client.GetPodUsingComponentName(a.ComponentName)
	if err != nil {
		return fmt.Errorf("error occurred while getting the pod: %w", err)
	}
	if pod.Status.Phase != corev1.PodRunning {
		return fmt.Errorf("pod for component %s is not running", a.ComponentName)
	}

	log.Infof("\nExecuting devfile test command for component %s", a.ComponentName)

	testCommand, err := common.ValidateAndGetTestDevfileCommands(a.Devfile.Data, testCmd)
	if err != nil {
		return errors.Wrap(err, "failed to validate devfile test command")
	}
	err = a.ExecuteDevfileCommand(testCommand, show)
	if err != nil {
		return errors.Wrapf(err, "failed to execute devfile commands for component %s", a.ComponentName)
	}
	return nil
}

// DoesComponentExist returns true if a component with the specified name exists, false otherwise
func (a Adapter) DoesComponentExist(cmpName string) (bool, error) {
	return utils.ComponentExists(a.Client, cmpName)
}

func (a Adapter) createOrUpdateComponent(componentExists bool, ei envinfo.EnvSpecificInfo) (err error) {
	ei.SetDevfileObj(a.Devfile)
	ocClient, err := occlient.New()
	if err != nil {
		return err
	}
	ocClient.SetKubeClient(&a.Client)

	storageClient := storagepkg.NewClient(storagepkg.ClientOptions{
		OCClient:            *ocClient,
		LocalConfigProvider: &ei,
	})

	// handle the ephemeral storage
	err = storage.HandleEphemeralStorage(a.Client, storageClient, a.ComponentName)
	if err != nil {
		return err
	}

	err = storagepkg.Push(storageClient, &ei)
	if err != nil {
		return err
	}

	componentName := a.ComponentName

	componentType := strings.TrimSuffix(a.AdapterContext.Devfile.Data.GetMetadata().Name, "-")

	labels := componentlabels.GetLabels(componentName, a.AppName, true)
	labels["component"] = componentName
	labels[componentlabels.ComponentTypeLabel] = componentType

	containers, err := generator.GetContainers(a.Devfile, parsercommon.DevfileOptions{})
	if err != nil {
		return err
	}

	if len(containers) == 0 {
		return fmt.Errorf("No valid components found in the devfile")
	}

	// Add the project volume before generating init containers
	utils.AddOdoProjectVolume(&containers)

	containers, err = utils.UpdateContainersWithSupervisord(a.Devfile, containers, a.devfileRunCmd, a.devfileDebugCmd, a.devfileDebugPort)
	if err != nil {
		return err
	}

	// Get ServiceBinding Secret name for each Link
	var sbSecrets []string
	for _, link := range ei.GetLink() {
		sb, err := a.Client.GetDynamicResource(kclient.ServiceBindingGroup, kclient.ServiceBindingVersion, kclient.ServiceBindingResource, link.Name)
		if err != nil {
			return err
		}
		secret, found, err := unstructured.NestedString(sb.Object, "status", "secret")
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("unable to find secret in ServiceBinding %s", link.Name)
		}
		sbSecrets = append(sbSecrets, secret)
	}

	// set EnvFrom to the container that's supposed to have link to the Operator backed service
	containers, err = utils.UpdateContainerWithEnvFromSecrets(containers, a.Devfile, a.devfileRunCmd, ei, sbSecrets)
	if err != nil {
		return err
	}

	objectMeta := generator.GetObjectMeta(componentName, a.Client.Namespace, labels, nil)
	supervisordInitContainer := kclient.GetBootstrapSupervisordInitContainer()
	initContainers, err := utils.GetPreStartInitContainers(a.Devfile, containers)
	if err != nil {
		return err
	}
	initContainers = append(initContainers, supervisordInitContainer)

	containerNameToVolumes, err := common.GetVolumes(a.Devfile)
	if err != nil {
		return err
	}

	var odoSourcePVCName string

	volumeNameToPVCName := make(map[string]string)

	// list all the pvcs for the component
	pvcs, err := a.Client.ListPVCs(fmt.Sprintf("%v=%v", "component", a.ComponentName))
	if err != nil {
		return err
	}

	for _, pvc := range pvcs {
		// check if the pvc is in the terminating state
		if pvc.DeletionTimestamp != nil {
			continue
		}
		volumeNameToPVCName[pvc.Labels[storagelabels.StorageLabel]] = pvc.Name
	}

	// Get PVC volumes and Volume Mounts
	containers, pvcVolumes, err := storage.GetPVCAndVolumeMount(containers, volumeNameToPVCName, containerNameToVolumes)
	if err != nil {
		return err
	}

	odoMandatoryVolumes := utils.GetOdoContainerVolumes(odoSourcePVCName)

	selectorLabels := map[string]string{
		"component": componentName,
	}

	deployParams := generator.DeploymentParams{
		TypeMeta:          generator.GetTypeMeta(kclient.DeploymentKind, kclient.DeploymentAPIVersion),
		ObjectMeta:        objectMeta,
		InitContainers:    initContainers,
		Containers:        containers,
		Volumes:           append(pvcVolumes, odoMandatoryVolumes...),
		PodSelectorLabels: selectorLabels,
	}

	deployment := generator.GetDeployment(deployParams)

	serviceParams := generator.ServiceParams{
		ObjectMeta:     objectMeta,
		SelectorLabels: selectorLabels,
	}
	service, err := generator.GetService(a.Devfile, serviceParams, parsercommon.DevfileOptions{})
	if err != nil {
		return err
	}
	klog.V(2).Infof("Creating deployment %v", deployment.Spec.Template.GetName())
	klog.V(2).Infof("The component name is %v", componentName)

	if componentExists {
		// If the component already exists, get the resource version of the deploy before updating
		klog.V(2).Info("The component already exists, attempting to update it")
		deployment, err = a.Client.UpdateDeployment(*deployment)
		if err != nil {
			return err
		}
		klog.V(2).Infof("Successfully updated component %v", componentName)
		oldSvc, err := a.Client.KubeClient.CoreV1().Services(a.Client.Namespace).Get(componentName, metav1.GetOptions{})
		ownerReference := generator.GetOwnerReference(deployment)
		service.OwnerReferences = append(service.OwnerReferences, ownerReference)
		if err != nil {
			// no old service was found, create a new one
			if len(service.Spec.Ports) > 0 {
				_, err = a.Client.CreateService(*service)
				if err != nil {
					return err
				}
				klog.V(2).Infof("Successfully created Service for component %s", componentName)
			}
		} else {
			if len(service.Spec.Ports) > 0 {
				service.Spec.ClusterIP = oldSvc.Spec.ClusterIP
				service.ResourceVersion = oldSvc.GetResourceVersion()
				_, err = a.Client.UpdateService(*service)
				if err != nil {
					return err
				}
				klog.V(2).Infof("Successfully update Service for component %s", componentName)
			} else {
				err = a.Client.KubeClient.CoreV1().Services(a.Client.Namespace).Delete(componentName, &metav1.DeleteOptions{})
				if err != nil {
					return err
				}
			}
		}
	} else {
		deployment, err = a.Client.CreateDeployment(*deployment)
		if err != nil {
			return err
		}
		klog.V(2).Infof("Successfully created component %v", componentName)
		ownerReference := generator.GetOwnerReference(deployment)
		service.OwnerReferences = append(service.OwnerReferences, ownerReference)
		if len(service.Spec.Ports) > 0 {
			_, err = a.Client.CreateService(*service)
			if err != nil {
				return err
			}
			klog.V(2).Infof("Successfully created Service for component %s", componentName)
		}

	}

	return nil
}

// getFirstContainerWithSourceVolume returns the first container that set mountSources: true as well
// as the path to the source volume inside the container.
// Because the source volume is shared across all components that need it, we only need to sync once,
// so we only need to find one container. If no container was found, that means there's no
// container to sync to, so return an error
func getFirstContainerWithSourceVolume(containers []corev1.Container) (string, string, error) {
	for _, c := range containers {
		for _, env := range c.Env {
			if env.Name == generator.EnvProjectsSrc {
				return c.Name, env.Value, nil
			}
		}
	}

	return "", "", fmt.Errorf("In order to sync files, odo requires at least one component in a devfile to set 'mountSources: true'")
}

// Delete deletes the component
func (a Adapter) Delete(labels map[string]string, show bool) error {

	log.Infof("\nGathering information for component %s", a.ComponentName)
	podSpinner := log.Spinner("Checking status for component")
	defer podSpinner.End(false)

	pod, err := a.Client.GetPodUsingComponentName(a.ComponentName)
	if kerrors.IsForbidden(err) {
		klog.V(2).Infof("Resource for %s forbidden", a.ComponentName)
		// log the error if it failed to determine if the component exists due to insufficient RBACs
		podSpinner.End(false)
		log.Warningf("%v", err)
		return nil
	} else if e, ok := err.(*kclient.PodNotFoundError); ok {
		podSpinner.End(false)
		log.Warningf("%v", e)
		return nil
	} else if err != nil {
		return errors.Wrapf(err, "unable to determine if component %s exists", a.ComponentName)
	}

	podSpinner.End(true)

	// if there are preStop events, execute them before deleting the deployment
	preStopEvents := a.Devfile.Data.GetEvents().PreStop
	if len(preStopEvents) > 0 {
		if pod.Status.Phase != corev1.PodRunning {
			return fmt.Errorf("unable to execute preStop events, pod for component %s is not running", a.ComponentName)
		}

		err = a.ExecDevfileEvent(preStopEvents, common.PreStop, show)
		if err != nil {
			return err
		}
	}

	log.Infof("\nDeleting component %s", a.ComponentName)
	spinner := log.Spinner("Deleting Kubernetes resources for component")
	defer spinner.End(false)

	err = a.Client.DeleteDeployment(labels)
	if err != nil {
		return err
	}

	spinner.End(true)
	log.Successf("Successfully deleted component")
	return nil
}

// Log returns log from component
func (a Adapter) Log(follow bool, command devfilev1.Command) (io.ReadCloser, error) {

	pod, err := a.Client.GetPodUsingComponentName(a.ComponentName)
	if err != nil {
		return nil, errors.Errorf("the component %s doesn't exist on the cluster", a.ComponentName)
	}

	if pod.Status.Phase != corev1.PodRunning {
		return nil, errors.Errorf("unable to show logs, component is not in running state. current status=%v", pod.Status.Phase)
	}

	containerName := command.Exec.Component

	return a.Client.GetPodLogs(pod.Name, containerName, follow)
}

// Exec executes a command in the component
func (a Adapter) Exec(command []string) error {
	exists, err := utils.ComponentExists(a.Client, a.ComponentName)
	if err != nil {
		return err
	}

	if !exists {
		return errors.Errorf("the component %s doesn't exist on the cluster", a.ComponentName)
	}

	runCommand, err := common.GetRunCommand(a.Devfile.Data, "")
	if err != nil {
		return err
	}
	containerName := runCommand.Exec.Component

	// get the pod
	pod, err := a.Client.GetPodUsingComponentName(a.ComponentName)
	if err != nil {
		return errors.Wrapf(err, "unable to get pod for component %s", a.ComponentName)
	}

	if pod.Status.Phase != corev1.PodRunning {
		return fmt.Errorf("unable to exec as the component is not running. Current status=%v", pod.Status.Phase)
	}

	componentInfo := common.ComponentInfo{
		PodName:       pod.Name,
		ContainerName: containerName,
	}

	return a.ExecuteCommand(componentInfo, command, true, nil, nil)
}

func (a Adapter) ExecCMDInContainer(componentInfo common.ComponentInfo, cmd []string, stdout io.Writer, stderr io.Writer, stdin io.Reader, tty bool) error {
	return a.Client.ExecCMDInContainer(componentInfo.ContainerName, componentInfo.PodName, cmd, stdout, stderr, stdin, tty)
}

// ExtractProjectToComponent extracts the project archive(tar) to the target path from the reader stdin
func (a Adapter) ExtractProjectToComponent(componentInfo common.ComponentInfo, targetPath string, stdin io.Reader) error {
	return a.Client.ExtractProjectToComponent(componentInfo.ContainerName, componentInfo.PodName, targetPath, stdin)
}
