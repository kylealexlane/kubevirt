/*
* This file is part of the KubeVirt project
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
*
* Copyright 2018 Red Hat, Inc.
*
 */

package admitters

import (
	"fmt"

	admissionv1 "k8s.io/api/admission/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfield "k8s.io/apimachinery/pkg/util/validation/field"

	"kubevirt.io/kubevirt/pkg/controller"
	"kubevirt.io/kubevirt/pkg/virt-api/webhooks"
	virtconfig "kubevirt.io/kubevirt/pkg/virt-config"

	v1 "kubevirt.io/api/core/v1"

	webhookutils "kubevirt.io/kubevirt/pkg/util/webhooks"
)

type VMIUpdateAdmitter struct {
	ClusterConfig *virtconfig.ClusterConfig
}

func (admitter *VMIUpdateAdmitter) Admit(ar *admissionv1.AdmissionReview) *admissionv1.AdmissionResponse {

	if resp := webhookutils.ValidateSchema(v1.VirtualMachineInstanceGroupVersionKind, ar.Request.Object.Raw); resp != nil {
		return resp
	}
	// Get new VMI from admission response
	newVMI, oldVMI, err := webhookutils.GetVMIFromAdmissionReview(ar)
	if err != nil {
		return webhookutils.ToAdmissionResponseError(err)
	}

	// Reject VMI update if VMI spec changed
	if !equality.Semantic.DeepEqual(newVMI.Spec, oldVMI.Spec) {
		// Only allow the KubeVirt SA to modify the VMI spec, since that means it went through the sub resource.
		if webhooks.IsKubeVirtServiceAccount(ar.Request.UserInfo.Username) {
			hotplugResponse := admitHotplug(newVMI.Spec.Volumes, oldVMI.Spec.Volumes, newVMI.Spec.Domain.Devices.Disks, oldVMI.Spec.Domain.Devices.Disks, oldVMI.Status.VolumeStatus, newVMI, admitter.ClusterConfig)
			if hotplugResponse != nil {
				return hotplugResponse
			}
		} else {
			return webhookutils.ToAdmissionResponse([]metav1.StatusCause{
				{
					Type:    metav1.CauseTypeFieldValueNotSupported,
					Message: "update of VMI object is restricted",
				},
			})
		}
	}

	if reviewResponse := admitVMILabelsUpdate(newVMI, oldVMI, ar); reviewResponse != nil {
		return reviewResponse
	}

	reviewResponse := admissionv1.AdmissionResponse{}
	reviewResponse.Allowed = true
	return &reviewResponse
}

func getExpectedDisks(newVolumes []v1.Volume) int {
	numMemoryDumpVolumes := 0
	for _, volume := range newVolumes {
		if volume.MemoryDump != nil {
			numMemoryDumpVolumes = numMemoryDumpVolumes + 1
		}
	}
	return len(newVolumes) - numMemoryDumpVolumes
}

// admitHotplug compares the old and new volumes and disks, and ensures that they match and are valid.
func admitHotplug(newVolumes, oldVolumes []v1.Volume, newDisks, oldDisks []v1.Disk, volumeStatuses []v1.VolumeStatus, newVMI *v1.VirtualMachineInstance, config *virtconfig.ClusterConfig) *admissionv1.AdmissionResponse {
	expectedDisks := getExpectedDisks(newVolumes)
	if expectedDisks != len(newDisks) {
		return webhookutils.ToAdmissionResponse([]metav1.StatusCause{
			{
				Type:    metav1.CauseTypeFieldValueInvalid,
				Message: fmt.Sprintf("number of disks (%d) does not equal the number of volumes (%d)", len(newDisks), expectedDisks),
			},
		})
	}
	newHotplugVolumeMap := getHotplugVolumes(newVolumes, volumeStatuses)
	newPermanentVolumeMap := getPermanentVolumes(newVolumes, volumeStatuses)
	oldHotplugVolumeMap := getHotplugVolumes(oldVolumes, volumeStatuses)
	oldPermanentVolumeMap := getPermanentVolumes(oldVolumes, volumeStatuses)

	newDiskMap := getDiskMap(newDisks)
	oldDiskMap := getDiskMap(oldDisks)

	permanentAr := verifyPermanentVolumes(newPermanentVolumeMap, oldPermanentVolumeMap, newDiskMap, oldDiskMap)
	if permanentAr != nil {
		return permanentAr
	}

	hotplugAr := verifyHotplugVolumes(newHotplugVolumeMap, oldHotplugVolumeMap, newDiskMap, oldDiskMap)
	if hotplugAr != nil {
		return hotplugAr
	}

	causes := ValidateVirtualMachineInstanceSpec(k8sfield.NewPath("spec"), &newVMI.Spec, config)
	if len(causes) > 0 {
		return webhookutils.ToAdmissionResponse(causes)
	}

	return nil
}

