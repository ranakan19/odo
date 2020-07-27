package component

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/openshift/odo/pkg/exec"
	"github.com/openshift/odo/pkg/secret"
	"github.com/openshift/odo/pkg/util"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/fatih/color"
	"github.com/pkg/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	runtimeUnstructured "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer/yaml"
	"k8s.io/klog"

	"github.com/openshift/odo/pkg/component"
	"github.com/openshift/odo/pkg/config"
	"github.com/openshift/odo/pkg/devfile/adapters/common"
	adaptersCommon "github.com/openshift/odo/pkg/devfile/adapters/common"
	"github.com/openshift/odo/pkg/devfile/adapters/kubernetes/storage"
	"github.com/openshift/odo/pkg/devfile/adapters/kubernetes/utils"
	versionsCommon "github.com/openshift/odo/pkg/devfile/parser/data/common"
	"github.com/openshift/odo/pkg/kclient"
	"github.com/openshift/odo/pkg/log"
	"github.com/openshift/odo/pkg/machineoutput"
	"github.com/openshift/odo/pkg/occlient"
	odoutil "github.com/openshift/odo/pkg/odo/util"
	"github.com/openshift/odo/pkg/sync"
	"github.com/openshift/odo/pkg/url"
)

const (
	DeployWaitTimeout = 60 * time.Second
)

// New instantiantes a component adapter
func New(adapterContext common.AdapterContext, client kclient.Client) Adapter {

	var loggingClient machineoutput.MachineEventLoggingClient

	if log.IsJSON() {
		loggingClient = machineoutput.NewConsoleMachineEventLoggingClient()
	} else {
		loggingClient = machineoutput.NewNoOpMachineEventLoggingClient()
	}

	return Adapter{
		Client:             client,
		AdapterContext:     adapterContext,
		machineEventLogger: loggingClient,
	}
}

// Adapter is a component adapter implementation for Kubernetes
type Adapter struct {
	Client kclient.Client
	common.AdapterContext
	devfileInitCmd     string
	devfileBuildCmd    string
	devfileRunCmd      string
	devfileDebugCmd    string
	devfileDebugPort   int
	machineEventLogger machineoutput.MachineEventLoggingClient
}

const dockerfilePath string = "Dockerfile"

func (a Adapter) generateBuildContainer(containerName string, dockerfileBytes []byte, imageTag string) corev1.Container {
	buildImage := "gcr.io/kaniko-project/executor:latest"

	// TODO(Optional): Init container before the buildah bud to copy over the files.
	//command := []string{"buildah"}
	//commandArgs := []string{"bud"}
	command := []string{}
	commandArgs := []string{"--context=git://github.com/ranakan19/nodejs-kaniko-test.git",
		"--destination=" + imageTag,
		"--skip-tls-verify"}

	// if dockerfilePath == "" {
	// 	dockerfilePath = "./Dockerfile"
	// }

	// TODO: Edit dockerfile env value if mounting it sometwhere else
	envVars := []corev1.EnvVar{
		{Name: "DOCKER_CONFIG", Value: "/kaniko/.docker"},
		{Name: "AWS_ACCESS_KEY_ID", Value: "NOT_SET"},
		{Name: "AWS_SECRET_KEY", Value: "NOT_SET"},
	}

	isPrivileged := false

	resourceReqs := corev1.ResourceRequirements{}

	container := kclient.GenerateContainer(containerName, buildImage, isPrivileged, command, commandArgs, envVars, resourceReqs, nil)

	container.VolumeMounts = []corev1.VolumeMount{
		{Name: "build-context", MountPath: "/kaniko/build-context"},
		{Name: "kaniko-secret", MountPath: "/kaniko/.docker"},
	}

	// if container.SecurityContext == nil {
	// 	container.SecurityContext = &corev1.SecurityContext{}
	// }
	// container.SecurityContext.RunAsUser = new(int64)

	return *container
}

func (a Adapter) generateInitContainer(initContainerName string) corev1.Container {

	initImage := "ubuntu"
	command := []string{}
	commandArgs := []string{}
	isPrivileged := false
	envVars := []corev1.EnvVar{}
	resourceReqs := corev1.ResourceRequirements{}

	container := kclient.GenerateContainer(initContainerName, initImage, isPrivileged, command, commandArgs, envVars, resourceReqs, nil)

	return *container
}

