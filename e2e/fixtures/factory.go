/*
 * factory.go
 *
 * This source file is part of the FoundationDB open source project
 *
 * Copyright 2023 Apple Inc. and the FoundationDB project authors
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package fixtures

import (
	"bytes"
	ctx "context"
	"fmt"
	"io"
	"log"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	fdbv1beta2 "github.com/FoundationDB/fdb-kubernetes-operator/api/v1beta2"
	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/duration"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	"k8s.io/kubectl/pkg/scheme"
	"k8s.io/utils/pointer"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	dashes = "--------------------------------------------------------------------------------"
)

// Factory is a helper struct to organize tests.
type Factory struct {
	*singleton
	shutdownHooks          ShutdownHooks
	chaosExperiments       []ChaosMeshExperiment
	invariantShutdownHooks ShutdownHooks
	beforeVersion          string
	shutdownInProgress     bool
	options                *FactoryOptions
}

// CreateFactory will create a factory based on the provided options.
func CreateFactory(options *FactoryOptions) *Factory {
	singleton, err := getSingleton(options)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())

	return &Factory{
		singleton:              singleton,
		options:                options,
		shutdownHooks:          ShutdownHooks{},
		invariantShutdownHooks: ShutdownHooks{},
	}
}

func (factory *Factory) addChaosExperiment(chaosExperiment ChaosMeshExperiment) {
	factory.chaosExperiments = append(factory.chaosExperiments, chaosExperiment)
}

// StopInvariantCheck will stop the current invariant checker by triggering the shutdown hook and resetting the shutdown hooks.
func (factory *Factory) StopInvariantCheck() {
	factory.invariantShutdownHooks.InvokeShutdownHandlers()
	// Reset the invariant shutdown hooks
	factory.invariantShutdownHooks = ShutdownHooks{}
}

// AddShutdownHook will add the provided shut down hook.
func (factory *Factory) AddShutdownHook(f func() error) {
	factory.shutdownHooks.Defer(f)
}

// GetChaosNamespace returns the chaos namespace that was provided per command line.
func (factory *Factory) GetChaosNamespace() string {
	return factory.options.chaosNamespace
}

func (factory *Factory) getCertificate() *corev1.Secret {
	return factory.singleton.certificate
}

// GetControllerRuntimeClient returns the controller runtime client.
func (factory *Factory) GetControllerRuntimeClient() client.Client {
	return factory.singleton.controllerRuntimeClient
}

// GetSecretName returns the secret name that contains the certificates used for the current test run.
func (factory *Factory) GetSecretName() string {
	return factory.singleton.certificate.Name
}

// GetBackupSecretName returns the name of the backup secret.
func (factory *Factory) GetBackupSecretName() string {
	return "backup-credentials"
}

func (factory *Factory) getConfig() *rest.Config {
	return factory.singleton.config
}

func (factory *Factory) getClient() *kubernetes.Clientset {
	return factory.singleton.client
}

// DeletePod deletes the provided Pod
func (factory *Factory) DeletePod(pod *corev1.Pod) {
	gomega.Expect(factory.GetControllerRuntimeClient().Delete(ctx.TODO(), pod)).NotTo(gomega.HaveOccurred())
}

// GetPod returns the Pod matching the namespace and name
func (factory *Factory) GetPod(namespace string, name string) (*corev1.Pod, error) {
	pod := &corev1.Pod{}
	err := factory.GetControllerRuntimeClient().Get(ctx.Background(), client.ObjectKey{Name: name, Namespace: namespace}, pod)

	return pod, err
}

// GetFDBVersion returns the parsed FDB version.
func (factory *Factory) GetFDBVersion() fdbv1beta2.Version {
	return factory.singleton.fdbVersion
}

// GetFDBVersionAsString returns the FDB version as string.
func (factory *Factory) GetFDBVersionAsString() string {
	return factory.options.fdbVersion
}

// ChaosTestsEnabled returns true if chaos tests should be executed.
func (factory *Factory) ChaosTestsEnabled() bool {
	return factory.options.enableChaosTests
}

// CreateFdbCluster creates a FDB cluster.
func (factory *Factory) CreateFdbCluster(
	config *ClusterConfig,
	options ...ClusterOption,
) *FdbCluster {
	spec := factory.GenerateFDBClusterSpec(config)
	return factory.CreateFdbClusterFromSpec(spec, config, options...)
}

// CreateFdbClusterFromSpec creates a FDB cluster. This method can be used in combination with the GenerateFDBClusterSpec method.
// In general this should only be used for special cases that are not covered by changing the ClusterOptions or the ClusterConfig.
func (factory *Factory) CreateFdbClusterFromSpec(
	spec *fdbv1beta2.FoundationDBCluster,
	config *ClusterConfig,
	options ...ClusterOption,
) *FdbCluster {
	startTime := time.Now()
	config.SetDefaults(factory)
	log.Printf("create cluster: %s", ToJSON(spec))

	cluster := factory.startFDBFromClusterSpec(spec, config, options...)
	log.Println(
		"FoundationDB cluster created (at version",
		cluster.cluster.Spec.Version,
		") in minutes",
		time.Since(startTime).Minutes(),
	)

	return cluster
}

// CreateFdbHaCluster creates a HA FDB Cluster based on the cluster config and cluster options
func (factory *Factory) CreateFdbHaCluster(
	config *ClusterConfig,
	options ...ClusterOption,
) *HaFdbCluster {
	startTime := time.Now()
	config.SetDefaults(factory)

	cluster, err := factory.ensureHAFdbClusterExists(
		config,
		options,
	)

	log.Println(
		"FoundationDB HA cluster created (at version",
		cluster.GetPrimary().cluster.Spec.Version,
		") in minutes",
		time.Since(startTime).Minutes(),
	)

	gomega.Expect(err).ToNot(gomega.HaveOccurred())

	return cluster
}

func (factory *Factory) getContainerOverrides(
	debugSymbols bool,
) (fdbv1beta2.ContainerOverrides, fdbv1beta2.ContainerOverrides) {
	mainImage, mainTag := GetBaseImageAndTag(
		GetDebugImage(debugSymbols, factory.GetFoundationDBImage()),
	)

	mainOverrides := fdbv1beta2.ContainerOverrides{
		EnableTLS: false,
		// The first entry is version specific e.g. this image + tag (if specified) will be used for the provided version
		// the second entry ensures we set the base image for e.g. upgrades independent of the version.
		ImageConfigs: []fdbv1beta2.ImageConfig{
			{
				BaseImage: mainImage,
				Tag:       mainTag,
				Version:   factory.GetFDBVersionAsString(),
			},
			{
				BaseImage: mainImage,
			},
		},
	}

	sidecarImage, sidecarTag := GetBaseImageAndTag(
		GetDebugImage(debugSymbols, factory.GetSidecarImage()),
	)
	sidecarOverrides := fdbv1beta2.ContainerOverrides{
		EnableTLS: false,
		ImageConfigs: []fdbv1beta2.ImageConfig{
			{
				BaseImage: sidecarImage,
				Tag:       sidecarTag,
				Version:   factory.GetFDBVersionAsString(),
			},
			{
				BaseImage: sidecarImage,
				TagSuffix: "-1",
			},
		},
	}

	// If no tag is specified ensure we add the required tag suffix.
	if sidecarTag == "" {
		sidecarOverrides.ImageConfigs[0].TagSuffix = "-1"
	}

	return mainOverrides, sidecarOverrides
}

func (factory *Factory) getClusterPrefix() string {
	prefix := factory.options.prefix
	if prefix == "" {
		return fmt.Sprintf("fdb-cluster-%s", RandStringRunes(8))
	}
	return prefix
}

// GetDefaultStorageClass returns either the StorageClass provided by the command line or fetches the StorageClass passed on
// the default Annotation.
func (factory *Factory) GetDefaultStorageClass() string {
	flagStorageClass := factory.options.storageClass
	// If a storage class is provided as parameter use that storage class.
	if flagStorageClass != "" {
		return flagStorageClass
	}

	// If no storage class is provided use the default one in the cluster
	storageClasses := factory.GetStorageClasses(nil)

	for _, storageClass := range storageClasses.Items {
		if _, ok := storageClass.Annotations["storageclass.kubernetes.io/is-default-class"]; ok {
			return storageClass.Name
		}
	}

	// If we are here we don't have a StorageClass provided as flag or found the default storage class
	gomega.Expect(
		fmt.Errorf(
			"no default storage class provided and not default storage class found in Kubernetes cluster",
		),
	).ToNot(gomega.HaveOccurred())
	return ""
}

// GetContext returns the Kubernetes context provided via command line.
func (factory *Factory) GetContext() string {
	return factory.options.context
}

// GetStorageClasses returns all StorageClasses present in this Kubernetes cluster that have the label foundationdb.org/operator-testing=true.
func (factory *Factory) GetStorageClasses(labels map[string]string) *storagev1.StorageClassList {
	storageClasses := &storagev1.StorageClassList{}
	gomega.Expect(
		factory.GetControllerRuntimeClient().List(ctx.TODO(), storageClasses, client.MatchingLabels(labels))).NotTo(gomega.HaveOccurred())

	return storageClasses
}

// Shutdown executes all the shutdown handlers, usually called in afterSuite or afterTest depending on your scoping of the factory.
func (factory *Factory) Shutdown() {
	factory.shutdownInProgress = true
	// If the cleanup flag is present don't do any cleanup
	if !factory.options.cleanup {
		return
	}

	// Wait 15 seconds before running all shutdown handlers to ensure everything can catch up.
	time.Sleep(15 * time.Second)
	err := factory.CleanupChaosMeshExperiments()
	if err != nil {
		return
	}

	factory.invariantShutdownHooks.InvokeShutdownHandlers()
	factory.shutdownHooks.InvokeShutdownHandlers()
}

// Get returns the (eventually consistent) status of this cluster.  This is used when bootstrapping an
// FdbCluster object, so it's a member of FdbOperatorClient.
func (factory *Factory) getClusterStatus(
	name string,
	namespace string,
) (*fdbv1beta2.FoundationDBCluster, error) {
	clusterRequest := &fdbv1beta2.FoundationDBCluster{}
	err := factory.GetControllerRuntimeClient().
		Get(ctx.Background(), client.ObjectKey{
			Name:      name,
			Namespace: namespace}, clusterRequest)
	if err != nil {
		return nil, err
	}

	return clusterRequest, nil
}

// DoesPodExist checks to see if Kubernetes still knows about this pod.
func (factory *Factory) DoesPodExist(pod corev1.Pod) (bool, error) {
	_, err := factory.GetPod(pod.Namespace, pod.Name)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}

	return true, nil
}

func (factory *Factory) logClusterInfo(spec *fdbv1beta2.FoundationDBCluster) {
	processCounts, _ := spec.GetProcessCountsWithDefaults()
	log.Println(dashes)
	log.Printf("process counts: %s", ToJSON(processCounts))
	log.Println(dashes)

	storagePodSpec := spec.GetProcessSettings("storage")
	log.Printf("storage pod: %s", ToJSON(storagePodSpec))
	log.Println(dashes)

	statelessPodSpec := spec.GetProcessSettings("stateless")
	log.Printf("stateless pod: %s", ToJSON(statelessPodSpec))
	log.Println(dashes)

	logPodSpec := spec.GetProcessSettings("log")
	log.Printf("log pod: %s", ToJSON(logPodSpec))
	log.Println(dashes)
}

func (factory *Factory) startFDBFromClusterSpec(
	spec *fdbv1beta2.FoundationDBCluster,
	config *ClusterConfig,
	options ...ClusterOption,
) *FdbCluster {
	spec = spec.DeepCopy()
	for _, option := range options {
		option(factory, spec)
	}

	factory.logClusterInfo(spec)

	fdbCluster, err := factory.ensureFdbClusterExists(spec, config)
	gomega.Expect(err).ToNot(gomega.HaveOccurred())
	factory.singleton.namespaces = append(
		factory.singleton.namespaces,
		fdbCluster.cluster.Namespace,
	)

	gomega.Expect(fdbCluster.WaitUntilAvailable()).ToNot(gomega.HaveOccurred())
	return fdbCluster
}

// ClusterOption provides a fluid mechanism for chaining together options for
// building clusters.
type ClusterOption func(*Factory, *fdbv1beta2.FoundationDBCluster)

// ExecuteCmdOnPod runs a command on the provided Pod. The command will be executed inside a bash -c ”.
func (factory *Factory) ExecuteCmdOnPod(
	pod *corev1.Pod,
	container string,
	command string,
	printOutput bool,
) (string, string, error) {
	return factory.ExecuteCmd(pod.Namespace, pod.Name, container, command, printOutput)
}

// ExecuteCmd executes command in the default container of a Pod with shell, returns stdout and stderr.
func (factory *Factory) ExecuteCmd(
	namespace string,
	name string,
	container string,
	command string,
	printOutput bool,
) (string, string, error) {
	cmd := []string{
		"/bin/bash",
		"-c",
		command,
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	err := factory.ExecuteCommandRaw(namespace, name, container, cmd, nil, &stdout, &stderr, false)
	sout := stdout.String()
	serr := stderr.String()
	// TODO: Stream these to our own stdout as we run.
	if printOutput {
		if sout != "" && !strings.Contains(serr, "constructing many client") {
			log.Println(sout)
		}
		// Callers of this used to skip printing serr if err was nil, but we never populate serr
		// if err is nil; always print for now.
		if serr != "" &&
			!strings.Contains(
				serr,
				"constructing many client",
			) { // ignoring constructing many client message
			log.Println(serr)
		}
	}
	return sout, serr, err
}

// ExecuteCommandRaw will run the command without putting it into a shell.
func (factory *Factory) ExecuteCommandRaw(
	namespace string,
	name string,
	container string,
	command []string,
	stdin io.Reader,
	stdout io.Writer,
	stderr io.Writer,
	isTty bool,
) error {
	req := factory.getClient().CoreV1().RESTClient().Post().
		Resource("pods").Name(name).
		Namespace(namespace).SubResource("exec")
	option := &corev1.PodExecOptions{
		Command:   command,
		Container: container,
		Stdin:     stdin != nil,
		Stdout:    stdout != nil,
		Stderr:    stderr != nil,
		TTY:       isTty,
	}
	req.VersionedParams(
		option,
		scheme.ParameterCodec,
	)
	exec, err := remotecommand.NewSPDYExecutor(factory.getConfig(), "POST", req.URL())
	if err != nil {
		return err
	}
	return exec.Stream(remotecommand.StreamOptions{
		Stdin:  stdin,
		Stdout: stdout,
		Stderr: stderr,
	})
}

// GetLogsFromPod returns the logs for the provided Pod and container
func (factory *Factory) GetLogsFromPod(pod *corev1.Pod, container string) string {
	req := factory.getClient().CoreV1().RESTClient().Get().
		Namespace(pod.Namespace).
		Name(pod.Name).
		Resource("pods").
		SubResource("log").
		Param("container", container)
	readCloser, err := req.Stream(ctx.Background())
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	defer func() { _ = readCloser.Close() }()
	var out bytes.Buffer
	_, err = io.Copy(&out, readCloser)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	return out.String()
}

// GetDefaultLabels returns the default labels set to all resources.
func (factory *Factory) GetDefaultLabels() map[string]string {
	return map[string]string{
		"foundationdb.org/testing": "chaos",
		"foundationdb.org/user":    factory.options.username,
	}
}

// SetBeforeVersion allows a user to overwrite the before version that should be used.
func (factory *Factory) SetBeforeVersion(version string) {
	factory.beforeVersion = version
}

// GetBeforeVersion returns the before version if set. This is used during upgrade tests.
func (factory *Factory) GetBeforeVersion() string {
	return factory.beforeVersion
}

// GetAdditionalSidecarVersions returns all additional FoundationDB versions that should be added to the sidecars. This
// method make sure that the operator has all required client libraries.
func (factory *Factory) GetAdditionalSidecarVersions() []fdbv1beta2.Version {
	compactVersionMap := map[string]fdbv1beta2.Version{}
	baseVersion := factory.GetFDBVersion()

	additionalVersions := make([]fdbv1beta2.Version, 0)
	for _, version := range getUpgradeVersions(factory.options.upgradeString) {
		updateVersionMapIfVersionIsMissingOrNewer(baseVersion, compactVersionMap, version.InitialVersion)
		updateVersionMapIfVersionIsMissingOrNewer(baseVersion, compactVersionMap, version.TargetVersion)
	}

	for _, version := range compactVersionMap {
		additionalVersions = append(additionalVersions, version)
	}

	return additionalVersions
}

// This method will update the provided map if the compact version of newVersion is either missing or the provided newVersion
// is newer than the current version in the map.
func updateVersionMapIfVersionIsMissingOrNewer(baseVersion fdbv1beta2.Version, versions map[string]fdbv1beta2.Version, newVersion fdbv1beta2.Version) {
	// Since we already include the base version we can skip all compatible versions
	if newVersion.Compact() == baseVersion.Compact() {
		return
	}

	currentVersion, ok := versions[newVersion.Compact()]
	if !ok {
		// If we don't have a version for this compact version we add it here
		versions[newVersion.Compact()] = newVersion
		return
	}

	// If the version in our map is newer we skip the current version
	if currentVersion.IsAtLeast(newVersion) {
		return
	}

	versions[newVersion.Compact()] = newVersion
}

// writePodInformation will write the Pod information from the provided Pod into a string.
func writePodInformation(pod corev1.Pod) string {
	var buffer strings.Builder
	var containers, readyContainers, restarts int
	for _, conStatus := range pod.Status.ContainerStatuses {
		containers++
		if conStatus.Ready {
			readyContainers++
		}

		restarts += int(conStatus.RestartCount)
	}

	buffer.WriteString(pod.GetName())
	buffer.WriteString("\t")
	buffer.WriteString(strconv.Itoa(readyContainers))
	buffer.WriteString("/")
	buffer.WriteString(strconv.Itoa(containers))
	buffer.WriteString("\t")
	buffer.WriteString(string(pod.Status.Phase))

	if pod.Status.Phase == corev1.PodPending {
		for _, condition := range pod.Status.Conditions {
			// Only check the PodScheduled condition.
			if condition.Type != corev1.PodScheduled {
				continue
			}

			// If the Pod is scheduled we can ignore this condition.
			if condition.Status == corev1.ConditionTrue {
				buffer.WriteString("\t-")
				continue
			}

			// Printout the message, why the Pod is not scheduling.
			buffer.WriteString("\t")
			if condition.Message != "" {
				buffer.WriteString(condition.Message)
			} else {
				buffer.WriteString("-")
			}
		}
	} else {
		buffer.WriteString("\t-")
	}

	buffer.WriteString("\t")
	buffer.WriteString(strconv.Itoa(restarts))

	if _, ok := pod.Labels[fdbv1beta2.FDBProcessGroupIDLabel]; ok {
		var mainTag, sidecarTag string
		for _, container := range pod.Spec.Containers {
			if container.Name == fdbv1beta2.MainContainerName {
				mainTag = strings.Split(container.Image, ":")[1]
				continue
			}

			if container.Name == fdbv1beta2.SidecarContainerName {
				sidecarTag = strings.Split(container.Image, ":")[1]
				continue
			}
		}

		buffer.WriteString("\t")
		buffer.WriteString(mainTag)
		buffer.WriteString("\t")
		buffer.WriteString(sidecarTag)
	} else {
		buffer.WriteString("\t-\t-")
	}

	buffer.WriteString("\t")
	endIdx := len(pod.Status.PodIPs) - 1
	for idx, ip := range pod.Status.PodIPs {
		buffer.WriteString(ip.IP)
		if endIdx > idx {
			buffer.WriteString(",")
		}
	}

	buffer.WriteString("\t")
	buffer.WriteString(pod.Spec.NodeName)
	buffer.WriteString("\t")
	buffer.WriteString(duration.HumanDuration(time.Since(pod.CreationTimestamp.Time)))

	return buffer.String()
}

// DumpState writes the state of the cluster to the log output. Useful for debugging test failures.
func (factory *Factory) DumpState(fdbCluster *FdbCluster) {
	if fdbCluster == nil || !factory.options.dumpOperatorState {
		return
	}

	cluster := fdbCluster.GetCluster()

	// We write the whole information into a buffer to prevent having multiple log line prefixes.
	var buffer strings.Builder
	buffer.WriteString("\n")
	buffer.WriteString("---------- ")
	buffer.WriteString(cluster.GetNamespace())
	buffer.WriteString(" ----------\n")

	buffer.WriteString(
		fmt.Sprintf(
			"%s\tGENERATION: %d\tRECONCILED: %d\tAVAILABLE: %t\tFULLREPLICATION: %t\tRUNNING_VERSION: %s\tDESIRED_VERSION: %s\t Age: %s\nConnection String: %s\n",
			cluster.GetName(),
			cluster.Generation,
			cluster.Status.Generations.Reconciled,
			cluster.Status.Health.Available,
			cluster.Status.Health.FullReplication,
			cluster.Status.RunningVersion,
			cluster.Spec.Version,
			duration.HumanDuration(time.Since(cluster.CreationTimestamp.Time)),
			cluster.Status.ConnectionString,
		),
	)
	// Printout all Pods for this namespace
	pods := &corev1.PodList{}
	err := factory.controllerRuntimeClient.List(ctx.Background(), pods, client.InNamespace(cluster.Namespace))
	if err != nil {
		log.Println(err)
		return
	}

	buffer.WriteString("---------- Pods ----------")
	log.Println(buffer.String())
	buffer.Reset()

	// Make use of a tabwriter for better output.
	w := tabwriter.NewWriter(log.Writer(), 0, 0, 1, ' ', tabwriter.Debug)
	_, _ = fmt.Fprintln(w, "Name\tReady\tSTATUS\tUnschedulable\tRestarts\tMain Image\tSidecar Image\tIPs\tNode\tAge")
	var operatorPods []corev1.Pod
	for _, pod := range pods.Items {
		if pod.Labels["app"] == "fdb-kubernetes-operator-controller-manager" {
			operatorPods = append(operatorPods, pod)
		}

		_, _ = fmt.Fprintln(w, writePodInformation(pod))
	}
	_ = w.Flush()

	log.Println(buffer.String())

	// Printout the logs of the operator Pods for the last 90 seconds.
	for _, pod := range operatorPods {
		log.Println(factory.GetLogsForPod(pod, "manager", pointer.Int64(300)))
	}
}

// GetLogsForPod will fetch the logs for the specified Pod and container since the provided seconds.
func (factory *Factory) GetLogsForPod(pod corev1.Pod, container string, since *int64) string {
	req := factory.getClient().CoreV1().
		Pods(pod.Namespace).
		GetLogs(pod.Name, &corev1.PodLogOptions{
			Container:    container,
			Follow:       false,
			SinceSeconds: since,
		})

	readCloser, err := req.Stream(ctx.Background())
	if err != nil {
		log.Println(err)
		return ""
	}

	logs, err := io.ReadAll(readCloser)
	if err != nil {
		log.Println(err)
		_ = readCloser.Close()
		return ""
	}
	if len(logs) == 0 {
		return ""
	}

	return string(logs)
}

// DumpStateHaCluster can be used to dump the state of the HA cluster. This includes the Kubernetes custom resource
// information as well as the operator logs and the Pod state.
func (factory *Factory) DumpStateHaCluster(fdbCluster *HaFdbCluster) {
	for _, cluster := range fdbCluster.clusters {
		factory.DumpState(cluster)
	}
}

// OperatorIsAtLeast is a helper method to check is the running operator is at least in the specified version.
func (factory *Factory) OperatorIsAtLeast(version string) bool {
	operatorVersion := strings.Split(factory.GetOperatorImage(), ":")[1]
	parsedOperatorVersion, err := fdbv1beta2.ParseFdbVersion(operatorVersion)
	if err != nil {
		// If the version can not be parsed be can assume it's either a self build or the latest version.
		log.Println(
			"operator version is",
			operatorVersion,
			" so assuming all features are supported",
		)
		return true
	}

	parsedVersion, err := fdbv1beta2.ParseFdbVersion(version)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())

	log.Println("operator version", parsedOperatorVersion, "minimum version", parsedVersion)
	return parsedOperatorVersion.IsAtLeast(parsedVersion)
}

// GetClusterOptions returns the cluster options that should be used for the operator testing. Those options can be changed
// by changing the according feature flags.
func (factory *Factory) GetClusterOptions(options ...ClusterOption) []ClusterOption {
	options = append(options, WithTLSEnabled)

	if factory.options.featureOperatorUnifiedImage {
		options = append(options, WithUnifiedImage)
	}

	if factory.options.featureOperatorLocalities {
		options = append(options, WithLocalitiesForExclusion)
	}

	if factory.options.featureOperatorDNS {
		options = append(options, WithDNSEnabled)
	}

	return options
}

// PrependRegistry if a registry was provided as flag, the registry will be prepended.
func (factory *Factory) PrependRegistry(container string) string {
	return prependRegistry(factory.options.registry, container)
}

// CreateIfAbsent will create the provided resource if absent.
func (factory *Factory) CreateIfAbsent(object client.Object) error {
	objectCopy, ok := object.DeepCopyObject().(client.Object)
	if !ok {
		return fmt.Errorf("cannot copy object")
	}

	ctrlClient := factory.GetControllerRuntimeClient()
	err := ctrlClient.
		Get(
			ctx.Background(),
			client.ObjectKey{Namespace: object.GetNamespace(), Name: object.GetName()},
			objectCopy,
		)

	if err != nil {
		if k8serrors.IsNotFound(err) {
			return ctrlClient.Create(ctx.Background(), object)
		}

		return err
	}

	return nil
}

// GetOperatorImage returns the operator image provided via command line. If a registry was defined the registry will be
// prepended.
func (factory *Factory) GetOperatorImage() string {
	return prependRegistry(factory.options.registry, factory.options.operatorImage)
}

// GetDataLoaderImage returns the dataloader image provided via command line. If a registry was defined the registry will be
// prepended.
func (factory *Factory) GetDataLoaderImage() string {
	return prependRegistry(factory.options.registry, factory.options.dataLoaderImage)
}

// GetSidecarImage returns the sidecar image provided via command line. If a registry was defined the registry will be
// prepended.
func (factory *Factory) GetSidecarImage() string {
	return prependRegistry(factory.options.registry, factory.options.sidecarImage)
}

// GetFoundationDBImage returns the FoundationDB image provided via command line. If a registry was defined the registry will be
// prepended.
func (factory *Factory) GetFoundationDBImage() string {
	return prependRegistry(factory.options.registry, factory.options.fdbImage)
}

// getImagePullPolicy returns the image pull policy based on the provided cloud provider. For Kind this will be Never, otherwise
// this will Always.
func (factory *Factory) getImagePullPolicy() corev1.PullPolicy {
	if strings.ToLower(factory.options.cloudProvider) == cloudProviderKind {
		return corev1.PullNever
	}

	return corev1.PullAlways
}

// UpdateNode update node definition
func (fdbCluster *FdbCluster) UpdateNode(node *corev1.Node) {
	gomega.Eventually(func() bool {
		err := fdbCluster.getClient().Update(ctx.Background(), node)
		return err == nil
	}).WithTimeout(time.Duration(2) * time.Minute).WithPolling(2 * time.Second).Should(gomega.BeTrue())
}

// GetNode return Node with the given name
func (fdbCluster *FdbCluster) GetNode(name string) *corev1.Node {
	// Retry if for some reasons an error is returned
	node := &corev1.Node{}
	gomega.Eventually(func() error {
		return fdbCluster.getClient().
			Get(ctx.TODO(), client.ObjectKey{Name: name}, node)
	}).WithTimeout(2 * time.Minute).WithPolling(1 * time.Second).ShouldNot(gomega.HaveOccurred())

	return node
}