func verifyHotplugVolumes(newHotplugVolumeMap, oldHotplugVolumeMap map[string]v1.Volume, newDisks, oldDisks map[string]v1.Disk) *admissionv1.AdmissionResponse {
	newEjectedCDRoms := getEjectedCDRoms(newHotplugVolumeMap, newDisks)
	newInsertedCDRoms := getInsertedCDRoms(newHotplugVolumeMap, newDisks)
	oldEjectedCDRoms := getEjectedCDRoms(oldHotplugVolumeMap, oldDisks)
	oldInsertedCDRoms := getInsertedCDRoms(oldHotplugVolumeMap, oldDisks)

	// Since we previously filtered only hotplugged volumes for the maps, and then
	// filtered inserted or ejected CDRoms, then newly inserted / ejected maps
	// are verified.
	newlyEjectedCDRomsMap := getOverlap(newEjectedCDRoms, oldInsertedCDRoms)
	newlyInsertedCDRomsMap := getOverlap(oldEjectedCDRoms, newInsertedCDRoms)

	for k, v := range newHotplugVolumeMap {
		if _, ok := oldHotplugVolumeMap[k]; ok {
			// We already verified the special cases when hotplugged volumes are being
			// ejected or inserted (from previosly inserted or ejected hotplugged volumes)
			_, isNewlyInserted := newlyInsertedCDRomsMap[k]
			_, isNewlyEjected := newlyEjectedCDRomsMap[k]
			if isNewlyEjected || isNewlyInserted {
				continue
			}

			// New and old have same volume, ensure they are the same
			if !equality.Semantic.DeepEqual(v, oldHotplugVolumeMap[k]) {
				return webhookutils.ToAdmissionResponse([]metav1.StatusCause{
					{
						Type:    metav1.CauseTypeFieldValueInvalid,
						Message: fmt.Sprintf("hotplug volume %s, changed", k),
					},
				})
			}
			if v.MemoryDump == nil {
				if _, ok := newDisks[k]; !ok {
					return webhookutils.ToAdmissionResponse([]metav1.StatusCause{
						{
							Type:    metav1.CauseTypeFieldValueInvalid,
							Message: fmt.Sprintf("Volume %s doesn't have a matching disk", k),
						},
					})
				}
				if !equality.Semantic.DeepEqual(newDisks[k], oldDisks[k]) {
					return webhookutils.ToAdmissionResponse([]metav1.StatusCause{
						{
							Type:    metav1.CauseTypeFieldValueInvalid,
							Message: fmt.Sprintf("hotplug disk %s, changed", k),
						},
					})
				}
			}
		} else {
			// This is a new volume, ensure that the volume is either DV, PVC or memoryDumpVolume
			if v.DataVolume == nil && v.PersistentVolumeClaim == nil && v.MemoryDump == nil {
				return webhookutils.ToAdmissionResponse([]metav1.StatusCause{
					{
						Type:    metav1.CauseTypeFieldValueInvalid,
						Message: fmt.Sprintf("volume %s is not a PVC or DataVolume", k),
					},
				})
			}
			if v.MemoryDump == nil {
				// Also ensure the matching new disk exists and is of type scsi
				if _, ok := newDisks[k]; !ok {
					return webhookutils.ToAdmissionResponse([]metav1.StatusCause{
						{
							Type:    metav1.CauseTypeFieldValueInvalid,
							Message: fmt.Sprintf("Disk %s does not exist", k),
						},
					})
				}

				disk := newDisks[k]
				// CDRoms are a special case and cannot be hotplugged on the fly.
				// Libvirt does not allow hotplugging CDRoms, so hotpluggable CDRoms
				// must only be defined during vmi creation.
				if disk.CDRom != nil {
					return webhookutils.ToAdmissionResponse([]metav1.StatusCause{
						{
							Type:    metav1.CauseTypeFieldValueInvalid,
							Message: fmt.Sprintf("new hotplugged Disk %s cannot have type cdrom", k),
						},
					})
				}
				if disk.Disk == nil || disk.Disk.Bus != "scsi" {
					return webhookutils.ToAdmissionResponse([]metav1.StatusCause{
						{
							Type:    metav1.CauseTypeFieldValueInvalid,
							Message: fmt.Sprintf("hotplugged Disk %s does not use a scsi bus", k),
						},
					})

				}
			}
		}
	}
	return nil
}

