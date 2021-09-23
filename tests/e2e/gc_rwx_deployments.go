/*
Copyright 2021 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package e2e

import (
	"context"
	"fmt"

	"github.com/onsi/ginkgo"
	"github.com/onsi/gomega"
	cnstypes "github.com/vmware/govmomi/cns/types"
	v1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/test/e2e/framework"
	fdep "k8s.io/kubernetes/test/e2e/framework/deployment"
	fnodes "k8s.io/kubernetes/test/e2e/framework/node"
	fpod "k8s.io/kubernetes/test/e2e/framework/pod"
	fpv "k8s.io/kubernetes/test/e2e/framework/pv"
)

var _ = ginkgo.Describe("[rwm-csi-tkg] File Volume Provision with Deployments", func() {
	f := framework.NewDefaultFramework("rwx-tkg-basic")
	var (
		client            clientset.Interface
		namespace         string
		scParameters      map[string]string
		storagePolicyName string
	)
	ginkgo.BeforeEach(func() {
		client = f.ClientSet
		namespace = getNamespaceToRunTests(f)
		scParameters = make(map[string]string)
		storagePolicyName = GetAndExpectStringEnvVar(envStoragePolicyNameForSharedDatastores)
		bootstrap()
		nodeList, err := fnodes.GetReadySchedulableNodes(f.ClientSet)
		framework.ExpectNoError(err, "Unable to find ready and schedulable Node")
		if !(len(nodeList.Items) > 0) {
			framework.Failf("Unable to find ready and schedulable Node")
		}
	})

	/*
		Test to verify file volume provision with DeploymentSets

		1. Create a SC
		2. Create two PVCs, PVC1 and PVC2 with "ReadWriteMany" access mode using the SC
		3. Wait for PVCs to be Bound in GC
		4. Verify if the mapping PVCs are also bound in the SV cluster using the volume handler
		5. Verify CnsVolumeMetadata CRD are created
		6. Verify health status of PVCs
		7. Verify volumes are created on CNS by using CNSQuery API and also check metadata is pushed to CNS
		8. Create Deployment type application using the PVCs created above
		9. Create Deployment type with replica count as 3 using the Storage Policy obtained in Step 1
		10. Wait until all Pods are ready
		11. Verify CnsFileAccessConfig CRD is created
		12. Scale down the replica count 2
		13. Scale-up replica count 5
		14. Scale down to 0 replicas and delete all pods
		15. Delete the Deployment app
		16. Verify CnsFileAccessConfig CRD are deleted
		17. Verify if all the pods are successfully deleted
		18. Verify using CNS Query API if all 2 PV's still exists
		19. Delete PVCs
		20. Verify if PVCs and PVs are deleted in the SV cluster and GC
		21. Verify CnsVolumeMetadata CRD are deleted
		22. Check if the VolumeID is deleted from CNS by using CNSQuery API
		23. Cleanup SC
	*/
	ginkgo.It("Verify RWX volumes with Deployment", func() {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		var storageclasspvc *storagev1.StorageClass
		var pvclaim *v1.PersistentVolumeClaim
		var err error
		var missingPod *v1.Pod

		ginkgo.By("CNS_TEST: Running for GC setup")
		scParameters[svStorageClassName] = storagePolicyName
		ginkgo.By("Creating a PVC")
		storageclasspvc, pvclaim, err = createPVCAndStorageClass(client,
			namespace, nil, scParameters, diskSize, nil, "", false, v1.ReadWriteMany)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		defer func() {
			err = client.StorageV1().StorageClasses().Delete(ctx, storageclasspvc.Name, *metav1.NewDeleteOptions(0))
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		}()

		ginkgo.By("Creating the PVC2 in guest cluster")
		pvc2 := getPersistentVolumeClaimSpecForRWX(namespace, nil, "", diskSize)
		pvc2.Spec.AccessModes[0] = v1.ReadWriteMany
		pvc2.Spec.StorageClassName = &storageclasspvc.Name

		pvc2, err = client.CoreV1().PersistentVolumeClaims(namespace).Create(ctx, pvc2, metav1.CreateOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		ginkgo.By("Expect claim to provision volume successfully")
		persistentvolumes, err := fpv.WaitForPVClaimBoundPhase(client,
			[]*v1.PersistentVolumeClaim{pvclaim, pvc2}, framework.ClaimProvisionTimeout)
		gomega.Expect(err).NotTo(gomega.HaveOccurred(), "Failed to provision volume")

		volHandle := persistentvolumes[0].Spec.CSI.VolumeHandle
		gomega.Expect(volHandle).NotTo(gomega.BeEmpty())

		volHandle2 := persistentvolumes[1].Spec.CSI.VolumeHandle
		gomega.Expect(volHandle2).NotTo(gomega.BeEmpty())

		volumeID := getVolumeIDFromSupervisorCluster(volHandle)
		gomega.Expect(volumeID).NotTo(gomega.BeEmpty())

		volumeID2 := getVolumeIDFromSupervisorCluster(volHandle2)
		gomega.Expect(volumeID2).NotTo(gomega.BeEmpty())

		defer func() {
			err = fpv.DeletePersistentVolumeClaim(client, pvclaim.Name, pvclaim.Namespace)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = e2eVSphere.waitForCNSVolumeToBeDeleted(volHandle)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			err = fpv.DeletePersistentVolumeClaim(client, pvc2.Name, pvc2.Namespace)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = e2eVSphere.waitForCNSVolumeToBeDeleted(volHandle2)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		}()

		// Verify using CNS Query API if VolumeID retrieved from PV is present.
		ginkgo.By(fmt.Sprintf("Invoking QueryCNSVolumeWithResult with VolumeID: %s", volumeID))
		queryResult, err := e2eVSphere.queryCNSVolumeWithResult(volumeID)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(queryResult.Volumes).ShouldNot(gomega.BeEmpty())
		ginkgo.By(fmt.Sprintf("volume Name:%s, capacity:%d volumeType:%s health:%s accesspoint: %s",
			queryResult.Volumes[0].Name,
			queryResult.Volumes[0].BackingObjectDetails.(*cnstypes.CnsVsanFileShareBackingDetails).CapacityInMb,
			queryResult.Volumes[0].VolumeType, queryResult.Volumes[0].HealthStatus,
			queryResult.Volumes[0].BackingObjectDetails.(*cnstypes.CnsVsanFileShareBackingDetails).AccessPoints),
		)

		// Verify using CNS Query API if VolumeID retrieved from PV is present.
		ginkgo.By(fmt.Sprintf("Invoking QueryCNSVolumeWithResult with VolumeID: %s", volumeID2))
		queryResult2, err := e2eVSphere.queryCNSVolumeWithResult(volumeID2)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(queryResult2.Volumes).ShouldNot(gomega.BeEmpty())
		ginkgo.By(fmt.Sprintf("volume Name:%s, capacity:%d volumeType:%s health:%s accesspoint: %s",
			queryResult2.Volumes[0].Name,
			queryResult2.Volumes[0].BackingObjectDetails.(*cnstypes.CnsVsanFileShareBackingDetails).CapacityInMb,
			queryResult2.Volumes[0].VolumeType, queryResult.Volumes[0].HealthStatus,
			queryResult2.Volumes[0].BackingObjectDetails.(*cnstypes.CnsVsanFileShareBackingDetails).AccessPoints),
		)

		labelsMap := make(map[string]string)
		labelsMap["app"] = "test"
		ginkgo.By("Creating a Deployment using pvc1 & pvc2")

		dep, err := createDeployment(ctx, client, 3, labelsMap, nil, namespace,
			[]*v1.PersistentVolumeClaim{pvclaim, pvc2}, "", false, busyBoxImageOnGcr)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		pods, err := fdep.GetPodsForDeployment(client, dep)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		var cnsFileAccessConfigCRDList []string
		for _, ddpod := range pods.Items {
			framework.Logf("Parsing the Pod %s", ddpod.Name)
			_, err := client.CoreV1().Pods(namespace).Get(ctx, ddpod.Name, metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			err = fpod.WaitForPodNameRunningInNamespace(client, ddpod.Name, namespace)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			ginkgo.By("Verifying whether the CnsFileAccessConfig CRD is created or not for Pod with pvc1")
			verifyCNSFileAccessConfigCRDInSupervisor(ctx, f, ddpod.Spec.NodeName+"-"+volHandle,
				crdCNSFileAccessConfig, crdVersion, crdGroup, true)

			cnsFileAccessConfigCRDList = append(cnsFileAccessConfigCRDList, ddpod.Spec.NodeName+"-"+volHandle)

			ginkgo.By("Verifying whether the CnsFileAccessConfig CRD is created or not for Pod with pvc2")
			verifyCNSFileAccessConfigCRDInSupervisor(ctx, f, ddpod.Spec.NodeName+"-"+volHandle2,
				crdCNSFileAccessConfig, crdVersion, crdGroup, true)
			cnsFileAccessConfigCRDList = append(cnsFileAccessConfigCRDList, ddpod.Spec.NodeName+"-"+volHandle2)
		}

		ginkgo.By("Scale down deployment to 2 replica")
		dep, err = client.AppsV1().Deployments(namespace).Get(ctx, dep.Name, metav1.GetOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		rep := dep.Spec.Replicas
		*rep = 2
		dep.Spec.Replicas = rep
		dep, err = client.AppsV1().Deployments(namespace).Update(ctx, dep, metav1.UpdateOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		pods2, err := fdep.GetPodsForDeployment(client, dep)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		for _, originalPod := range pods.Items {
			if !(originalPod.Name == pods2.Items[0].Name || originalPod.Name == pods2.Items[1].Name) {
				missingPod = originalPod.DeepCopy()
			} else {
				framework.Logf("Found Pod Name in both the Array %s", originalPod.Name)
			}
		}

		ginkgo.By("Verifying whether Pod is Deleted or not")
		err = fpod.WaitForPodNotFoundInNamespace(client, missingPod.Name, namespace, pollTimeout)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		ginkgo.By("Verifying whether the CnsFileAccessConfig CRD is Deleted or not for Pod with pvc2")
		verifyCNSFileAccessConfigCRDInSupervisor(ctx, f, missingPod.Spec.NodeName+"-"+volHandle, crdCNSFileAccessConfig,
			crdVersion, crdGroup, false)

		ginkgo.By("Verifying whether the CnsFileAccessConfig CRD is Deleted or not for Pod with pvc2")
		verifyCNSFileAccessConfigCRDInSupervisor(ctx, f, missingPod.Spec.NodeName+"-"+volHandle2, crdCNSFileAccessConfig,
			crdVersion, crdGroup, false)

		defer func() {
			framework.Logf("Delete deployment set")
			err := client.AppsV1().Deployments(namespace).Delete(ctx, dep.Name, *metav1.NewDeleteOptions(0))
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		}()
	})
})