func (a Adapter) createBuildDeployment(labels map[string]string, container, initContainer corev1.Container) (err error) {

	objectMeta := kclient.CreateObjectMeta(a.ComponentName, a.Client.Namespace, labels, nil)
	podTemplateSpec := kclient.GeneratePodTemplateSpec(objectMeta, []corev1.Container{container})

	// TODO: For openshift, need to specify a service account that allows priviledged containers
	// saEnv := os.Getenv("BUILD_SERVICE_ACCOUNT")
	// if saEnv != "" {
	// 	podTemplateSpec.Spec.ServiceAccountName = saEnv
	// }

	buildContextVolume := corev1.Volume{
		Name: "build-context",
		VolumeSource: corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{},
		},
	}

	kanikoSecretVolume := corev1.Volume{
		Name: "kaniko-secret",
		VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{
				SecretName: "registry-credentials",
				Items: []corev1.KeyToPath{
					{
						Key:  ".dockerconfigjson",
						Path: "config.json",
					},
				},
			},
		},
	}

	podTemplateSpec.Spec.Volumes = append(podTemplateSpec.Spec.Volumes, buildContextVolume)
	podTemplateSpec.Spec.Volumes = append(podTemplateSpec.Spec.Volumes, kanikoSecretVolume)
	podTemplateSpec.Spec.InitContainers = []corev1.Container{initContainer}

	deploymentSpec := kclient.GenerateDeploymentSpec(*podTemplateSpec)
	klog.V(3).Infof("Creating deployment %v", deploymentSpec.Template.GetName())
	log.Info("Creating deployment %v", deploymentSpec.Template.GetName())
	log.Spinner("-------------------------------------------------CREATING DEPLOYMENT!!! ")
	_, err = a.Client.CreateDeployment(*deploymentSpec)
	if err != nil {
		log.Spinner("-------------------------------------------------DEPLOYMENT ERROR!!! ")
		return err
	}
	log.Spinner("-------------------------------------------------DEPLOYMENT CREATED!!! ")
	klog.V(3).Infof("Successfully created component %v", deploymentSpec.Template.GetName())

	return nil
}

func (a Adapter) injectBuildContext(syncFolder io.Reader, imageTag string, compInfo common.ComponentInfo) (err error) {

	buildContextTar, err := ioutil.ReadAll(syncFolder)
	if err != nil {
		return err
	}
	writeErr := ioutil.WriteFile("tmp/kaniko-build-context.tar.gz", buildContextTar, 0644)
	if writeErr != nil {
		return writeErr
	}
	// command := []string{adaptersCommon.ShellExecutable, "tar -zxf tmp/kaniko-build-context.tar.gz -c /kaniko/build-context",
	// 	"while true; do sleep 1; if [ -f /tmp/complete ]; then break; fi done"}
	command := []string{adaptersCommon.ShellExecutable, "while true; do sleep 1; if [ -f /tmp/complete ]; then break; fi done"}

	err = exec.ExecuteCommand(&a.Client, compInfo, command, false, nil, nil)
	if err != nil {
		return errors.Wrapf(err, "failed to load tar into specified build context directory")
	}
	return nil
}

func (a Adapter) runBuildConfig(client *occlient.Client, parameters common.BuildParameters) (err error) {
	buildName := a.ComponentName

	commonObjectMeta := metav1.ObjectMeta{
		Name: buildName,
	}

	controlC := make(chan os.Signal)
	signal.Notify(controlC, os.Interrupt, syscall.SIGTERM)
	go a.terminateBuild(controlC, client, commonObjectMeta)

	_, err = client.CreateDockerBuildConfigWithBinaryInput(commonObjectMeta, dockerfilePath, parameters.Tag, []corev1.EnvVar{})
	if err != nil {
		return err
	}

	defer func() {
		// This will delete both the BuildConfig and any builds using that BuildConfig
		derr := client.DeleteBuildConfig(commonObjectMeta)
		if err == nil {
			err = derr
		}
	}()

	syncAdapter := sync.New(a.AdapterContext, &a.Client)
	reader, err := syncAdapter.SyncFilesBuild(parameters, dockerfilePath)
	if err != nil {
		return err
	}

	bc, err := client.RunBuildConfigWithBinaryInput(buildName, reader)
	if err != nil {
		return err
	}
	log.Successf("Started build %s using BuildConfig", bc.Name)

	reader, writer := io.Pipe()

	var cmdOutput string
	// This Go routine will automatically pipe the output from WaitForBuildToFinish to
	// our logger.
	// We pass the controlC os.Signal in order to output the logs within the terminateBuild
	// function if the process is interrupted by the user performing a ^C. If we didn't pass it
	// The Scanner would consume the log, and only output it if there was an err within this
	// func.
	go func(controlC chan os.Signal) {
		select {
		case <-controlC:
			return
		default:
			scanner := bufio.NewScanner(reader)
			for scanner.Scan() {
				line := scanner.Text()

				if log.IsDebug() {
					_, err := fmt.Fprintln(os.Stdout, line)
					if err != nil {
						log.Errorf("Unable to print to stdout: %v", err)
					}
				}

				cmdOutput += fmt.Sprintln(line)
			}
		}
	}(controlC)

	s := log.Spinner("Waiting for build to complete")
	if err := client.WaitForBuildToFinish(bc.Name, writer); err != nil {
		s.End(false)
		return errors.Wrapf(err, "unable to build image using BuildConfig %s, error: %s", buildName, cmdOutput)
	}

	s.End(true)
	// Stop listening for a ^C so it doesnt perform terminateBuild during any later stages
	signal.Stop(controlC)
	return
}