func verifyPermanentVolumes(newPermanentVolumeMap, oldPermanentVolumeMap map[string]v1.Volume, newDisks, oldDisks map[string]v1.Disk) *admissionv1.AdmissionResponse {
	if len(newPermanentVolumeMap) != len(oldPermanentVolumeMap) {
		// Removed one of the permanent volumes, reject admission.
		return webhookutils.ToAdmissionResponse([]metav1.StatusCause{
			{
				Type:    metav1.CauseTypeFieldValueInvalid,
				Message: "Number of permanent volumes has changed",
			},
		})
	}

	// Ensure we didn't modify any permanent volumes
	for k, v := range newPermanentVolumeMap {
		// Know at this point the new old and permanent have the same count.
		if _, ok := oldPermanentVolumeMap[k]; !ok {
			return webhookutils.ToAdmissionResponse([]metav1.StatusCause{
				{
					Type:    metav1.CauseTypeFieldValueInvalid,
					Message: fmt.Sprintf("permanent volume %s, not found", k),
				},
			})
		}
		if !equality.Semantic.DeepEqual(v, oldPermanentVolumeMap[k]) {
			return webhookutils.ToAdmissionResponse([]metav1.StatusCause{
				{
					Type:    metav1.CauseTypeFieldValueInvalid,
					Message: fmt.Sprintf("permanent volume %s, changed", k),
				},
			})
		}
		if !equality.Semantic.DeepEqual(newDisks[k], oldDisks[k]) {
			return webhookutils.ToAdmissionResponse([]metav1.StatusCause{
				{
					Type:    metav1.CauseTypeFieldValueInvalid,
					Message: fmt.Sprintf("permanent disk %s, changed", k),
				},
			})
		}
	}
	return nil
}

func getDiskMap(disks []v1.Disk) map[string]v1.Disk {
	newDiskMap := make(map[string]v1.Disk, 0)
	for _, disk := range disks {
		if disk.Name != "" {
			newDiskMap[disk.Name] = disk
		}
	}
	return newDiskMap
}

func getHotplugVolumes(volumes []v1.Volume, oldVolumeStatuses []v1.VolumeStatus) map[string]v1.Volume {
	volumeStatusMap := make(map[string]v1.VolumeStatus, 0)
	for _, volumeStatus := range oldVolumeStatuses {
		volumeStatusMap[volumeStatus.Name] = volumeStatus
	}

	hotplugVolumes := make(map[string]v1.Volume, 0)
	for _, volume := range volumes {
		if oldVolumeStatus, ok := volumeStatusMap[volume.Name]; ok {
			// Use old code logic and utilize old volume status to check if it is
			// hotpluggable
			if controller.IsHotpluggableVolume(volume, &oldVolumeStatus) {
				hotplugVolumes[volume.Name] = volume
			}
		} else {
			// Follow old code logic and if any volume is not present in the old
			// status map, then consider it a hotplug volume.
			hotplugVolumes[volume.Name] = volume
		}
	}
	return hotplugVolumes
}

