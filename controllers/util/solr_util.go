/*
 * Licensed to the Apache Software Foundation (ASF) under one or more
 * contributor license agreements.  See the NOTICE file distributed with
 * this work for additional information regarding copyright ownership.
 * The ASF licenses this file to You under the Apache License, Version 2.0
 * (the "License"); you may not use this file except in compliance with
 * the License.  You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package util

import (
	"crypto/sha256"
	b64 "encoding/base64"
	"encoding/json"
	"fmt"
	"math/rand"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	solr "github.com/apache/solr-operator/api/v1beta1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	netv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

const (
	SolrClientPortName = "solr-client"

	SolrNodeContainer = "solrcloud-node"

	DefaultSolrUser  = 8983
	DefaultSolrGroup = 8983

	SolrStorageFinalizer             = "storage.finalizers.solr.apache.org"
	SolrZKConnectionStringAnnotation = "solr.apache.org/zkConnectionString"
	SolrPVCTechnologyLabel           = "solr.apache.org/technology"
	SolrCloudPVCTechnology           = "solr-cloud"
	SolrPVCStorageLabel              = "solr.apache.org/storage"
	SolrCloudPVCDataStorage          = "data"
	SolrPVCInstanceLabel             = "solr.apache.org/instance"
	SolrXmlMd5Annotation             = "solr.apache.org/solrXmlMd5"
	SolrXmlFile                      = "solr.xml"
	LogXmlMd5Annotation              = "solr.apache.org/logXmlMd5"
	LogXmlFile                       = "log4j2.xml"
	SecurityJsonFile                 = "security.json"
	BasicAuthMd5Annotation           = "solr.apache.org/basicAuthMd5"
	DefaultProbePath                 = "/admin/info/system"

	DefaultStatefulSetPodManagementPolicy = appsv1.ParallelPodManagement
)

// GenerateStatefulSet returns a new appsv1.StatefulSet pointer generated for the SolrCloud instance
// object: SolrCloud instance
// replicas: the number of replicas for the SolrCloud instance
// storage: the size of the storage for the SolrCloud instance (e.g. 100Gi)
// zkConnectionString: the connectionString of the ZK instance to connect to
func GenerateStatefulSet(solrCloud *solr.SolrCloud, solrCloudStatus *solr.SolrCloudStatus, hostNameIPs map[string]string, reconcileConfigInfo map[string]string, tls *TLSCerts) *appsv1.StatefulSet {
	terminationGracePeriod := int64(60)
	solrPodPort := solrCloud.Spec.SolrAddressability.PodPort
	fsGroup := int64(DefaultSolrGroup)

	probeScheme := corev1.URISchemeHTTP
	if tls != nil {
		probeScheme = corev1.URISchemeHTTPS
	}

	defaultProbeTimeout := int32(1)
	defaultHandler := corev1.Handler{
		HTTPGet: &corev1.HTTPGetAction{
			Scheme: probeScheme,
			Path:   "/solr" + DefaultProbePath,
			Port:   intstr.FromInt(solrPodPort),
		},
	}

	labels := solrCloud.SharedLabelsWith(solrCloud.GetLabels())
	selectorLabels := solrCloud.SharedLabels()

	labels["technology"] = solr.SolrTechnologyLabel
	selectorLabels["technology"] = solr.SolrTechnologyLabel

	annotations := map[string]string{
		SolrZKConnectionStringAnnotation: solrCloudStatus.ZkConnectionString(),
	}

	podLabels := labels

	customSSOptions := solrCloud.Spec.CustomSolrKubeOptions.StatefulSetOptions
	if nil != customSSOptions {
		labels = MergeLabelsOrAnnotations(labels, customSSOptions.Labels)
		annotations = MergeLabelsOrAnnotations(annotations, customSSOptions.Annotations)
	}

	customPodOptions := solrCloud.Spec.CustomSolrKubeOptions.PodOptions
	var podAnnotations map[string]string
	if nil != customPodOptions {
		podLabels = MergeLabelsOrAnnotations(podLabels, customPodOptions.Labels)
		podAnnotations = customPodOptions.Annotations

		if customPodOptions.TerminationGracePeriodSeconds != nil {
			terminationGracePeriod = *customPodOptions.TerminationGracePeriodSeconds
		}
	}

	// Keep track of the SolrOpts that the Solr Operator needs to set
	// These will be added to the SolrOpts given by the user.
	allSolrOpts := []string{"-DhostPort=$(SOLR_NODE_PORT)"}

	// Volumes & Mounts
	solrVolumes := []corev1.Volume{
		{
			Name: "solr-xml",
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: reconcileConfigInfo[SolrXmlFile],
					},
					Items: []corev1.KeyToPath{
						{
							Key:  SolrXmlFile,
							Path: SolrXmlFile,
						},
					},
					DefaultMode: &PublicReadOnlyPermissions,
				},
			},
		},
	}

	solrDataVolumeName := "data"
	volumeMounts := []corev1.VolumeMount{{Name: solrDataVolumeName, MountPath: "/var/solr/data"}}

	var pvcs []corev1.PersistentVolumeClaim
	if solrCloud.UsesPersistentStorage() {
		pvc := solrCloud.Spec.StorageOptions.PersistentStorage.PersistentVolumeClaimTemplate.DeepCopy()

		// Set the default name of the pvc
		if pvc.ObjectMeta.Name == "" {
			pvc.ObjectMeta.Name = solrDataVolumeName
		}

		// Set some defaults in the PVC Spec
		if len(pvc.Spec.AccessModes) == 0 {
			pvc.Spec.AccessModes = []corev1.PersistentVolumeAccessMode{
				corev1.ReadWriteOnce,
			}
		}
		if pvc.Spec.VolumeMode == nil {
			temp := corev1.PersistentVolumeFilesystem
			pvc.Spec.VolumeMode = &temp
		}

		//  Add internally-used labels.
		internalLabels := map[string]string{
			SolrPVCTechnologyLabel: SolrCloudPVCTechnology,
			SolrPVCStorageLabel:    SolrCloudPVCDataStorage,
			SolrPVCInstanceLabel:   solrCloud.Name,
		}
		pvc.ObjectMeta.Labels = MergeLabelsOrAnnotations(internalLabels, pvc.ObjectMeta.Labels)

		pvcs = []corev1.PersistentVolumeClaim{
			{
				ObjectMeta: metav1.ObjectMeta{
					Name:        pvc.ObjectMeta.Name,
					Labels:      pvc.ObjectMeta.Labels,
					Annotations: pvc.ObjectMeta.Annotations,
				},
				Spec: pvc.Spec,
			},
		}
	} else {
		ephemeralVolume := corev1.Volume{
			Name:         solrDataVolumeName,
			VolumeSource: corev1.VolumeSource{},
		}
		if solrCloud.Spec.StorageOptions.EphemeralStorage != nil {
			if nil != solrCloud.Spec.StorageOptions.EphemeralStorage.HostPath {
				ephemeralVolume.VolumeSource.HostPath = solrCloud.Spec.StorageOptions.EphemeralStorage.HostPath
			} else if nil != solrCloud.Spec.StorageOptions.EphemeralStorage.EmptyDir {
				ephemeralVolume.VolumeSource.EmptyDir = solrCloud.Spec.StorageOptions.EphemeralStorage.EmptyDir
			} else {
				ephemeralVolume.VolumeSource.EmptyDir = &corev1.EmptyDirVolumeSource{}
			}
		} else {
			ephemeralVolume.VolumeSource.EmptyDir = &corev1.EmptyDirVolumeSource{}
		}
		solrVolumes = append(solrVolumes, ephemeralVolume)
	}

	// Add necessary specs for backupRepos
	for _, repo := range solrCloud.Spec.BackupRepositories {
		volumeSource, mount := RepoVolumeSourceAndMount(&repo, solrCloud.Name)
		if volumeSource != nil {
			solrVolumes = append(solrVolumes, corev1.Volume{
				Name:         RepoVolumeName(&repo),
				VolumeSource: *volumeSource,
			})
			mount.Name = RepoVolumeName(&repo)
			volumeMounts = append(volumeMounts, *mount)
		}
	}

	if nil != customPodOptions {
		// Add Custom Volumes to pod
		for _, volume := range customPodOptions.Volumes {
			// Only add the container mount if one has been provided.
			if volume.DefaultContainerMount != nil {
				volume.DefaultContainerMount.Name = volume.Name
				volumeMounts = append(volumeMounts, *volume.DefaultContainerMount)
			}

			solrVolumes = append(solrVolumes, corev1.Volume{
				Name:         volume.Name,
				VolumeSource: volume.Source,
			})
		}
	}

	// Host Aliases
	hostAliases := make([]corev1.HostAlias, len(hostNameIPs))
	if len(hostAliases) == 0 {
		hostAliases = nil
	} else {
		hostNames := make([]string, len(hostNameIPs))
		index := 0
		for hostName := range hostNameIPs {
			hostNames[index] = hostName
			index += 1
		}

		sort.Strings(hostNames)

		for index, hostName := range hostNames {
			hostAliases[index] = corev1.HostAlias{
				IP:        hostNameIPs[hostName],
				Hostnames: []string{hostName},
			}
			index++
		}
	}

	solrHostName := solrCloud.AdvertisedNodeHost("$(POD_HOSTNAME)")
	solrAdressingPort := solrCloud.NodePort()

	// Solr can take longer than SOLR_STOP_WAIT to run solr stop, give it a few extra seconds before forcefully killing the pod.
	solrStopWait := terminationGracePeriod - 5
	if solrStopWait < 0 {
		solrStopWait = 0
	}

	// Environment Variables
	envVars := []corev1.EnvVar{
		{
			Name:  "SOLR_JAVA_MEM",
			Value: solrCloud.Spec.SolrJavaMem,
		},
		{
			Name:  "SOLR_HOME",
			Value: "/var/solr/data",
		},
		{
			// This is the port that jetty will listen on
			Name:  "SOLR_PORT",
			Value: strconv.Itoa(solrPodPort),
		},
		{
			// This is the port that the Solr Node will advertise itself as listening on in live_nodes
			Name:  "SOLR_NODE_PORT",
			Value: strconv.Itoa(solrAdressingPort),
		},
		{
			Name: "POD_HOSTNAME",
			ValueFrom: &corev1.EnvVarSource{
				FieldRef: &corev1.ObjectFieldSelector{
					FieldPath:  "metadata.name",
					APIVersion: "v1",
				},
			},
		},
		{
			Name:  "SOLR_HOST",
			Value: solrHostName,
		},
		{
			Name:  "SOLR_LOG_LEVEL",
			Value: solrCloud.Spec.SolrLogLevel,
		},
		{
			Name:  "GC_TUNE",
			Value: solrCloud.Spec.SolrGCTune,
		},
		{
			Name:  "SOLR_STOP_WAIT",
			Value: strconv.FormatInt(solrStopWait, 10),
		},
	}

	// Add all necessary information for connection to Zookeeper
	zkEnvVars, zkSolrOpt, hasChroot := createZkConnectionEnvVars(solrCloud, solrCloudStatus)
	if zkSolrOpt != "" {
		allSolrOpts = append(allSolrOpts, zkSolrOpt)
	}
	envVars = append(envVars, zkEnvVars...)

	// Only have a postStart command to create the chRoot, if it is not '/' (which does not need to be created)
	var postStart *corev1.Handler
	if hasChroot {
		postStart = &corev1.Handler{
			Exec: &corev1.ExecAction{
				Command: []string{"sh", "-c", "solr zk ls ${ZK_CHROOT} -z ${ZK_SERVER} || solr zk mkroot ${ZK_CHROOT} -z ${ZK_SERVER}"},
			},
		}
	}

	// Default preStop hook
	preStop := &corev1.Handler{
		Exec: &corev1.ExecAction{
			Command: []string{"solr", "stop", "-p", strconv.Itoa(solrPodPort)},
		},
	}

	// Add Custom EnvironmentVariables to the solr container
	if nil != customPodOptions {
		envVars = append(envVars, customPodOptions.EnvVariables...)
	}

	// Did the user provide a custom log config?
	if reconcileConfigInfo[LogXmlFile] != "" {
		if reconcileConfigInfo[LogXmlMd5Annotation] != "" {
			if podAnnotations == nil {
				podAnnotations = make(map[string]string, 1)
			}
			podAnnotations[LogXmlMd5Annotation] = reconcileConfigInfo[LogXmlMd5Annotation]
		}

		// cannot use /var/solr as a mountPath, so mount the custom log config
		// in a sub-dir named after the user-provided ConfigMap
		volMount, envVar, newVolume := setupVolumeMountForUserProvidedConfigMapEntry(reconcileConfigInfo, LogXmlFile, solrVolumes, "LOG4J_PROPS")
		volumeMounts = append(volumeMounts, *volMount)
		envVars = append(envVars, *envVar)
		if newVolume != nil {
			solrVolumes = append(solrVolumes, *newVolume)
		}
	}

	if (tls != nil && tls.ServerConfig != nil && tls.ServerConfig.Options.ClientAuth != solr.None) || (solrCloud.Spec.SolrSecurity != nil && solrCloud.Spec.SolrSecurity.ProbesRequireAuth) {
		probeCommand, vol, volMount := configureSecureProbeCommand(solrCloud, defaultHandler.HTTPGet)
		if vol != nil {
			solrVolumes = append(solrVolumes, *vol)
		}
		if volMount != nil {
			volumeMounts = append(volumeMounts, *volMount)
		}
		// reset the defaultHandler for the probes to invoke the SolrCLI api action instead of HTTP
		defaultHandler = corev1.Handler{Exec: &corev1.ExecAction{Command: []string{"sh", "-c", probeCommand}}}
		defaultProbeTimeout = 5
	}

	// track the MD5 of the custom solr.xml in the pod spec annotations,
	// so we get a rolling restart when the configMap changes
	if reconcileConfigInfo[SolrXmlMd5Annotation] != "" {
		if podAnnotations == nil {
			podAnnotations = make(map[string]string, 1)
		}
		podAnnotations[SolrXmlMd5Annotation] = reconcileConfigInfo[SolrXmlMd5Annotation]
	}

	if solrCloud.Spec.SolrOpts != "" {
		allSolrOpts = append(allSolrOpts, solrCloud.Spec.SolrOpts)
	}

	// Add SOLR_OPTS last, so that it can use values from all of the other ENV_VARS
	envVars = append(envVars, corev1.EnvVar{
		Name:  "SOLR_OPTS",
		Value: strings.Join(allSolrOpts, " "),
	})

	initContainers := generateSolrSetupInitContainers(solrCloud, solrCloudStatus, solrDataVolumeName, reconcileConfigInfo)

	// Add user defined additional init containers
	if customPodOptions != nil && len(customPodOptions.InitContainers) > 0 {
		initContainers = append(initContainers, customPodOptions.InitContainers...)
	}

	containers := []corev1.Container{
		{
			Name:            SolrNodeContainer,
			Image:           solrCloud.Spec.SolrImage.ToImageName(),
			ImagePullPolicy: solrCloud.Spec.SolrImage.PullPolicy,
			Ports: []corev1.ContainerPort{
				{
					ContainerPort: int32(solrPodPort),
					Name:          SolrClientPortName,
					Protocol:      "TCP",
				},
			},
			LivenessProbe: &corev1.Probe{
				InitialDelaySeconds: 20,
				TimeoutSeconds:      defaultProbeTimeout,
				SuccessThreshold:    1,
				FailureThreshold:    3,
				PeriodSeconds:       10,
				Handler:             defaultHandler,
			},
			ReadinessProbe: &corev1.Probe{
				InitialDelaySeconds: 15,
				TimeoutSeconds:      defaultProbeTimeout,
				SuccessThreshold:    1,
				FailureThreshold:    3,
				PeriodSeconds:       5,
				Handler:             defaultHandler,
			},
			VolumeMounts: volumeMounts,
			Env:          envVars,
			Lifecycle: &corev1.Lifecycle{
				PostStart: postStart,
				PreStop:   preStop,
			},
		},
	}

	// Add user defined additional sidecar containers
	if customPodOptions != nil && len(customPodOptions.SidecarContainers) > 0 {
		containers = append(containers, customPodOptions.SidecarContainers...)
	}

	// Decide which update strategy to use
	updateStrategy := appsv1.OnDeleteStatefulSetStrategyType
	if solrCloud.Spec.UpdateStrategy.Method == solr.StatefulSetUpdate {
		// Only use the rolling update strategy if the StatefulSetUpdate method is specified.
		updateStrategy = appsv1.RollingUpdateStatefulSetStrategyType
	}

	// Determine which podManagementPolicy to use for the statefulSet
	podManagementPolicy := DefaultStatefulSetPodManagementPolicy
	if solrCloud.Spec.CustomSolrKubeOptions.StatefulSetOptions != nil && solrCloud.Spec.CustomSolrKubeOptions.StatefulSetOptions.PodManagementPolicy != "" {
		podManagementPolicy = solrCloud.Spec.CustomSolrKubeOptions.StatefulSetOptions.PodManagementPolicy
	}

	// Create the Stateful Set
	stateful := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:        solrCloud.StatefulSetName(),
			Namespace:   solrCloud.GetNamespace(),
			Labels:      labels,
			Annotations: annotations,
		},
		Spec: appsv1.StatefulSetSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: selectorLabels,
			},
			ServiceName:         solrCloud.HeadlessServiceName(),
			Replicas:            solrCloud.Spec.Replicas,
			PodManagementPolicy: podManagementPolicy,
			UpdateStrategy: appsv1.StatefulSetUpdateStrategy{
				Type: updateStrategy,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      podLabels,
					Annotations: podAnnotations,
				},

				Spec: corev1.PodSpec{
					TerminationGracePeriodSeconds: &terminationGracePeriod,
					SecurityContext: &corev1.PodSecurityContext{
						FSGroup: &fsGroup,
					},
					Volumes:        solrVolumes,
					InitContainers: initContainers,
					HostAliases:    hostAliases,
					Containers:     containers,
				},
			},
			VolumeClaimTemplates: pvcs,
		},
	}

	var imagePullSecrets []corev1.LocalObjectReference

	if customPodOptions != nil {
		imagePullSecrets = customPodOptions.ImagePullSecrets
	}

	if solrCloud.Spec.SolrImage.ImagePullSecret != "" {
		imagePullSecrets = append(
			imagePullSecrets,
			corev1.LocalObjectReference{Name: solrCloud.Spec.SolrImage.ImagePullSecret},
		)
	}

	stateful.Spec.Template.Spec.ImagePullSecrets = imagePullSecrets

	if nil != customPodOptions {
		solrContainer := &stateful.Spec.Template.Spec.Containers[0]

		if customPodOptions.ServiceAccountName != "" {
			stateful.Spec.Template.Spec.ServiceAccountName = customPodOptions.ServiceAccountName
		}

		if customPodOptions.Affinity != nil {
			stateful.Spec.Template.Spec.Affinity = customPodOptions.Affinity
		}

		if customPodOptions.Resources.Limits != nil || customPodOptions.Resources.Requests != nil {
			solrContainer.Resources = customPodOptions.Resources
		}

		if customPodOptions.PodSecurityContext != nil {
			stateful.Spec.Template.Spec.SecurityContext = customPodOptions.PodSecurityContext
		}

		if customPodOptions.Lifecycle != nil {
			solrContainer.Lifecycle = customPodOptions.Lifecycle
		}

		if customPodOptions.Tolerations != nil {
			stateful.Spec.Template.Spec.Tolerations = customPodOptions.Tolerations
		}

		if customPodOptions.NodeSelector != nil {
			stateful.Spec.Template.Spec.NodeSelector = customPodOptions.NodeSelector
		}

		if customPodOptions.StartupProbe != nil {
			// Default Solr container does not contain a startupProbe, so copy the livenessProbe
			baseProbe := solrContainer.LivenessProbe.DeepCopy()
			// Two options are different by default from the livenessProbe
			baseProbe.TimeoutSeconds = 30
			baseProbe.FailureThreshold = 15
			solrContainer.StartupProbe = customizeProbe(baseProbe, *customPodOptions.StartupProbe)
		}

		if customPodOptions.LivenessProbe != nil {
			solrContainer.LivenessProbe = customizeProbe(solrContainer.LivenessProbe, *customPodOptions.LivenessProbe)
		}

		if customPodOptions.ReadinessProbe != nil {
			solrContainer.ReadinessProbe = customizeProbe(solrContainer.ReadinessProbe, *customPodOptions.ReadinessProbe)
		}

		if customPodOptions.PriorityClassName != "" {
			stateful.Spec.Template.Spec.PriorityClassName = customPodOptions.PriorityClassName
		}
	}

	// Enrich the StatefulSet config to enable TLS on Solr pods if needed
	if tls != nil {
		tls.enableTLSOnSolrCloudStatefulSet(stateful)
	}

	return stateful
}

func generateSolrSetupInitContainers(solrCloud *solr.SolrCloud, solrCloudStatus *solr.SolrCloudStatus, solrDataVolumeName string, reconcileConfigInfo map[string]string) (containers []corev1.Container) {
	// The setup of the solr.xml will always be necessary
	volumeMounts := []corev1.VolumeMount{
		{
			Name:      "solr-xml",
			MountPath: "/tmp",
		},
		{
			Name:      solrDataVolumeName,
			MountPath: "/tmp-config",
		},
	}
	setupCommands := []string{"cp /tmp/solr.xml /tmp-config/solr.xml"}

	// Add prep for backup-restore Repositories
	// This entails setting the correct permissions for the directory
	for _, repo := range solrCloud.Spec.BackupRepositories {
		if IsRepoManaged(&repo) {
			_, volumeMount := RepoVolumeSourceAndMount(&repo, solrCloud.Name)
			volumeMounts = append(volumeMounts, *volumeMount)

			setupCommands = append(setupCommands, fmt.Sprintf(
				"chown -R %d:%d %s",
				DefaultSolrUser,
				DefaultSolrGroup,
				volumeMount.MountPath))
		}
	}

	volumePrepInitContainer := corev1.Container{
		Name:            "cp-solr-xml",
		Image:           solrCloud.Spec.BusyBoxImage.ToImageName(),
		ImagePullPolicy: solrCloud.Spec.BusyBoxImage.PullPolicy,
		Command:         []string{"sh", "-c", strings.Join(setupCommands, " && ")},
		VolumeMounts:    volumeMounts,
	}

	containers = append(containers, volumePrepInitContainer)

	if hasZKSetupContainer, zkSetupContainer := generateZKInteractionInitContainer(solrCloud, solrCloudStatus, reconcileConfigInfo); hasZKSetupContainer {
		containers = append(containers, zkSetupContainer)
	}

	return containers
}

func GenerateBackupRepositoriesForSolrXml(backupRepos []solr.SolrBackupRepository) string {
	if len(backupRepos) == 0 {
		return ""
	}
	libs := make(map[string]bool, 0)
	repoXMLs := make([]string, len(backupRepos))

	for i, repo := range backupRepos {
		for _, lib := range AdditionalRepoLibs(&repo) {
			libs[lib] = true
		}
		repoXMLs[i] = RepoXML(&repo)
	}
	sort.Strings(repoXMLs)

	libXml := ""
	if len(libs) > 0 {
		libList := make([]string, 0)
		for lib := range libs {
			libList = append(libList, lib)
		}
		sort.Strings(libList)
		libXml = fmt.Sprintf("<str name=\"sharedLib\">%s</str>", strings.Join(libList, ","))
	}

	return fmt.Sprintf(
		`%s 
		<backup>
		%s
		</backup>`, libXml, strings.Join(repoXMLs, `
`))
}

const DefaultSolrXML = `<?xml version="1.0" encoding="UTF-8" ?>
<solr>
  <solrcloud>
    <str name="host">${host:}</str>
    <int name="hostPort">${hostPort:80}</int>
    <str name="hostContext">${hostContext:solr}</str>
    <bool name="genericCoreNodeNames">${genericCoreNodeNames:true}</bool>
    <int name="zkClientTimeout">${zkClientTimeout:30000}</int>
    <int name="distribUpdateSoTimeout">${distribUpdateSoTimeout:600000}</int>
    <int name="distribUpdateConnTimeout">${distribUpdateConnTimeout:60000}</int>
    <str name="zkCredentialsProvider">${zkCredentialsProvider:org.apache.solr.common.cloud.DefaultZkCredentialsProvider}</str>
    <str name="zkACLProvider">${zkACLProvider:org.apache.solr.common.cloud.DefaultZkACLProvider}</str>
  </solrcloud>
  <shardHandlerFactory name="shardHandlerFactory"
    class="HttpShardHandlerFactory">
    <int name="socketTimeout">${socketTimeout:600000}</int>
    <int name="connTimeout">${connTimeout:60000}</int>
  </shardHandlerFactory>
  %s
</solr>
`

// GenerateConfigMap returns a new corev1.ConfigMap pointer generated for the SolrCloud instance solr.xml
// solrCloud: SolrCloud instance
func GenerateConfigMap(solrCloud *solr.SolrCloud) *corev1.ConfigMap {
	labels := solrCloud.SharedLabelsWith(solrCloud.GetLabels())
	var annotations map[string]string

	customOptions := solrCloud.Spec.CustomSolrKubeOptions.ConfigMapOptions
	if nil != customOptions {
		labels = MergeLabelsOrAnnotations(labels, customOptions.Labels)
		annotations = MergeLabelsOrAnnotations(annotations, customOptions.Annotations)
	}

	backupSection := GenerateBackupRepositoriesForSolrXml(solrCloud.Spec.BackupRepositories)
	configMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:        solrCloud.ConfigMapName(),
			Namespace:   solrCloud.GetNamespace(),
			Labels:      labels,
			Annotations: annotations,
		},
		Data: map[string]string{
			"solr.xml": GenerateSolrXMLString(backupSection),
		},
	}

	return configMap
}

func GenerateSolrXMLString(backupSection string) string {
	return fmt.Sprintf(DefaultSolrXML, backupSection)
}

// GenerateCommonService returns a new corev1.Service pointer generated for the entire SolrCloud instance
// solrCloud: SolrCloud instance
func GenerateCommonService(solrCloud *solr.SolrCloud) *corev1.Service {
	labels := solrCloud.SharedLabelsWith(solrCloud.GetLabels())
	labels["service-type"] = "common"

	selectorLabels := solrCloud.SharedLabels()
	selectorLabels["technology"] = solr.SolrTechnologyLabel

	var annotations map[string]string

	// Add externalDNS annotation if necessary
	extOpts := solrCloud.Spec.SolrAddressability.External
	if extOpts != nil && extOpts.Method == solr.ExternalDNS && !extOpts.HideCommon {
		annotations = make(map[string]string, 1)
		urls := []string{solrCloud.ExternalDnsDomain(extOpts.DomainName)}
		for _, domain := range extOpts.AdditionalDomainNames {
			urls = append(urls, solrCloud.ExternalDnsDomain(domain))
		}
		annotations["external-dns.alpha.kubernetes.io/hostname"] = strings.Join(urls, ",")
	}

	customOptions := solrCloud.Spec.CustomSolrKubeOptions.CommonServiceOptions
	if nil != customOptions {
		labels = MergeLabelsOrAnnotations(labels, customOptions.Labels)
		annotations = MergeLabelsOrAnnotations(annotations, customOptions.Annotations)
	}

	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:        solrCloud.CommonServiceName(),
			Namespace:   solrCloud.GetNamespace(),
			Labels:      labels,
			Annotations: annotations,
		},
		Spec: corev1.ServiceSpec{
			Ports: []corev1.ServicePort{
				{Name: SolrClientPortName, Port: int32(solrCloud.Spec.SolrAddressability.CommonServicePort), Protocol: corev1.ProtocolTCP, TargetPort: intstr.FromString(SolrClientPortName)},
			},
			Selector: selectorLabels,
		},
	}
	return service
}

// GenerateHeadlessService returns a new Headless corev1.Service pointer generated for the SolrCloud instance
// The PublishNotReadyAddresses option is set as true, because we want each pod to be reachable no matter the readiness of the pod.
// solrCloud: SolrCloud instance
func GenerateHeadlessService(solrCloud *solr.SolrCloud) *corev1.Service {
	labels := solrCloud.SharedLabelsWith(solrCloud.GetLabels())
	labels["service-type"] = "headless"

	selectorLabels := solrCloud.SharedLabels()
	selectorLabels["technology"] = solr.SolrTechnologyLabel

	var annotations map[string]string

	// Add externalDNS annotation if necessary
	extOpts := solrCloud.Spec.SolrAddressability.External
	if extOpts != nil && extOpts.Method == solr.ExternalDNS && !extOpts.HideNodes {
		annotations = make(map[string]string, 1)
		urls := []string{solrCloud.ExternalDnsDomain(extOpts.DomainName)}
		for _, domain := range extOpts.AdditionalDomainNames {
			urls = append(urls, solrCloud.ExternalDnsDomain(domain))
		}
		annotations["external-dns.alpha.kubernetes.io/hostname"] = strings.Join(urls, ",")
	}

	customOptions := solrCloud.Spec.CustomSolrKubeOptions.HeadlessServiceOptions
	if nil != customOptions {
		labels = MergeLabelsOrAnnotations(labels, customOptions.Labels)
		annotations = MergeLabelsOrAnnotations(annotations, customOptions.Annotations)
	}

	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:        solrCloud.HeadlessServiceName(),
			Namespace:   solrCloud.GetNamespace(),
			Labels:      labels,
			Annotations: annotations,
		},
		Spec: corev1.ServiceSpec{
			Ports: []corev1.ServicePort{
				{Name: SolrClientPortName, Port: int32(solrCloud.NodePort()), Protocol: corev1.ProtocolTCP, TargetPort: intstr.FromString(SolrClientPortName)},
			},
			Selector:                 selectorLabels,
			ClusterIP:                corev1.ClusterIPNone,
			PublishNotReadyAddresses: true,
		},
	}
	return service
}

// GenerateNodeService returns a new External corev1.Service pointer generated for the given Solr Node.
// The PublishNotReadyAddresses option is set as true, because we want each pod to be reachable no matter the readiness of the pod.
// solrCloud: SolrCloud instance
// nodeName: string node
func GenerateNodeService(solrCloud *solr.SolrCloud, nodeName string) *corev1.Service {
	labels := solrCloud.SharedLabelsWith(solrCloud.GetLabels())
	labels["service-type"] = "external"

	selectorLabels := solrCloud.SharedLabels()
	selectorLabels["technology"] = solr.SolrTechnologyLabel
	selectorLabels["statefulset.kubernetes.io/pod-name"] = nodeName

	var annotations map[string]string

	customOptions := solrCloud.Spec.CustomSolrKubeOptions.NodeServiceOptions
	if nil != customOptions {
		labels = MergeLabelsOrAnnotations(labels, customOptions.Labels)
		annotations = MergeLabelsOrAnnotations(annotations, customOptions.Annotations)
	}

	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:        nodeName,
			Namespace:   solrCloud.GetNamespace(),
			Labels:      labels,
			Annotations: annotations,
		},
		Spec: corev1.ServiceSpec{
			Selector: selectorLabels,
			Ports: []corev1.ServicePort{
				{Name: SolrClientPortName, Port: int32(solrCloud.NodePort()), Protocol: corev1.ProtocolTCP, TargetPort: intstr.FromString(SolrClientPortName)},
			},
			PublishNotReadyAddresses: true,
		},
	}
	return service
}

// GenerateIngress returns a new Ingress pointer generated for the entire SolrCloud, pointing to all instances
// solrCloud: SolrCloud instance
// nodeStatuses: []SolrNodeStatus the nodeStatuses
func GenerateIngress(solrCloud *solr.SolrCloud, nodeNames []string) (ingress *netv1.Ingress) {
	labels := solrCloud.SharedLabelsWith(solrCloud.GetLabels())
	var annotations map[string]string

	customOptions := solrCloud.Spec.CustomSolrKubeOptions.IngressOptions
	if nil != customOptions {
		labels = MergeLabelsOrAnnotations(labels, customOptions.Labels)
		annotations = MergeLabelsOrAnnotations(annotations, customOptions.Annotations)
	}

	extOpts := solrCloud.Spec.SolrAddressability.External

	// Create advertised domain name and possible additional domain names'
	allDomains := append([]string{extOpts.DomainName}, extOpts.AdditionalDomainNames...)
	rules, allHosts := CreateSolrIngressRules(solrCloud, nodeNames, allDomains)

	var ingressTLS []netv1.IngressTLS
	if solrCloud.Spec.SolrTLS != nil && solrCloud.Spec.SolrTLS.PKCS12Secret != nil {
		ingressTLS = append(ingressTLS, netv1.IngressTLS{SecretName: solrCloud.Spec.SolrTLS.PKCS12Secret.Name})
	} // else if using mountedTLSDir, it's likely they'll have an auto-wired TLS solution for Ingress as well via annotations

	if extOpts.IngressTLSTerminationSecret != "" {
		ingressTLS = append(ingressTLS, netv1.IngressTLS{
			SecretName: extOpts.IngressTLSTerminationSecret,
			Hosts:      allHosts,
		})
	}
	solrNodesRequireTLS := solrCloud.Spec.SolrTLS != nil
	ingressFrontedByTLS := len(ingressTLS) > 0

	// TLS Passthrough annotations
	if solrNodesRequireTLS {
		if annotations == nil {
			annotations = make(map[string]string, 1)
		}
		_, ok := annotations["nginx.ingress.kubernetes.io/backend-protocol"]
		if !ok {
			annotations["nginx.ingress.kubernetes.io/backend-protocol"] = "HTTPS"
		}
	} else {
		if annotations == nil {
			annotations = make(map[string]string, 1)
		}
		_, ok := annotations["nginx.ingress.kubernetes.io/backend-protocol"]
		if !ok {
			annotations["nginx.ingress.kubernetes.io/backend-protocol"] = "HTTP"
		}
	}

	// TLS Accept annotations
	if ingressFrontedByTLS {
		_, ok := annotations["nginx.ingress.kubernetes.io/ssl-redirect"]
		if !ok {
			annotations["nginx.ingress.kubernetes.io/ssl-redirect"] = "true"
		}
	}

	ingress = &netv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Name:        solrCloud.CommonIngressName(),
			Namespace:   solrCloud.GetNamespace(),
			Labels:      labels,
			Annotations: annotations,
		},
		Spec: netv1.IngressSpec{
			Rules: rules,
			TLS:   ingressTLS,
		},
	}
	return ingress
}

// CreateSolrIngressRules returns all applicable ingress rules for a cloud.
// solrCloud: SolrCloud instance
// nodeNames: the names for each of the solr pods
// domainName: string Domain for the ingress rule to use
func CreateSolrIngressRules(solrCloud *solr.SolrCloud, nodeNames []string, domainNames []string) (ingressRules []netv1.IngressRule, allHosts []string) {
	if !solrCloud.Spec.SolrAddressability.External.HideCommon {
		for _, domainName := range domainNames {
			rule := CreateCommonIngressRule(solrCloud, domainName)
			ingressRules = append(ingressRules, rule)
			allHosts = append(allHosts, rule.Host)
		}
	}
	if !solrCloud.Spec.SolrAddressability.External.HideNodes {
		for _, nodeName := range nodeNames {
			for _, domainName := range domainNames {
				rule := CreateNodeIngressRule(solrCloud, nodeName, domainName)
				ingressRules = append(ingressRules, rule)
				allHosts = append(allHosts, rule.Host)
			}
		}
	}
	return
}

// CreateCommonIngressRule returns a new Ingress Rule generated for a SolrCloud under the given domainName
// solrCloud: SolrCloud instance
// domainName: string Domain for the ingress rule to use
func CreateCommonIngressRule(solrCloud *solr.SolrCloud, domainName string) (ingressRule netv1.IngressRule) {
	pathType := netv1.PathTypeImplementationSpecific
	ingressRule = netv1.IngressRule{
		Host: solrCloud.ExternalCommonUrl(domainName, false),
		IngressRuleValue: netv1.IngressRuleValue{
			HTTP: &netv1.HTTPIngressRuleValue{
				Paths: []netv1.HTTPIngressPath{
					{
						Backend: netv1.IngressBackend{
							Service: &netv1.IngressServiceBackend{
								Name: solrCloud.CommonServiceName(),
								Port: netv1.ServiceBackendPort{
									Number: int32(solrCloud.Spec.SolrAddressability.CommonServicePort),
								},
							},
						},
						PathType: &pathType,
					},
				},
			},
		},
	}
	return ingressRule
}

// CreateNodeIngressRule returns a new Ingress Rule generated for a specific Solr Node under the given domainName
// solrCloud: SolrCloud instance
// nodeName: string Name of the node
// domainName: string Domain for the ingress rule to use
func CreateNodeIngressRule(solrCloud *solr.SolrCloud, nodeName string, domainName string) (ingressRule netv1.IngressRule) {
	pathType := netv1.PathTypeImplementationSpecific
	ingressRule = netv1.IngressRule{
		Host: solrCloud.ExternalNodeUrl(nodeName, domainName, false),
		IngressRuleValue: netv1.IngressRuleValue{
			HTTP: &netv1.HTTPIngressRuleValue{
				Paths: []netv1.HTTPIngressPath{
					{
						Backend: netv1.IngressBackend{
							Service: &netv1.IngressServiceBackend{
								Name: nodeName,
								Port: netv1.ServiceBackendPort{
									Number: int32(solrCloud.NodePort()),
								},
							},
						},
						PathType: &pathType,
					},
				},
			},
		},
	}
	return ingressRule
}

// TODO: Have this replace the postStart hook for creating the chroot
func generateZKInteractionInitContainer(solrCloud *solr.SolrCloud, solrCloudStatus *solr.SolrCloudStatus, reconcileConfigInfo map[string]string) (bool, corev1.Container) {
	allSolrOpts := make([]string, 0)

	// Add all necessary ZK Info
	envVars, zkSolrOpt, _ := createZkConnectionEnvVars(solrCloud, solrCloudStatus)
	if zkSolrOpt != "" {
		allSolrOpts = append(allSolrOpts, zkSolrOpt)
	}

	if solrCloud.Spec.SolrOpts != "" {
		allSolrOpts = append(allSolrOpts, solrCloud.Spec.SolrOpts)
	}

	// Add SOLR_OPTS last, so that it can use values from all of the other ENV_VARS
	if len(allSolrOpts) > 0 {
		envVars = append(envVars, corev1.EnvVar{
			Name:  "SOLR_OPTS",
			Value: strings.Join(allSolrOpts, " "),
		})
	}

	cmd := ""

	if solrCloud.Spec.SolrTLS != nil {
		cmd = setUrlSchemeClusterPropCmd()
	}

	if reconcileConfigInfo[SecurityJsonFile] != "" {
		envVars = append(envVars, corev1.EnvVar{Name: "SECURITY_JSON", ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: solrCloud.SecurityBootstrapSecretName()},
				Key:                  SecurityJsonFile}}})

		if cmd == "" {
			cmd += "solr zk ls ${ZK_CHROOT} -z ${ZK_SERVER} || solr zk mkroot ${ZK_CHROOT} -z ${ZK_SERVER}; "
		}
		cmd += "ZK_SECURITY_JSON=$(/opt/solr/server/scripts/cloud-scripts/zkcli.sh -zkhost ${ZK_HOST} -cmd get /security.json); "
		cmd += "if [ ${#ZK_SECURITY_JSON} -lt 3 ]; then echo $SECURITY_JSON > /tmp/security.json; /opt/solr/server/scripts/cloud-scripts/zkcli.sh -zkhost ${ZK_HOST} -cmd putfile /security.json /tmp/security.json; echo \"put security.json in ZK\"; fi"
	}

	if cmd != "" {
		return true, corev1.Container{
			Name:                     "setup-zk",
			Image:                    solrCloud.Spec.SolrImage.ToImageName(),
			ImagePullPolicy:          solrCloud.Spec.SolrImage.PullPolicy,
			TerminationMessagePath:   "/dev/termination-log",
			TerminationMessagePolicy: "File",
			Command:                  []string{"sh", "-c", cmd},
			Env:                      envVars,
		}
	}

	return false, corev1.Container{}
}

func createZkConnectionEnvVars(solrCloud *solr.SolrCloud, solrCloudStatus *solr.SolrCloudStatus) (envVars []corev1.EnvVar, solrOpt string, hasChroot bool) {
	zkConnectionStr, zkServer, zkChroot := solrCloudStatus.DissectZkInfo()
	envVars = []corev1.EnvVar{
		{
			Name:  "ZK_HOST",
			Value: zkConnectionStr,
		},
		{
			Name:  "ZK_CHROOT",
			Value: zkChroot,
		},
		{
			Name:  "ZK_SERVER",
			Value: zkServer,
		},
	}

	// Add ACL information, if given, through Env Vars
	allACL, readOnlyACL := solrCloud.Spec.ZookeeperRef.GetACLs()
	if hasACLs, aclEnvs := AddACLsToEnv(allACL, readOnlyACL); hasACLs {
		envVars = append(envVars, aclEnvs...)

		// The $SOLR_ZK_CREDS_AND_ACLS parameter does not get picked up when running solr, it must be added to the SOLR_OPTS.
		solrOpt = "$(SOLR_ZK_CREDS_AND_ACLS)"
	}

	return envVars, solrOpt, len(zkChroot) > 1
}

func setupVolumeMountForUserProvidedConfigMapEntry(reconcileConfigInfo map[string]string, fileKey string, solrVolumes []corev1.Volume, envVar string) (*corev1.VolumeMount, *corev1.EnvVar, *corev1.Volume) {
	volName := strings.ReplaceAll(fileKey, ".", "-")
	mountPath := fmt.Sprintf("/var/solr/%s", reconcileConfigInfo[fileKey])
	appendedToExisting := false
	if reconcileConfigInfo[fileKey] == reconcileConfigInfo[SolrXmlFile] {
		// the user provided a custom log4j2.xml and solr.xml, append to the volume for solr.xml created above
		for _, vol := range solrVolumes {
			if vol.Name == "solr-xml" {
				vol.ConfigMap.Items = append(vol.ConfigMap.Items, corev1.KeyToPath{Key: fileKey, Path: fileKey})
				appendedToExisting = true
				volName = vol.Name
				break
			}
		}
	}

	var vol *corev1.Volume = nil
	if !appendedToExisting {
		vol = &corev1.Volume{
			Name: volName,
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: reconcileConfigInfo[fileKey]},
					Items:                []corev1.KeyToPath{{Key: fileKey, Path: fileKey}},
					DefaultMode:          &PublicReadOnlyPermissions,
				},
			},
		}
	}
	pathToFile := fmt.Sprintf("%s/%s", mountPath, fileKey)

	return &corev1.VolumeMount{Name: volName, MountPath: mountPath}, &corev1.EnvVar{Name: envVar, Value: pathToFile}, vol
}

func BasicAuthHeader(basicAuthSecret *corev1.Secret) string {
	creds := fmt.Sprintf("%s:%s", basicAuthSecret.Data[corev1.BasicAuthUsernameKey], basicAuthSecret.Data[corev1.BasicAuthPasswordKey])
	return "Basic " + b64.StdEncoding.EncodeToString([]byte(creds))
}

func ValidateBasicAuthSecret(basicAuthSecret *corev1.Secret) error {
	if basicAuthSecret.Type != corev1.SecretTypeBasicAuth {
		return fmt.Errorf("invalid secret type %v; user-provided secret %s must be of type: %v",
			basicAuthSecret.Type, basicAuthSecret.Name, corev1.SecretTypeBasicAuth)
	}

	if _, ok := basicAuthSecret.Data[corev1.BasicAuthUsernameKey]; !ok {
		return fmt.Errorf("%s key not found in user-provided basic-auth secret %s",
			corev1.BasicAuthUsernameKey, basicAuthSecret.Name)
	}

	if _, ok := basicAuthSecret.Data[corev1.BasicAuthPasswordKey]; !ok {
		return fmt.Errorf("%s key not found in user-provided basic-auth secret %s",
			corev1.BasicAuthPasswordKey, basicAuthSecret.Name)
	}

	return nil
}

func GenerateBasicAuthSecretWithBootstrap(solrCloud *solr.SolrCloud) (*corev1.Secret, *corev1.Secret) {

	securityBootstrapInfo := generateSecurityJson(solrCloud)

	labels := solrCloud.SharedLabelsWith(solrCloud.GetLabels())
	var annotations map[string]string
	basicAuthSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:        solrCloud.BasicAuthSecretName(),
			Namespace:   solrCloud.GetNamespace(),
			Labels:      labels,
			Annotations: annotations,
		},
		Data: map[string][]byte{
			corev1.BasicAuthUsernameKey: []byte(solr.DefaultBasicAuthUsername),
			corev1.BasicAuthPasswordKey: securityBootstrapInfo[solr.DefaultBasicAuthUsername],
		},
		Type: corev1.SecretTypeBasicAuth,
	}

	// this secret holds the admin and solr user credentials and the security.json needed to bootstrap Solr security
	// once the security.json is created using the setup-zk initContainer, it is not updated by the operator
	boostrapSecuritySecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:        solrCloud.SecurityBootstrapSecretName(),
			Namespace:   solrCloud.GetNamespace(),
			Labels:      labels,
			Annotations: annotations,
		},
		Data: map[string][]byte{
			"admin":          securityBootstrapInfo["admin"],
			"solr":           securityBootstrapInfo["solr"],
			SecurityJsonFile: securityBootstrapInfo[SecurityJsonFile],
		},
		Type: corev1.SecretTypeOpaque,
	}

	return basicAuthSecret, boostrapSecuritySecret
}

func generateSecurityJson(solrCloud *solr.SolrCloud) map[string][]byte {
	blockUnknown := true

	probeRole := "\"k8s\"" // probe endpoints are secures
	if !solrCloud.Spec.SolrSecurity.ProbesRequireAuth {
		blockUnknown = false
		probeRole = "null" // a JSON null value here to allow open access
	}

	probePaths := getProbePaths(solrCloud)
	probeAuthz := ""
	for i, p := range probePaths {
		if i > 0 {
			probeAuthz += ", "
		}
		if strings.HasPrefix(p, "/solr") {
			p = p[len("/solr"):]
		}
		probeAuthz += fmt.Sprintf("{ \"name\": \"k8s-probe-%d\", \"role\":%s, \"collection\": null, \"path\":\"%s\" }", i, probeRole, p)
	}

	// Create the user accounts for security.json with random passwords
	// hashed with random salt, just as Solr's hashing works
	username := solr.DefaultBasicAuthUsername
	users := []string{"admin", username, "solr"}
	secretData := make(map[string][]byte, len(users))
	credentials := make(map[string]string, len(users))
	for _, u := range users {
		secretData[u] = randomPassword()
		credentials[u] = solrPasswordHash(secretData[u])
	}
	credentialsJson, _ := json.Marshal(credentials)

	securityJson := fmt.Sprintf(`{
      "authentication":{
        "blockUnknown": %t,
        "class":"solr.BasicAuthPlugin",
        "credentials": %s,
        "realm":"Solr Basic Auth",
        "forwardCredentials": false
      },
      "authorization": {
        "class": "solr.RuleBasedAuthorizationPlugin",
        "user-role": {
          "admin": ["admin", "k8s"],
          "%s": ["k8s"],
          "solr": ["users", "k8s"]
        },
        "permissions": [
          %s,
          { "name": "k8s-status", "role":"k8s", "collection": null, "path":"/admin/collections" },
          { "name": "k8s-metrics", "role":"k8s", "collection": null, "path":"/admin/metrics" },
          { "name": "k8s-zk", "role":"k8s", "collection": null, "path":"/admin/zookeeper/status" },
          { "name": "k8s-ping", "role":"k8s", "collection": "*", "path":"/admin/ping" },
          { "name": "read", "role":["admin","users"] },
          { "name": "update", "role":["admin"] },
          { "name": "security-read", "role": ["admin"] },
          { "name": "security-edit", "role": ["admin"] },
          { "name": "all", "role":["admin"] }
        ]
      }
    }`, blockUnknown, credentialsJson, username, probeAuthz)

	// we need to store the security.json in the secret, otherwise we'd recompute it for every reconcile loop
	// but that doesn't work for randomized passwords ...
	secretData[SecurityJsonFile] = []byte(securityJson)

	return secretData
}

func GetCustomProbePaths(solrCloud *solr.SolrCloud) []string {
	probePaths := []string{}

	podOptions := solrCloud.Spec.CustomSolrKubeOptions.PodOptions
	if podOptions == nil {
		return probePaths
	}

	// include any custom paths
	if podOptions.ReadinessProbe != nil && podOptions.ReadinessProbe.HTTPGet != nil {
		probePaths = append(probePaths, podOptions.ReadinessProbe.HTTPGet.Path)
	}

	if podOptions.LivenessProbe != nil && podOptions.LivenessProbe.HTTPGet != nil {
		probePaths = append(probePaths, podOptions.LivenessProbe.HTTPGet.Path)
	}

	if podOptions.StartupProbe != nil && podOptions.StartupProbe.HTTPGet != nil {
		probePaths = append(probePaths, podOptions.StartupProbe.HTTPGet.Path)
	}

	return probePaths
}

// Gets a list of probe paths we need to setup authz for
func getProbePaths(solrCloud *solr.SolrCloud) []string {
	probePaths := []string{DefaultProbePath}
	probePaths = append(probePaths, GetCustomProbePaths(solrCloud)...)
	return uniqueProbePaths(probePaths)
}

func randomPassword() []byte {
	rand.Seed(time.Now().UnixNano())
	lower := "abcdefghijklmnpqrstuvwxyz" // no 'o'
	upper := strings.ToUpper(lower)
	digits := "0123456789"
	chars := lower + upper + digits + "()[]%#@-()[]%#@-"
	pass := make([]byte, 16)
	// start with a lower char and end with an upper
	pass[0] = lower[rand.Intn(len(lower))]
	pass[len(pass)-1] = upper[rand.Intn(len(upper))]
	perm := rand.Perm(len(chars))
	for i := 1; i < len(pass)-1; i++ {
		pass[i] = chars[perm[i]]
	}
	return pass
}

func randomSaltHash() []byte {
	b := make([]byte, 32)
	rand.Read(b)
	salt := sha256.Sum256(b)
	return salt[:]
}

// this mimics the password hash generation approach used by Solr
func solrPasswordHash(passBytes []byte) string {
	// combine password with salt to create the hash
	salt := randomSaltHash()
	passHashBytes := sha256.Sum256(append(salt[:], passBytes...))
	passHashBytes = sha256.Sum256(passHashBytes[:])
	passHash := b64.StdEncoding.EncodeToString(passHashBytes[:])
	return fmt.Sprintf("%s %s", passHash, b64.StdEncoding.EncodeToString(salt))
}

func uniqueProbePaths(paths []string) []string {
	keys := make(map[string]bool)
	var set []string
	for _, name := range paths {
		if _, exists := keys[name]; !exists {
			keys[name] = true
			set = append(set, name)
		}
	}
	return set
}

// When running with TLS and clientAuth=Need or if the probe endpoints require auth, we need to use a command instead of HTTP Get
// This function builds the custom probe command and returns any associated volume / mounts needed for the auth secrets
func configureSecureProbeCommand(solrCloud *solr.SolrCloud, defaultProbeGetAction *corev1.HTTPGetAction) (string, *corev1.Volume, *corev1.VolumeMount) {
	// mount the secret in a file so it gets updated; env vars do not see:
	// https://kubernetes.io/docs/concepts/configuration/secret/#environment-variables-are-not-updated-after-a-secret-update
	basicAuthOption := ""
	enableBasicAuth := ""
	var volMount *corev1.VolumeMount
	var vol *corev1.Volume
	if solrCloud.Spec.SolrSecurity != nil && solrCloud.Spec.SolrSecurity.ProbesRequireAuth {
		secretName := solrCloud.BasicAuthSecretName()
		vol = &corev1.Volume{
			Name: strings.ReplaceAll(secretName, ".", "-"),
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName:  secretName,
					DefaultMode: &SecretReadOnlyPermissions,
				},
			},
		}
		mountPath := fmt.Sprintf("/etc/secrets/%s", vol.Name)
		volMount = &corev1.VolumeMount{Name: vol.Name, MountPath: mountPath}
		usernameFile := fmt.Sprintf("%s/%s", mountPath, corev1.BasicAuthUsernameKey)
		passwordFile := fmt.Sprintf("%s/%s", mountPath, corev1.BasicAuthPasswordKey)
		basicAuthOption = fmt.Sprintf("-Dbasicauth=$(cat %s):$(cat %s)", usernameFile, passwordFile)
		enableBasicAuth = " -Dsolr.httpclient.builder.factory=org.apache.solr.client.solrj.impl.PreemptiveBasicAuthClientBuilderFactory "
	}

	// Is TLS enabled? If so we need some additional SSL related props
	tlsJavaToolOpts, tlsJavaSysProps := secureProbeTLSJavaToolOpts(solrCloud)
	javaToolOptions := strings.TrimSpace(basicAuthOption + " " + tlsJavaToolOpts)

	// construct the probe command to invoke the SolrCLI "api" action
	//
	// and yes, this is ugly, but bin/solr doesn't expose the "api" action (as of 8.8.0) so we have to invoke java directly
	// taking some liberties on the /opt/solr path based on the official Docker image as there is no ENV var set for that path
	probeCommand := fmt.Sprintf("JAVA_TOOL_OPTIONS=\"%s\" java %s %s "+
		"-Dsolr.install.dir=\"/opt/solr\" -Dlog4j.configurationFile=\"/opt/solr/server/resources/log4j2-console.xml\" "+
		"-classpath \"/opt/solr/server/solr-webapp/webapp/WEB-INF/lib/*:/opt/solr/server/lib/ext/*:/opt/solr/server/lib/*\" "+
		"org.apache.solr.util.SolrCLI api -get %s://localhost:%d%s",
		javaToolOptions, tlsJavaSysProps, enableBasicAuth, solrCloud.UrlScheme(false), defaultProbeGetAction.Port.IntVal, defaultProbeGetAction.Path)
	probeCommand = regexp.MustCompile(`\s+`).ReplaceAllString(strings.TrimSpace(probeCommand), " ")

	return probeCommand, vol, volMount
}