// terminateBuild is triggered if the user performs a ^C action within the terminal during the build phase
// of the deploy.
// It cleans up the resources created for the build, as the defer function would not be reached.
// The subsequent deploy would fail if these resources are not cleaned up.
func (a Adapter) terminateBuild(c chan os.Signal, client *occlient.Client, commonObjectMeta metav1.ObjectMeta) {
	_ = <-c

	log.Info("\nBuild process interrupted, terminating build, this might take a few seconds")
	err := client.DeleteBuildConfig(commonObjectMeta)
	if err != nil {
		log.Info("\n", err.Error())
	}
	os.Exit(0)
}

// Build image for devfile project
func (a Adapter) Build(parameters common.BuildParameters) (err error) {
	client, err := occlient.New()
	if err != nil {
		return err
	}

	useKanikoBuild := true

	isBuildConfigSupported, err := client.IsBuildConfigSupported()
	if err != nil {
		return err
	}
	if useKanikoBuild {
		// perform kaniko build
		err := a.BuildWithKaniko(parameters)
		if err != nil {
			return err
		}
	}

	if isBuildConfigSupported {
		return a.runBuildConfig(client, parameters)
	}

	return errors.New("unable to build image, only Openshift BuildConfig build is supported")
}

func (a Adapter) BuildWithKaniko(parameters common.BuildParameters) (err error) {

	NamespacedName := types.NamespacedName{
		Name:      "registry-credentials",
		Namespace: parameters.EnvSpecificInfo.GetNamespace(),
	}

	dockerConfigBytes, err := util.LoadFileIntoMemory(parameters.Credentials)
	dockerSecret, err := secret.CreateDockerConfigSecret(NamespacedName, dockerConfigBytes)

	if err != nil {
		return err
	}

	gvk := schema.GroupVersionKind{
		Group:   "",
		Version: "v1",
		Kind:    "Secret",
	}
	gvr := schema.GroupVersionResource{Group: gvk.Group, Version: gvk.Version, Resource: strings.ToLower(gvk.Kind + "s")}
	// var dockerSecretUnstructured *unstructured.Unstructured
	dockerSecretMap, err := runtimeUnstructured.DefaultUnstructuredConverter.ToUnstructured(dockerSecret)
	dockerSecretByteArray, err := json.Marshal(dockerSecretMap)
	var dockerSecretUnstructured *unstructured.Unstructured
	dockerErr := json.Unmarshal(dockerSecretByteArray, &dockerSecretUnstructured)
	if dockerErr != nil {
		return dockerErr
	}
	_, dockerSecreteError := a.Client.DynamicClient.Resource(gvr).Namespace(NamespacedName.Namespace).Create(dockerSecretUnstructured, metav1.CreateOptions{})

	if dockerSecreteError != nil {
		return dockerSecreteError
	}
	containerName := a.ComponentName + "-container"
	initContainerName := "kaniko-init"
	buildContainer := a.generateBuildContainer(containerName, parameters.DockerfileBytes, parameters.Tag)
	// log.Spinner("-------------------------------------------------BUILDER CONTAINER CREATED!!! ")
	labels := map[string]string{
		"component": a.ComponentName,
	}

	initContainer := a.generateInitContainer(initContainerName)
	// log.Spinner("---------------------------------------------------INIT CONTAINER CREATED!!!--------------------- ")
	err = a.createBuildDeployment(labels, buildContainer, initContainer)
	if err != nil {
		return errors.Wrap(err, "error while creating kaniko deployment")
	}

	// Delete deployment
	defer func() {
		derr := a.Delete(labels)
		if err == nil {
			err = errors.Wrapf(derr, "failed to delete build step for component with name: %s", a.ComponentName)
		}

		// rerr := os.Remove(parameters.DockerfilePath)
		// if err == nil {
		// 	err = errors.Wrapf(rerr, "failed to delete %s", parameters.DockerfilePath)
		// }
	}()

	_, err = a.Client.WaitForDeploymentRollout(a.ComponentName)
	if err != nil {
		return errors.Wrap(err, "error while waiting for deployment rollout")
	}

	// Wait for Pod to be in running state otherwise we can't sync data or exec commands to it.
	_, err = a.waitAndGetComponentPod(false)
	if err != nil {
		return errors.Wrapf(err, "unable to get pod for component %s", a.ComponentName)
	}

	// Need to wait for container to start
	time.Sleep(5 * time.Second)

	// Sync files to volume
	log.Infof("\nSyncing to component %s", a.ComponentName)
	// Get a sync adapter. Check if project files have changed and sync accordingly
	// syncAdapter := sync.New(a.AdapterContext, &a.Client)
	// compInfo := common.ComponentInfo{
	// 	ContainerName: initContainerName,
	// 	PodName:       pod.GetName(),
	// }
	// var dockerfilepath = "Dockerfile"
	// syncFolder, err := syncAdapter.SyncFilesBuild(parameters, dockerfilepath)

	if err != nil {
		return errors.Wrapf(err, "failed to sync to component with name %s", a.ComponentName)
	}

	// err = a.injectBuildContext(syncFolder, parameters.Tag, compInfo)
	// if err != nil {
	// 	return err
	// }

	return

}