func getPermanentVolumes(volumes []v1.Volume, oldVolumeStatuses []v1.VolumeStatus) map[string]v1.Volume {
	volumeStatusMap := make(map[string]v1.VolumeStatus, 0)
	for _, volumeStatus := range oldVolumeStatuses {
		volumeStatusMap[volumeStatus.Name] = volumeStatus
	}

	permanentVolumes := make(map[string]v1.Volume, 0)
	for _, volume := range volumes {
		// Only consider volumes that existed in the old status map as permanent.
		if oldVolumeStatus, ok := volumeStatusMap[volume.Name]; ok {
			// Use old code logic and utilize old volume status to check if it is
			// hotpluggable
			if !controller.IsHotpluggableVolume(volume, &oldVolumeStatus) {
				permanentVolumes[volume.Name] = volume
			}
		}
	}
	return permanentVolumes
}

func getEjectedCDRoms(volumes map[string]v1.Volume, disks map[string]v1.Disk) map[string]v1.Volume {
	cdromDisks := make(map[string]v1.Disk, 0)
	for _, disk := range disks {
		if disk.CDRom != nil && disk.Name != "" {
			cdromDisks[disk.Name] = v1.Disk{}
		}
	}
	ejectedCDRoms := make(map[string]v1.Volume, 0)
	for _, volume := range volumes {
		if volume.EjectedCDRom != nil {
			if _, ok := cdromDisks[volume.Name]; ok {
				ejectedCDRoms[volume.Name] = volume
			}
		}
	}
	return ejectedCDRoms
}

func getInsertedCDRoms(volumes map[string]v1.Volume, disks map[string]v1.Disk) map[string]v1.Volume {
	cdromDisks := make(map[string]v1.Disk, 0)
	for _, disk := range disks {
		if disk.CDRom != nil && disk.Name != "" {
			cdromDisks[disk.Name] = v1.Disk{}
		}
	}
	insertedCDRoms := make(map[string]v1.Volume, 0)
	for _, volume := range volumes {
		if volume.EjectedCDRom == nil && (volume.DataVolume != nil || volume.PersistentVolumeClaim != nil) {
			if _, ok := cdromDisks[volume.Name]; ok {
				insertedCDRoms[volume.Name] = volume
			}
		}
	}
	return insertedCDRoms
}

func getOverlap(volumes1, volumes2 map[string]v1.Volume) map[string]v1.Volume {
	overlappedVolumes := make(map[string]v1.Volume)
	for k, vol1 := range volumes1 {
		if _, ok := volumes2[k]; ok {
			overlappedVolumes[k] = vol1
		}
	}

	return overlappedVolumes
}

func admitVMILabelsUpdate(
	newVMI *v1.VirtualMachineInstance,
	oldVMI *v1.VirtualMachineInstance,
	ar *admissionv1.AdmissionReview) *admissionv1.AdmissionResponse {

	if webhooks.IsKubeVirtServiceAccount(ar.Request.UserInfo.Username) {
		return nil
	}

	oldLabels := filterKubevirtLabels(oldVMI.ObjectMeta.Labels)
	newLabels := filterKubevirtLabels(newVMI.ObjectMeta.Labels)

	if !equality.Semantic.DeepEqual(oldLabels, newLabels) {
		return webhookutils.ToAdmissionResponse([]metav1.StatusCause{
			{
				Type:    metav1.CauseTypeFieldValueNotSupported,
				Message: "modification of the following reserved kubevirt.io/ labels on a VMI object is prohibited",
			},
		})
	}

	return nil
}

func filterKubevirtLabels(labels map[string]string) map[string]string {
	m := make(map[string]string)
	if len(labels) == 0 {
		// Return the empty map to avoid edge cases
		return m
	}
	for label, value := range labels {
		if _, ok := restrictedVmiLabels[label]; ok {
			m[label] = value
		}
	}

	return m
}