func substitueYamlVariables(baseYaml []byte, yamlSubstitutions map[string]string) []byte {
	// TODO: Provide a better way to do the substitution in the manifest file(s)
	for key, value := range yamlSubstitutions {
		if value != "" && bytes.Contains(baseYaml, []byte(key)) {
			klog.V(3).Infof("Replacing %s with %s", key, value)
			tempYaml := bytes.ReplaceAll(baseYaml, []byte(key), []byte(value))
			baseYaml = tempYaml
		}
	}
	return baseYaml
}

func getNamedCondition(route *unstructured.Unstructured, conditionTypeValue string) map[string]interface{} {
	status := route.UnstructuredContent()["status"].(map[string]interface{})
	conditions := status["conditions"].([]interface{})
	for i := range conditions {
		c := conditions[i].(map[string]interface{})
		klog.V(4).Infof("Condition returned\n%s\n", c)
		if c["type"] == conditionTypeValue {
			return c
		}
	}
	return nil
}

// TODO: Create a function to wait for deploment completion of any unstructured object
func (a Adapter) waitForManifestDeployCompletion(applicationName string, gvr schema.GroupVersionResource, conditionTypeValue string) (*unstructured.Unstructured, error) {
	klog.V(4).Infof("Waiting for %s manifest deployment completion", applicationName)
	w, err := a.Client.DynamicClient.Resource(gvr).Namespace(a.Client.Namespace).Watch(metav1.ListOptions{FieldSelector: "metadata.name=" + applicationName})
	if err != nil {
		return nil, errors.Wrapf(err, "unable to watch deployment")
	}
	defer w.Stop()
	success := make(chan *unstructured.Unstructured)
	failure := make(chan error)

	go func() {
		defer close(success)
		defer close(failure)

		for {
			val, ok := <-w.ResultChan()
			if !ok {
				failure <- errors.New("watch channel was closed")
				return
			}
			if watchObject, ok := val.Object.(*unstructured.Unstructured); ok {
				// TODO: Add more details on what to check to see if object deployment is complete
				// Currently only checks to see if status.conditions[] contains a condition with type = conditionTypeValue
				condition := getNamedCondition(watchObject, conditionTypeValue)
				if condition != nil {
					if condition["status"] == "Fail" {
						failure <- fmt.Errorf("manifest deployment %s failed", applicationName)
						return
					} else if condition["status"] == "True" {
						success <- watchObject
						return
					}
				}
			}
		}
	}()

	select {
	case val := <-success:
		return val, nil
	case err := <-failure:
		return nil, err
	case <-time.After(DeployWaitTimeout):
		return nil, errors.Errorf("timeout while waiting for %s manifest deployment completion", applicationName)
	}
}

// Build image for devfile project
func (a Adapter) Deploy(parameters common.DeployParameters) (err error) {
	namespace := a.Client.Namespace
	applicationName := a.ComponentName + "-deploy"
	deploymentManifest := &unstructured.Unstructured{}

	// Specify the substitution keys and values
	yamlSubstitutions := map[string]string{
		"CONTAINER_IMAGE": parameters.Tag,
		"COMPONENT_NAME":  applicationName,
		"PORT":            strconv.Itoa(parameters.DeploymentPort),
	}

	// Build a yaml decoder with the unstructured Scheme
	yamlDecoder := yaml.NewDecodingSerializer(unstructured.UnstructuredJSONScheme)

	// This will override if manifest.yaml is present
	writtenToManifest := false
	manifestFile, err := os.Create(filepath.Join(a.Context, ".odo", "manifest.yaml"))
	if err != nil {
		err = manifestFile.Close()
		return errors.Wrap(err, "Unable to create the local manifest file")
	}

	defer func() {
		merr := manifestFile.Close()
		if err == nil {
			err = merr
		}
	}()

	manifests := bytes.Split(parameters.ManifestSource, []byte("---"))
	for _, manifest := range manifests {
		if len(manifest) > 0 {
			// Substitute the values in the manifest file
			deployYaml := substitueYamlVariables(manifest, yamlSubstitutions)

			_, gvk, err := yamlDecoder.Decode([]byte(deployYaml), nil, deploymentManifest)
			if err != nil {
				return errors.Wrap(err, "Failed to decode the manifest yaml")
			}

			gvr := schema.GroupVersionResource{Group: gvk.Group, Version: gvk.Version, Resource: strings.ToLower(gvk.Kind + "s")}
			klog.V(3).Infof("Manifest type: %s", gvr.String())

			labels := map[string]string{
				"component": applicationName,
			}

			manifestLabels := deploymentManifest.GetLabels()
			if manifestLabels != nil {
				for key, value := range labels {
					manifestLabels[key] = value
				}
				deploymentManifest.SetLabels(manifestLabels)
			} else {
				deploymentManifest.SetLabels(labels)
			}

			// Check to see whether deployed resource already exists. If not, create else update
			instanceFound := false
			item, err := a.Client.DynamicClient.Resource(gvr).Namespace(namespace).Get(deploymentManifest.GetName(), metav1.GetOptions{})
			if item != nil && err == nil {
				instanceFound = true
				deploymentManifest.SetResourceVersion(item.GetResourceVersion())
				deploymentManifest.SetAnnotations(item.GetAnnotations())
				// If deployment is a `Service` of type `ClusterIP` then the service in the manifest will probably not
				// have a ClusterIP defined, as this is determined when the manifest is applied. When updating the Service
				// the manifest cannot have an empty `ClusterIP` defintion, so we need to copy this from the existing definition.
				if item.GetKind() == "Service" {
					currentServiceSpec := item.UnstructuredContent()["spec"].(map[string]interface{})
					if currentServiceSpec["type"] == "ClusterIP" {
						newService := deploymentManifest.UnstructuredContent()
						newService["spec"].(map[string]interface{})["clusterIP"] = currentServiceSpec["clusterIP"]
						deploymentManifest.SetUnstructuredContent(newService)
					}
				}
			}

			actionType := "Creating"
			if instanceFound {
				actionType = "Updating" // Update deployment
			}
			s := log.Spinnerf("%s resource of kind %s", strings.Title(actionType), gvk.Kind)
			result := &unstructured.Unstructured{}
			if !instanceFound {
				result, err = a.Client.DynamicClient.Resource(gvr).Namespace(namespace).Create(deploymentManifest, metav1.CreateOptions{})
			} else {
				result, err = a.Client.DynamicClient.Resource(gvr).Namespace(namespace).Update(deploymentManifest, metav1.UpdateOptions{})
			}
			if err != nil {
				s.End(false)
				return errors.Wrapf(err, "Failed when %s manifest %s", actionType, gvk.Kind)
			}
			s.End(true)

			// Write the returned manifest to the local manifest file
			if writtenToManifest {
				_, err = manifestFile.WriteString("---\n")
				if err != nil {
					return errors.Wrap(err, "Unable to write to local manifest file")
				}
			}
			err = yamlDecoder.Encode(result, manifestFile)
			if err != nil {
				return errors.Wrap(err, "Unable to write to local manifest file")
			}
			writtenToManifest = true
		}
	}

	s := log.Spinner("Determining the application URL")

	// TODO: Can we use a occlient created somewhere else rather than create another
	client, err := occlient.New()
	if err != nil {
		return err
	}

	// Need to wait for a second to give the server time to create the artifacts
	// TODO: Replace wait with a wait for object to be created
	time.Sleep(2 * time.Second)

	fullURL := ""
	urlList, err := url.List(client, &config.LocalConfigInfo{}, "", applicationName)
	if err != nil {
		s.End(false)
		return errors.Wrapf(err, "Unable to determine URL for application %s", applicationName)
	}
	if len(urlList.Items) > 0 {
		for _, url := range urlList.Items {
			fullURL = fmt.Sprintf("%s://%s", url.Spec.Protocol, url.Spec.Host)
		}
	} else {
		// No URL found - try looking for a knative Route therefore need to wait for Service and Route to be setup.
		knGvr := schema.GroupVersionResource{Group: "serving.knative.dev", Version: "v1", Resource: "routes"}
		route, err := a.waitForManifestDeployCompletion(applicationName, knGvr, "Ready")
		if err != nil {
			s.End(false)
			return errors.Wrap(err, "error while waiting for deployment completion")
		}
		fullURL = route.UnstructuredContent()["status"].(map[string]interface{})["url"].(string)
	}

	if fullURL != "" {
		s.End(true)
		log.Successf("Successfully deployed component: %s", fullURL)
	} else {
		s.End(false)
		log.Errorf("URL unable to be determined for component %s", a.ComponentName)
	}

	return nil
}

func (a Adapter) DeployDelete(manifest []byte) (err error) {
	deploymentManifest := &unstructured.Unstructured{}
	// Build a yaml decoder with the unstructured Scheme
	yamlDecoder := yaml.NewDecodingSerializer(unstructured.UnstructuredJSONScheme)
	manifests := bytes.Split(manifest, []byte("---"))
	for _, splitManifest := range manifests {
		if len(manifest) > 0 {
			_, gvk, err := yamlDecoder.Decode([]byte(splitManifest), nil, deploymentManifest)
			if err != nil {
				return err
			}
			klog.V(3).Infof("Deploy manifest:\n\n%s", deploymentManifest)
			gvr := schema.GroupVersionResource{Group: gvk.Group, Version: gvk.Version, Resource: strings.ToLower(gvk.Kind + "s")}
			klog.V(3).Infof("Manifest type: %s", gvr.String())

			_, err = a.Client.DynamicClient.Resource(gvr).Namespace(a.Client.Namespace).Get(deploymentManifest.GetName(), metav1.GetOptions{})
			if err != nil {
				errorMessage := "Could not delete component " + deploymentManifest.GetName() + " as component was not found"
				return errors.New(errorMessage)
			}

			err = a.Client.DynamicClient.Resource(gvr).Namespace(a.Client.Namespace).Delete(deploymentManifest.GetName(), &metav1.DeleteOptions{})
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// Push updates the component if a matching component exists or creates one if it doesn't exist
// Once the component has started, it will sync the source code to it.
func (a Adapter) Push(parameters common.PushParameters) (err error) {
	componentExists := utils.ComponentExists(a.Client, a.ComponentName)

	a.devfileInitCmd = parameters.DevfileInitCmd
	a.devfileBuildCmd = parameters.DevfileBuildCmd
	a.devfileRunCmd = parameters.DevfileRunCmd
	a.devfileDebugCmd = parameters.DevfileDebugCmd
	a.devfileDebugPort = parameters.DebugPort

	podChanged := false
	var podName string

	// If the component already exists, retrieve the pod's name before it's potentially updated
	if componentExists {
		pod, err := a.waitAndGetComponentPod(true)
		if err != nil {
			return errors.Wrapf(err, "unable to get pod for component %s", a.ComponentName)
		}
		podName = pod.GetName()
	}

	// Validate the devfile build and run commands
	log.Info("\nValidation")
	s := log.Spinner("Validating the devfile")
	pushDevfileCommands, err := common.ValidateAndGetPushDevfileCommands(a.Devfile.Data, a.devfileInitCmd, a.devfileBuildCmd, a.devfileRunCmd)
	if err != nil {
		s.End(false)
		return errors.Wrap(err, "failed to validate devfile build and run commands")
	}
	s.End(true)

	log.Infof("\nCreating Kubernetes resources for component %s", a.ComponentName)

	if parameters.Debug {
		pushDevfileDebugCommands, err := common.ValidateAndGetDebugDevfileCommands(a.Devfile.Data, a.devfileDebugCmd)
		if err != nil {
			return fmt.Errorf("debug command is not valid")
		}
		pushDevfileCommands[versionsCommon.DebugCommandGroupType] = pushDevfileDebugCommands
		parameters.ForceBuild = true
	}

	err = a.createOrUpdateComponent(componentExists)
	if err != nil {
		return errors.Wrap(err, "unable to create or update component")
	}

	_, err = a.Client.WaitForDeploymentRollout(a.ComponentName)
	if err != nil {
		return errors.Wrap(err, "error while waiting for deployment rollout")
	}

	// Wait for Pod to be in running state otherwise we can't sync data or exec commands to it.
	pod, err := a.waitAndGetComponentPod(true)
	if err != nil {
		return errors.Wrapf(err, "unable to get pod for component %s", a.ComponentName)
	}

	err = component.ApplyConfig(nil, &a.Client, config.LocalConfigInfo{}, parameters.EnvSpecificInfo, color.Output, componentExists)
	if err != nil {
		odoutil.LogErrorAndExit(err, "Failed to update config to component deployed.")
	}

	// Compare the name of the pod with the one before the rollout. If they differ, it means there's a new pod and a force push is required
	if componentExists && podName != pod.GetName() {
		podChanged = true
	}

	// Find at least one pod with the source volume mounted, error out if none can be found
	containerName, err := getFirstContainerWithSourceVolume(pod.Spec.Containers)
	if err != nil {
		return errors.Wrapf(err, "error while retrieving container from pod %s with a mounted project volume", podName)
	}

	log.Infof("\nSyncing to component %s", a.ComponentName)
	// Get a sync adapter. Check if project files have changed and sync accordingly
	syncAdapter := sync.New(a.AdapterContext, &a.Client)
	compInfo := common.ComponentInfo{
		ContainerName: containerName,
		PodName:       pod.GetName(),
	}
	syncParams := adaptersCommon.SyncParameters{
		PushParams:      parameters,
		CompInfo:        compInfo,
		ComponentExists: componentExists,
		PodChanged:      podChanged,
	}
	execRequired, err := syncAdapter.SyncFiles(syncParams)
	if err != nil {
		return errors.Wrapf(err, "Failed to sync to component with name %s", a.ComponentName)
	}

	if execRequired {
		log.Infof("\nExecuting devfile commands for component %s", a.ComponentName)
		err = a.execDevfile(pushDevfileCommands, componentExists, parameters.Show, pod.GetName(), pod.Spec.Containers, parameters.Debug)
		if err != nil {
			return err
		}
	}

	return nil
}

// DoesComponentExist returns true if a component with the specified name exists, false otherwise
func (a Adapter) DoesComponentExist(cmpName string) bool {
	return utils.ComponentExists(a.Client, cmpName)
}

func (a Adapter) createOrUpdateComponent(componentExists bool) (err error) {
	componentName := a.ComponentName

	labels := map[string]string{
		"component": componentName,
	}

	containers, err := utils.GetContainers(a.Devfile)
	if err != nil {
		return err
	}

	if len(containers) == 0 {
		return fmt.Errorf("No valid components found in the devfile")
	}

	containers, err = utils.UpdateContainersWithSupervisord(a.Devfile, containers, a.devfileRunCmd, a.devfileDebugCmd, a.devfileDebugPort)
	if err != nil {
		return err
	}

	objectMeta := kclient.CreateObjectMeta(componentName, a.Client.Namespace, labels, nil)
	podTemplateSpec := kclient.GeneratePodTemplateSpec(objectMeta, containers)

	kclient.AddBootstrapSupervisordInitContainer(podTemplateSpec)

	componentAliasToVolumes := adaptersCommon.GetVolumes(a.Devfile)

	var uniqueStorages []common.Storage
	volumeNameToPVCName := make(map[string]string)
	processedVolumes := make(map[string]bool)

	// Get a list of all the unique volume names and generate their PVC names
	for _, volumes := range componentAliasToVolumes {
		for _, vol := range volumes {
			if _, ok := processedVolumes[vol.Name]; !ok {
				processedVolumes[vol.Name] = true

				// Generate the PVC Names
				klog.V(4).Infof("Generating PVC name for %v", vol.Name)
				generatedPVCName, err := storage.GeneratePVCNameFromDevfileVol(vol.Name, componentName)
				if err != nil {
					return err
				}

				// Check if we have an existing PVC with the labels, overwrite the generated name with the existing name if present
				existingPVCName, err := storage.GetExistingPVC(&a.Client, vol.Name, componentName)
				if err != nil {
					return err
				}
				if len(existingPVCName) > 0 {
					klog.V(4).Infof("Found an existing PVC for %v, PVC %v will be re-used", vol.Name, existingPVCName)
					generatedPVCName = existingPVCName
				}

				pvc := common.Storage{
					Name:   generatedPVCName,
					Volume: vol,
				}
				uniqueStorages = append(uniqueStorages, pvc)
				volumeNameToPVCName[vol.Name] = generatedPVCName
			}
		}
	}

	// Add PVC and Volume Mounts to the podTemplateSpec
	err = kclient.AddPVCAndVolumeMount(podTemplateSpec, volumeNameToPVCName, componentAliasToVolumes)
	if err != nil {
		return err
	}

	deploymentSpec := kclient.GenerateDeploymentSpec(*podTemplateSpec)
	var containerPorts []corev1.ContainerPort
	for _, c := range deploymentSpec.Template.Spec.Containers {
		if len(containerPorts) == 0 {
			containerPorts = c.Ports
		} else {
			containerPorts = append(containerPorts, c.Ports...)
		}
	}
	serviceSpec := kclient.GenerateServiceSpec(objectMeta.Name, containerPorts)
	klog.V(4).Infof("Creating deployment %v", deploymentSpec.Template.GetName())
	klog.V(4).Infof("The component name is %v", componentName)

	if utils.ComponentExists(a.Client, componentName) {
		// If the component already exists, get the resource version of the deploy before updating
		klog.V(4).Info("The component already exists, attempting to update it")
		deployment, err := a.Client.UpdateDeployment(*deploymentSpec)
		if err != nil {
			return err
		}
		klog.V(4).Infof("Successfully updated component %v", componentName)
		oldSvc, err := a.Client.KubeClient.CoreV1().Services(a.Client.Namespace).Get(componentName, metav1.GetOptions{})
		objectMetaTemp := objectMeta
		ownerReference := kclient.GenerateOwnerReference(deployment)
		objectMetaTemp.OwnerReferences = append(objectMeta.OwnerReferences, ownerReference)
		if err != nil {
			// no old service was found, create a new one
			if len(serviceSpec.Ports) > 0 {
				_, err = a.Client.CreateService(objectMetaTemp, *serviceSpec)
				if err != nil {
					return err
				}
				klog.V(4).Infof("Successfully created Service for component %s", componentName)
			}
		} else {
			if len(serviceSpec.Ports) > 0 {
				serviceSpec.ClusterIP = oldSvc.Spec.ClusterIP
				objectMetaTemp.ResourceVersion = oldSvc.GetResourceVersion()
				_, err = a.Client.UpdateService(objectMetaTemp, *serviceSpec)
				if err != nil {
					return err
				}
				klog.V(4).Infof("Successfully update Service for component %s", componentName)
			} else {
				err = a.Client.KubeClient.CoreV1().Services(a.Client.Namespace).Delete(componentName, &metav1.DeleteOptions{})
				if err != nil {
					return err
				}
			}
		}
	} else {
		deployment, err := a.Client.CreateDeployment(*deploymentSpec)
		if err != nil {
			return err
		}
		klog.V(4).Infof("Successfully created component %v", componentName)
		ownerReference := kclient.GenerateOwnerReference(deployment)
		objectMetaTemp := objectMeta
		objectMetaTemp.OwnerReferences = append(objectMeta.OwnerReferences, ownerReference)
		if len(serviceSpec.Ports) > 0 {
			_, err = a.Client.CreateService(objectMetaTemp, *serviceSpec)
			if err != nil {
				return err
			}
			klog.V(4).Infof("Successfully created Service for component %s", componentName)
		}

	}

	// Get the storage adapter and create the volumes if it does not exist
	stoAdapter := storage.New(a.AdapterContext, a.Client)
	err = stoAdapter.Create(uniqueStorages)
	if err != nil {
		return err
	}

	return nil
}

func (a Adapter) waitAndGetComponentPod(hideSpinner bool) (*corev1.Pod, error) {
	podSelector := fmt.Sprintf("component=%s", a.ComponentName)
	watchOptions := metav1.ListOptions{
		LabelSelector: podSelector,
	}
	// Wait for Pod to be in running state otherwise we can't sync data to it.
	pod, err := a.Client.WaitAndGetPod(watchOptions, corev1.PodRunning, "Waiting for component to start", hideSpinner)
	if err != nil {
		return nil, errors.Wrapf(err, "error while waiting for pod %s", podSelector)
	}
	return pod, nil
}

// Executes all the commands from the devfile in order: init and build - which are both optional, and a compulsary run.
// Init only runs once when the component is created.
func (a Adapter) execDevfile(commandsMap common.PushCommandsMap, componentExists, show bool, podName string, containers []corev1.Container, isDebug bool) (err error) {
	// If nothing has been passed, then the devfile is missing the required run command
	if len(commandsMap) == 0 {
		return errors.New(fmt.Sprint("error executing devfile commands - there should be at least 1 command"))
	}

	compInfo := common.ComponentInfo{
		PodName: podName,
	}

	// only execute Init command, if it is first run of container.
	if !componentExists {

		// Get Init Command
		command, ok := commandsMap[versionsCommon.InitCommandGroupType]
		if ok {
			compInfo.ContainerName = command.Exec.Component
			err = exec.ExecuteDevfileBuildAction(&a.Client, *command.Exec, command.Exec.Id, compInfo, show, a.machineEventLogger)
			if err != nil {
				return err
			}

		}

	}

	// Get Build Command
	command, ok := commandsMap[versionsCommon.BuildCommandGroupType]
	if ok {
		compInfo.ContainerName = command.Exec.Component
		err = exec.ExecuteDevfileBuildAction(&a.Client, *command.Exec, command.Exec.Id, compInfo, show, a.machineEventLogger)
		if err != nil {
			return err
		}
	}

	// Get Run or Debug Command
	if isDebug {
		command, ok = commandsMap[versionsCommon.DebugCommandGroupType]
	} else {
		command, ok = commandsMap[versionsCommon.RunCommandGroupType]
	}
	if ok {
		klog.V(4).Infof("Executing devfile command %v", command.Exec.Id)
		compInfo.ContainerName = command.Exec.Component

		// Check if the devfile debug component containers have supervisord as the entrypoint.
		// Start the supervisord if the odo component does not exist
		if !componentExists {
			err = a.InitRunContainerSupervisord(command.Exec.Component, podName, containers)
			if err != nil {
				a.machineEventLogger.ReportError(err, machineoutput.TimestampNow())
				return
			}
		}

		if componentExists && !common.IsRestartRequired(command) {
			klog.V(4).Infof("restart:false, Not restarting %v Command", command.Exec.Id)
			if isDebug {
				err = exec.ExecuteDevfileDebugActionWithoutRestart(&a.Client, *command.Exec, command.Exec.Id, compInfo, show, a.machineEventLogger)
			} else {
				err = exec.ExecuteDevfileRunActionWithoutRestart(&a.Client, *command.Exec, command.Exec.Id, compInfo, show, a.machineEventLogger)
			}
			return
		}
		if isDebug {
			err = exec.ExecuteDevfileDebugAction(&a.Client, *command.Exec, command.Exec.Id, compInfo, show, a.machineEventLogger)
		} else {
			err = exec.ExecuteDevfileRunAction(&a.Client, *command.Exec, command.Exec.Id, compInfo, show, a.machineEventLogger)
		}

	}

	return
}

// InitRunContainerSupervisord initializes the supervisord in the container if
// the container has entrypoint that is not supervisord
func (a Adapter) InitRunContainerSupervisord(containerName, podName string, containers []corev1.Container) (err error) {
	for _, container := range containers {
		if container.Name == containerName && !reflect.DeepEqual(container.Command, []string{common.SupervisordBinaryPath}) {
			command := []string{common.SupervisordBinaryPath, "-c", common.SupervisordConfFile, "-d"}
			compInfo := common.ComponentInfo{
				ContainerName: containerName,
				PodName:       podName,
			}
			err = exec.ExecuteCommand(&a.Client, compInfo, command, true, nil, nil)
		}
	}

	return
}

// getFirstContainerWithSourceVolume returns the first container that set mountSources: true
// Because the source volume is shared across all components that need it, we only need to sync once,
// so we only need to find one container. If no container was found, that means there's no
// container to sync to, so return an error
func getFirstContainerWithSourceVolume(containers []corev1.Container) (string, error) {
	for _, c := range containers {
		for _, vol := range c.VolumeMounts {
			if vol.Name == kclient.OdoSourceVolume {
				return c.Name, nil
			}
		}
	}

	return "", fmt.Errorf("In order to sync files, odo requires at least one component in a devfile to set 'mountSources: true'")
}

// Delete deletes the component
func (a Adapter) Delete(labels map[string]string) error {
	if !utils.ComponentExists(a.Client, a.ComponentName) {
		return errors.Errorf("the component %s doesn't exist on the cluster", a.ComponentName)
	}

	return a.Client.DeleteDeployment(labels)
}
