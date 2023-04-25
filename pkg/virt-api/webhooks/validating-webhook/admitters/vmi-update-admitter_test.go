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
	"encoding/json"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/types"
	admissionv1 "k8s.io/api/admission/v1"
	authv1 "k8s.io/api/authentication/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"kubevirt.io/client-go/api"

	v1 "kubevirt.io/api/core/v1"

	"kubevirt.io/kubevirt/pkg/testutils"
	webhookutils "kubevirt.io/kubevirt/pkg/util/webhooks"
	"kubevirt.io/kubevirt/pkg/virt-api/webhooks"
	"kubevirt.io/kubevirt/pkg/virt-operator/resource/generate/components"
)

var _ = Describe("Validating VMIUpdate Admitter", func() {
	kv := &v1.KubeVirt{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "kubevirt",
			Namespace: "kubevirt",
		},
		Spec: v1.KubeVirtSpec{
			Configuration: v1.KubeVirtConfiguration{
				DeveloperConfiguration: &v1.DeveloperConfiguration{},
			},
		},
		Status: v1.KubeVirtStatus{
			Phase: v1.KubeVirtPhaseDeploying,
		},
	}
	config, _, _ := testutils.NewFakeClusterConfigUsingKV(kv)
	vmiUpdateAdmitter := &VMIUpdateAdmitter{config}

	DescribeTable("should reject documents containing unknown or missing fields for", func(data string, validationResult string, gvr metav1.GroupVersionResource, review func(ar *admissionv1.AdmissionReview) *admissionv1.AdmissionResponse) {
		input := map[string]interface{}{}
		json.Unmarshal([]byte(data), &input)

		ar := &admissionv1.AdmissionReview{
			Request: &admissionv1.AdmissionRequest{
				Resource: gvr,
				Object: runtime.RawExtension{
					Raw: []byte(data),
				},
			},
		}
		resp := review(ar)
		Expect(resp.Allowed).To(BeFalse())
		Expect(resp.Result.Message).To(Equal(validationResult))
	},
		Entry("VirtualMachineInstance update",
			`{"very": "unknown", "spec": { "extremely": "unknown" }}`,
			`.very in body is a forbidden property, spec.extremely in body is a forbidden property, spec.domain in body is required`,
			webhooks.VirtualMachineInstanceGroupVersionResource,
			vmiUpdateAdmitter.Admit,
		),
	)

	It("should reject valid VirtualMachineInstance spec on update", func() {
		vmi := api.NewMinimalVMI("testvmi")

		updateVmi := vmi.DeepCopy()
		updateVmi.Spec.Domain.Devices.Disks = append(vmi.Spec.Domain.Devices.Disks, v1.Disk{
			Name: "testdisk",
		})
		updateVmi.Spec.Volumes = append(vmi.Spec.Volumes, v1.Volume{
			Name: "testdisk",
			VolumeSource: v1.VolumeSource{
				ContainerDisk: testutils.NewFakeContainerDiskSource(),
			},
		})
		newVMIBytes, _ := json.Marshal(&updateVmi)
		oldVMIBytes, _ := json.Marshal(&vmi)

		ar := &admissionv1.AdmissionReview{
			Request: &admissionv1.AdmissionRequest{
				Resource: webhooks.VirtualMachineInstanceGroupVersionResource,
				Object: runtime.RawExtension{
					Raw: newVMIBytes,
				},
				OldObject: runtime.RawExtension{
					Raw: oldVMIBytes,
				},
				Operation: admissionv1.Update,
			},
		}

		resp := vmiUpdateAdmitter.Admit(ar)
		Expect(resp.Allowed).To(BeFalse())
		Expect(resp.Result.Details.Causes).To(HaveLen(1))
		Expect(resp.Result.Details.Causes[0].Message).To(Equal("update of VMI object is restricted"))
	})

	DescribeTable(
		"Should allow VMI upon modification of non kubevirt.io/ labels by non kubevirt user or service account",
		func(originalVmiLabels map[string]string, updateVmiLabels map[string]string) {
			vmi := api.NewMinimalVMI("testvmi")
			updateVmi := vmi.DeepCopy() // Don't need to copy the labels
			vmi.Labels = originalVmiLabels
			updateVmi.Labels = updateVmiLabels
			newVMIBytes, _ := json.Marshal(&updateVmi)
			oldVMIBytes, _ := json.Marshal(&vmi)
			ar := &admissionv1.AdmissionReview{
				Request: &admissionv1.AdmissionRequest{
					UserInfo: authv1.UserInfo{Username: "system:serviceaccount:someNamespace:someUser"},
					Resource: webhooks.VirtualMachineInstanceGroupVersionResource,
					Object: runtime.RawExtension{
						Raw: newVMIBytes,
					},
					OldObject: runtime.RawExtension{
						Raw: oldVMIBytes,
					},
					Operation: admissionv1.Update,
				},
			}
			resp := admitVMILabelsUpdate(updateVmi, vmi, ar)
			Expect(resp).To(BeNil())
		},
		Entry("Update of an existing label",
			map[string]string{"kubevirt.io/l": "someValue", "other-label/l": "value"},
			map[string]string{"kubevirt.io/l": "someValue", "other-label/l": "newValue"},
		),
		Entry("Add a new label when no labels we defined at all",
			nil,
			map[string]string{"l": "someValue"},
		),
		Entry("Delete a label",
			map[string]string{"kubevirt.io/l": "someValue", "l": "anotherValue"},
			map[string]string{"kubevirt.io/l": "someValue"},
		),
		Entry("Delete all labels",
			map[string]string{"l": "someValue", "l2": "anotherValue"},
			nil,
		),
	)

	DescribeTable(
		"Should allow VMI upon modification of kubevirt.io/ labels by kubevirt internal service account",
		func(originalVmiLabels map[string]string, updateVmiLabels map[string]string, serviceAccount string) {
			vmi := api.NewMinimalVMI("testvmi")
			updateVmi := vmi.DeepCopy() // Don't need to copy the labels
			vmi.Labels = originalVmiLabels
			updateVmi.Labels = updateVmiLabels
			newVMIBytes, _ := json.Marshal(&updateVmi)
			oldVMIBytes, _ := json.Marshal(&vmi)
			ar := &admissionv1.AdmissionReview{
				Request: &admissionv1.AdmissionRequest{
					UserInfo: authv1.UserInfo{Username: "system:serviceaccount:kubevirt:" + serviceAccount},
					Resource: webhooks.VirtualMachineInstanceGroupVersionResource,
					Object: runtime.RawExtension{
						Raw: newVMIBytes,
					},
					OldObject: runtime.RawExtension{
						Raw: oldVMIBytes,
					},
					Operation: admissionv1.Update,
				},
			}
			resp := admitVMILabelsUpdate(updateVmi, vmi, ar)
			Expect(resp).To(BeNil())
		},
		Entry("Update by API",
			map[string]string{v1.NodeNameLabel: "someValue"},
			map[string]string{v1.NodeNameLabel: "someNewValue"},
			components.ApiServiceAccountName,
		),
		Entry("Update by Handler",
			map[string]string{v1.NodeNameLabel: "someValue"},
			map[string]string{v1.NodeNameLabel: "someNewValue"},
			components.HandlerServiceAccountName,
		),
		Entry("Update by Controller",
			map[string]string{v1.NodeNameLabel: "someValue"},
			map[string]string{v1.NodeNameLabel: "someNewValue"},
			components.ControllerServiceAccountName,
		),
	)

	DescribeTable(
		"Should reject VMI upon modification of kubevirt.io/ reserved labels by non kubevirt user or service account",
		func(originalVmiLabels map[string]string, updateVmiLabels map[string]string) {
			vmi := api.NewMinimalVMI("testvmi")
			updateVmi := vmi.DeepCopy() // Don't need to copy the labels
			vmi.Labels = originalVmiLabels
			updateVmi.Labels = updateVmiLabels
			newVMIBytes, _ := json.Marshal(&updateVmi)
			oldVMIBytes, _ := json.Marshal(&vmi)
			ar := &admissionv1.AdmissionReview{
				Request: &admissionv1.AdmissionRequest{
					UserInfo: authv1.UserInfo{Username: "system:serviceaccount:someNamespace:someUser"},
					Resource: webhooks.VirtualMachineInstanceGroupVersionResource,
					Object: runtime.RawExtension{
						Raw: newVMIBytes,
					},
					OldObject: runtime.RawExtension{
						Raw: oldVMIBytes,
					},
					Operation: admissionv1.Update,
				},
			}
			resp := admitVMILabelsUpdate(updateVmi, vmi, ar)
			Expect(resp.Allowed).To(BeFalse())
			Expect(resp.Result.Details.Causes).To(HaveLen(1))
			Expect(resp.Result.Details.Causes[0].Message).To(Equal("modification of the following reserved kubevirt.io/ labels on a VMI object is prohibited"))
		},
		Entry("Update of an existing label",
			map[string]string{v1.CreatedByLabel: "someValue"},
			map[string]string{v1.CreatedByLabel: "someNewValue"},
		),
		Entry("Add kubevirt.io/ label when no labels we defined at all",
			nil,
			map[string]string{v1.CreatedByLabel: "someValue"},
		),
		Entry("Delete kubevirt.io/ label",
			map[string]string{"kubevirt.io/l": "someValue", v1.CreatedByLabel: "anotherValue"},
			map[string]string{"kubevirt.io/l": "someValue"},
		),
		Entry("Delete all kubevirt.io/ labels",
			map[string]string{v1.CreatedByLabel: "someValue", "kubevirt.io/l2": "anotherValue"},
			nil,
		),
	)

	makeMapFromVolumes := func(volumes []v1.Volume) map[string]v1.Volume {
		res := make(map[string]v1.Volume, 0)
		for _, volume := range volumes {
			res[volume.Name] = volume
		}
		return res
	}

	makeMapFromDisks := func(disks []v1.Disk) map[string]v1.Disk {
		res := make(map[string]v1.Disk, 0)
		for _, disk := range disks {
			res[disk.Name] = disk
		}
		return res
	}

	makeVolume := func(name string, kind string, isHotPluggable bool) v1.Volume {
		volume := v1.Volume{}
		volume.Name = fmt.Sprintf("volume-name-%s", name)

		if kind == "pvc" {
			volume.VolumeSource = v1.VolumeSource{
				PersistentVolumeClaim: &v1.PersistentVolumeClaimVolumeSource{
					Hotpluggable: isHotPluggable,
				},
			}
		} else if kind == "dv" {
			volume.VolumeSource = v1.VolumeSource{
				DataVolume: &v1.DataVolumeSource{
					Hotpluggable: isHotPluggable,
					Name:         fmt.Sprintf("dv-name-%s", name),
				},
			}
		} else if kind == "ejectedcdrom" {
			volume.VolumeSource = v1.VolumeSource{
				EjectedCDRom: &v1.EjectedCDRomSource{},
			}
		}

		return volume
	}

	makeDisk := func(name string, isCDRom bool) v1.Disk {
		disk := v1.Disk{}
		disk.Name = fmt.Sprintf("volume-name-%s", name)

		if isCDRom {
			disk.DiskDevice = v1.DiskDevice{
				CDRom: &v1.CDRomTarget{
					Bus: "sata",
				},
			}
		} else {
			disk.DiskDevice = v1.DiskDevice{
				Disk: &v1.DiskTarget{
					Bus: "scsi",
				},
			}
		}

		return disk
	}

	makeStatus := func(name string, isHotplug bool) v1.VolumeStatus {
		status := v1.VolumeStatus{
			Name:   fmt.Sprintf("volume-name-%s", name),
			Target: fmt.Sprintf("volume-target-%s", name),
		}

		if isHotplug {
			status.HotplugVolume = &v1.HotplugVolumeStatus{
				AttachPodName: fmt.Sprintf("test-pod-%s", name),
			}
			status.Phase = v1.VolumeReady
		}

		return status
	}

	makeVolumes := func(indexes ...int) []v1.Volume {
		res := make([]v1.Volume, 0)
		for _, index := range indexes {
			res = append(res, v1.Volume{
				Name: fmt.Sprintf("volume-name-%d", index),
				VolumeSource: v1.VolumeSource{
					DataVolume: &v1.DataVolumeSource{
						Name: fmt.Sprintf("dv-name-%d", index),
					},
				},
			})
		}
		return res
	}

	makeVolumesWithMemoryDumpVol := func(total int, indexes ...int) []v1.Volume {
		res := make([]v1.Volume, 0)
		for i := 0; i < total; i++ {
			memoryDump := false
			for _, index := range indexes {
				if i == index {
					memoryDump = true
					res = append(res, v1.Volume{
						Name: fmt.Sprintf("volume-name-%d", index),
						VolumeSource: v1.VolumeSource{
							MemoryDump: testutils.NewFakeMemoryDumpSource(fmt.Sprintf("volume-name-%d", index)),
						},
					})
				}
			}
			if !memoryDump {
				res = append(res, v1.Volume{
					Name: fmt.Sprintf("volume-name-%d", i),
					VolumeSource: v1.VolumeSource{
						DataVolume: &v1.DataVolumeSource{
							Name: fmt.Sprintf("dv-name-%d", i),
						},
					},
				})
			}
		}
		return res
	}

	makeDisks := func(indexes ...int) []v1.Disk {
		res := make([]v1.Disk, 0)
		for _, index := range indexes {
			bootOrder := uint(index + 1)
			res = append(res, v1.Disk{
				Name: fmt.Sprintf("volume-name-%d", index),
				DiskDevice: v1.DiskDevice{
					Disk: &v1.DiskTarget{
						Bus: "scsi",
					},
				},
				BootOrder: &bootOrder,
			})
		}
		return res
	}

	makeDisksInvalidBusLastDisk := func(indexes ...int) []v1.Disk {
		res := makeDisks(indexes...)
		for i, index := range indexes {
			if i == len(indexes)-1 {
				res[index].Disk.Bus = "invalid"
			}
		}
		return res
	}

	makeDisksInvalidBootOrder := func(indexes ...int) []v1.Disk {
		res := makeDisks(indexes...)
		bootOrder := uint(0)
		for i, index := range indexes {
			if i == len(indexes)-1 {
				res[index].BootOrder = &bootOrder
			}
		}
		return res
	}

	makeStatuses := func(statusCount, hotplugCount int) []v1.VolumeStatus {
		res := make([]v1.VolumeStatus, 0)
		for i := 0; i < statusCount; i++ {
			res = append(res, v1.VolumeStatus{
				Name:   fmt.Sprintf("volume-name-%d", i),
				Target: fmt.Sprintf("volume-target-%d", i),
			})
			if i >= statusCount-hotplugCount {
				res[i].HotplugVolume = &v1.HotplugVolumeStatus{
					AttachPodName: fmt.Sprintf("test-pod-%d", i),
				}
				res[i].Phase = v1.VolumeReady
			}
		}
		return res
	}

	makeExpected := func(message, field string) *admissionv1.AdmissionResponse {
		return webhookutils.ToAdmissionResponse([]metav1.StatusCause{
			{
				Type:    metav1.CauseTypeFieldValueInvalid,
				Message: message,
				Field:   field,
			},
		})
	}

	DescribeTable("Should calculate the hotplugvolumes", func(volumes []v1.Volume, statuses []v1.VolumeStatus, expected []v1.Volume) {
		result := getHotplugVolumes(volumes, statuses)
		Expect(equality.Semantic.DeepEqual(result, makeMapFromVolumes(expected))).To(BeTrue(), "result: %v and expected: %v do not match", result, expected)
	},
		Entry("Should be empty if statuses is empty", makeVolumes(), makeStatuses(0, 0), []v1.Volume{}),
		Entry("Should be empty if statuses has multiple entries, but no hotplug", makeVolumes(), makeStatuses(2, 0), []v1.Volume{}),
		Entry("Should be empty if statuses has one entry, but no hotplug", makeVolumes(), makeStatuses(1, 0), []v1.Volume{}),
		Entry("Should have a single hotplug if status has one hotplug", makeVolumes(0, 1), makeStatuses(2, 1), makeVolumes(1)),
		Entry("Should have a multiple hotplug if status has multiple hotplug", makeVolumes(0, 1, 2, 3), makeStatuses(4, 2), makeVolumes(2, 3)),
		Entry("Should be empty if everything is empty", makeVolumes(), []v1.VolumeStatus{}, []v1.Volume{}),
		Entry("Should be empty if there are only non-hotplugged volumes and non-hotplugged volume statuses",
			[]v1.Volume{
				makeVolume("vol1", "dv", false),
				makeVolume("vol2", "pvc", false),
			},
			[]v1.VolumeStatus{
				makeStatus("vol1", false),
				makeStatus("vol2", false),
			},
			[]v1.Volume{},
		),
		Entry("Should get a hotplugged pvc",
			[]v1.Volume{
				makeVolume("vol1", "dv", false),
				makeVolume("vol2", "pvc", true),
			},
			[]v1.VolumeStatus{
				makeStatus("vol1", false),
				makeStatus("vol2", false),
			},
			[]v1.Volume{
				makeVolume("vol2", "pvc", true),
			},
		),
		Entry("Should get a hotplugged dv",
			[]v1.Volume{
				makeVolume("vol1", "dv", true),
				makeVolume("vol2", "pvc", false),
			},
			[]v1.VolumeStatus{
				makeStatus("vol1", false),
				makeStatus("vol2", false),
			},
			[]v1.Volume{
				makeVolume("vol1", "dv", true),
			},
		),
		Entry("Should get an ejected cdrom",
			[]v1.Volume{
				makeVolume("vol1", "dv", false),
				makeVolume("vol2", "ejectedcdrom", true),
				makeVolume("vol3", "pvc", false),
			},
			[]v1.VolumeStatus{
				makeStatus("vol1", false),
				makeStatus("vol2", false),
				makeStatus("vol3", false),
			},
			[]v1.Volume{
				makeVolume("vol2", "ejectedcdrom", true),
			},
		),
		Entry("Should get multiple hotplugged volumes",
			[]v1.Volume{
				makeVolume("vol1", "dv", false),
				makeVolume("vol2", "dv", true),
				makeVolume("vol3", "ejectedcdrom", true),
				makeVolume("vol4", "pvc", false),
				makeVolume("vol5", "pvc", true),
			},
			[]v1.VolumeStatus{
				makeStatus("vol1", false),
				makeStatus("vol2", false),
				makeStatus("vol3", false),
				makeStatus("vol4", false),
				makeStatus("vol5", false),
			},
			[]v1.Volume{
				makeVolume("vol2", "dv", true),
				makeVolume("vol3", "ejectedcdrom", true),
				makeVolume("vol5", "pvc", true),
			},
		),
		Entry("Should consider volume hotplugged if not in status",
			[]v1.Volume{
				makeVolume("vol1", "dv", false),
				makeVolume("vol2", "dv", true),
				makeVolume("vol3", "ejectedcdrom", true),
				makeVolume("vol4", "pvc", false),
				makeVolume("vol5", "pvc", true),
			},
			nil,
			[]v1.Volume{
				makeVolume("vol1", "dv", false),
				makeVolume("vol2", "dv", true),
				makeVolume("vol3", "ejectedcdrom", true),
				makeVolume("vol4", "pvc", false),
				makeVolume("vol5", "pvc", true),
			},
		),
		Entry("Should consider volume hotplugged if hotplug in status",
			[]v1.Volume{
				makeVolume("vol1", "dv", false),
			},
			[]v1.VolumeStatus{
				makeStatus("vol1", true),
			},
			[]v1.Volume{
				makeVolume("vol1", "dv", false),
			},
		),
	)

	DescribeTable("Should calculate the permanent volumes", func(volumes []v1.Volume, volumeStatuses []v1.VolumeStatus, expected []v1.Volume) {
		result := getPermanentVolumes(volumes, volumeStatuses)
		Expect(equality.Semantic.DeepEqual(result, makeMapFromVolumes(expected))).To(BeTrue(), "result: %v and expected: %v do not match", result, expected)
	},
		Entry("Should be empty if volume is empty", makeVolumes(), makeStatuses(0, 0), []v1.Volume{}),
		Entry("Should be empty if all volumes are hotplugged", makeVolumes(0, 1, 2, 3), makeStatuses(4, 4), []v1.Volume{}),
		Entry("Should return all volumes if hotplugged is empty with multiple volumes", makeVolumes(0, 1, 2, 3), makeStatuses(4, 0), makeVolumes(0, 1, 2, 3)),
		Entry("Should return all volumes if hotplugged is empty with a single volume", makeVolumes(0), makeStatuses(1, 0), makeVolumes(0)),
		Entry("Should return 3 volumes if  1 hotplugged volume", makeVolumes(0, 1, 2, 3), makeStatuses(4, 1), makeVolumes(0, 1, 2)),
		Entry("Should be empty if all is empty", makeVolumes(), []v1.VolumeStatus{}, []v1.Volume{}),
		Entry("Should be empty if there are only hotplugged volumes",
			[]v1.Volume{
				makeVolume("vol1", "dv", true),
				makeVolume("vol2", "pvc", true),
				makeVolume("vol3", "ejectedcdrom", true),
			},
			[]v1.VolumeStatus{
				makeStatus("vol1", false),
				makeStatus("vol2", false),
				makeStatus("vol3", false),
			},
			[]v1.Volume{},
		),
		Entry("Should get a non-hotplugged pvc",
			[]v1.Volume{
				makeVolume("vol1", "dv", true),
				makeVolume("vol2", "pvc", false),
				makeVolume("vol3", "ejectedcdrom", true),
			},
			[]v1.VolumeStatus{
				makeStatus("vol1", false),
				makeStatus("vol2", false),
				makeStatus("vol3", false),
			},
			[]v1.Volume{
				makeVolume("vol2", "pvc", false),
			},
		),
		Entry("Should get a non-hotplugged dv",
			[]v1.Volume{
				makeVolume("vol1", "dv", false),
				makeVolume("vol2", "pvc", true),
				makeVolume("vol3", "ejectedcdrom", true),
			},
			[]v1.VolumeStatus{
				makeStatus("vol1", false),
				makeStatus("vol2", false),
				makeStatus("vol3", false),
			},
			[]v1.Volume{
				makeVolume("vol1", "dv", false),
			},
		),
		Entry("Should get multiple non-hotplugged volumes",
			[]v1.Volume{
				makeVolume("vol1", "dv", false),
				makeVolume("vol2", "dv", true),
				makeVolume("vol3", "ejectedcdrom", true),
				makeVolume("vol4", "pvc", false),
				makeVolume("vol5", "pvc", true),
			},
			[]v1.VolumeStatus{
				makeStatus("vol1", false),
				makeStatus("vol2", false),
				makeStatus("vol3", false),
				makeStatus("vol4", false),
				makeStatus("vol5", false),
			},
			[]v1.Volume{
				makeVolume("vol1", "dv", false),
				makeVolume("vol4", "pvc", false),
			},
		),
		Entry("Should be empty when all volumes are considered hotplugged due to empty status",
			[]v1.Volume{
				makeVolume("vol1", "dv", false),
				makeVolume("vol2", "dv", true),
				makeVolume("vol3", "ejectedcdrom", true),
				makeVolume("vol4", "pvc", false),
				makeVolume("vol5", "pvc", true),
			},
			[]v1.VolumeStatus{},
			[]v1.Volume{},
		),
		Entry("Should ignore hotplugged volumes based on their status",
			[]v1.Volume{
				makeVolume("vol1", "dv", false),
				makeVolume("vol2", "dv", false),
				makeVolume("vol4", "pvc", false),
				makeVolume("vol5", "pvc", false),
			},
			[]v1.VolumeStatus{
				makeStatus("vol1", false),
				makeStatus("vol2", true),
				makeStatus("vol4", false),
				makeStatus("vol5", true),
			},
			[]v1.Volume{
				makeVolume("vol1", "dv", false),
				makeVolume("vol4", "pvc", false),
			},
		),
	)

	DescribeTable("Should get inserted cdroms", func(volumes []v1.Volume, disks []v1.Disk, expected []v1.Volume) {
		res := getInsertedCDRoms(makeMapFromVolumes(volumes), makeMapFromDisks(disks))
		Expect(equality.Semantic.DeepEqual(res, makeMapFromVolumes(expected))).To(BeTrue(), "result: %v and expected: %v do not match", res, expected)
	},
		Entry("Should be empty if there are no volumes", makeVolumes(), []v1.Disk{makeDisk("vol1", true)}, []v1.Volume{}),
		Entry("Should be empty if there are no disks", []v1.Volume{makeVolume("vol1", "dv", true)}, []v1.Disk{}, []v1.Volume{}),
		Entry("Should ignore ejected cdroms volumes",
			[]v1.Volume{
				makeVolume("vol3", "ejectedcdrom", true),
			},
			[]v1.Disk{
				makeDisk("vol3", true),
			},
			[]v1.Volume{},
		),
		Entry("Should get non-ejected volumes",
			[]v1.Volume{
				makeVolume("vol1", "dv", true),
				makeVolume("vol5", "pvc", true),
			},
			[]v1.Disk{
				makeDisk("vol1", true),
				makeDisk("vol5", true),
			},
			[]v1.Volume{
				makeVolume("vol1", "dv", true),
				makeVolume("vol5", "pvc", true),
			},
		),
		Entry("Should ignore non-cdrom corresponding disks",
			[]v1.Volume{
				makeVolume("vol1", "dv", true),
				makeVolume("vol2", "dv", true),
				makeVolume("vol4", "pvc", true),
				makeVolume("vol5", "pvc", true),
			},
			[]v1.Disk{
				makeDisk("vol1", true),
				makeDisk("vol2", false),
				makeDisk("vol4", false),
				makeDisk("vol5", true),
			},
			[]v1.Volume{
				makeVolume("vol1", "dv", true),
				makeVolume("vol5", "pvc", true),
			},
		),
		Entry("Should ignore non matching names volumes",
			[]v1.Volume{
				makeVolume("vol1", "dv", true),
				makeVolume("vol2", "dv", true),
				makeVolume("vol3", "ejectedcdrom", true),
				makeVolume("vol4", "pvc", true),
				makeVolume("vol5", "pvc", true),
			},
			[]v1.Disk{
				makeDisk("vol6", true),
				makeDisk("vol7", false),
				makeDisk("vol8", true),
				makeDisk("vol9", false),
				makeDisk("vol10", true),
			},
			[]v1.Volume{},
		),
		Entry("Should follow multiple rules at once",
			[]v1.Volume{
				makeVolume("vol1", "dv", true),
				makeVolume("vol2", "dv", true),
				makeVolume("vol3", "ejectedcdrom", true),
				makeVolume("vol4", "pvc", true),
				makeVolume("vol5", "pvc", true),
			},
			[]v1.Disk{
				makeDisk("vol1", true),
				makeDisk("vol2", false),
				makeDisk("vol3", true),
				makeDisk("vol4", false),
				makeDisk("vol9", true),
			},
			[]v1.Volume{
				makeVolume("vol1", "dv", true),
			},
		),
	)

	DescribeTable("Should get ejected cdroms", func(volumes []v1.Volume, disks []v1.Disk, expected []v1.Volume) {
		res := getEjectedCDRoms(makeMapFromVolumes(volumes), makeMapFromDisks(disks))
		Expect(equality.Semantic.DeepEqual(res, makeMapFromVolumes(expected))).To(BeTrue(), "result: %v and expected: %v do not match", res, expected)
	},
		Entry("Should be empty if there are no volumes", makeVolumes(), []v1.Disk{makeDisk("vol1", true)}, []v1.Volume{}),
		Entry("Should be empty if there are no disks", []v1.Volume{makeVolume("vol1", "dv", true)}, []v1.Disk{}, []v1.Volume{}),
		Entry("Should get ejected cdroms volumes",
			[]v1.Volume{
				makeVolume("vol3", "ejectedcdrom", true),
			},
			[]v1.Disk{
				makeDisk("vol3", true),
			},
			[]v1.Volume{
				makeVolume("vol3", "ejectedcdrom", true),
			},
		),
		Entry("Should ignore non-ejected volumes",
			[]v1.Volume{
				makeVolume("vol1", "dv", true),
				makeVolume("vol5", "pvc", true),
			},
			[]v1.Disk{
				makeDisk("vol1", true),
				makeDisk("vol5", true),
			},
			[]v1.Volume{},
		),
		Entry("Should ignore non-cdrom corresponding disks",
			[]v1.Volume{
				makeVolume("vol1", "ejectedcdrom", true),
				makeVolume("vol2", "ejectedcdrom", true),
			},
			[]v1.Disk{
				makeDisk("vol1", true),
				makeDisk("vol2", false),
			},
			[]v1.Volume{
				makeVolume("vol1", "ejectedcdrom", true),
			},
		),
		Entry("Should ignore non matching names volumes",
			[]v1.Volume{
				makeVolume("vol1", "ejectedcdrom", true),
				makeVolume("vol2", "dv", true),
				makeVolume("vol3", "ejectedcdrom", true),
				makeVolume("vol4", "pvc", true),
				makeVolume("vol5", "pvc", true),
			},
			[]v1.Disk{
				makeDisk("vol6", true),
				makeDisk("vol7", false),
				makeDisk("vol8", true),
				makeDisk("vol9", false),
				makeDisk("vol10", true),
			},
			[]v1.Volume{},
		),
		Entry("Should follow multiple rules at once",
			[]v1.Volume{
				makeVolume("vol1", "dv", true),
				makeVolume("vol2", "dv", true),
				makeVolume("vol3", "ejectedcdrom", true),
				makeVolume("vol33", "ejectedcdrom", true),
				makeVolume("vol4", "pvc", true),
				makeVolume("vol5", "pvc", true),
			},
			[]v1.Disk{
				makeDisk("vol1", true),
				makeDisk("vol2", false),
				makeDisk("vol3", true),
				makeDisk("vol33", false),
				makeDisk("vol4", false),
				makeDisk("vol9", true),
			},
			[]v1.Volume{
				makeVolume("vol3", "ejectedcdrom", true),
			},
		),
	)

	DescribeTable("Should get overlap", func(volumes []v1.Volume, volumes2 []v1.Volume, expected []v1.Volume) {
		res := getOverlap(makeMapFromVolumes(volumes), makeMapFromVolumes(volumes2))
		Expect(equality.Semantic.DeepEqual(res, makeMapFromVolumes(expected))).To(BeTrue(), "result: %v and expected: %v do not match", res, expected)
	},
		Entry("Should be empty if there are no volumes", makeVolumes(), []v1.Volume{makeVolume("vol1", "dv", true)}, []v1.Volume{}),
		Entry("Should be empty if there are no secondary volumes", []v1.Volume{makeVolume("vol1", "dv", true)}, makeVolumes(), []v1.Volume{}),
		Entry("Should get overlap of multiple volumes",
			[]v1.Volume{
				makeVolume("vol1", "dv", true),
				makeVolume("vol2", "dv", true),
				makeVolume("notin2", "dv", true),
			},
			[]v1.Volume{
				makeVolume("vol1", "dv", true),
				makeVolume("notin1", "dv", false),
				makeVolume("notin1", "dv", true),
				makeVolume("notin1", "dv", false),
				makeVolume("vol2", "dv", false),
				makeVolume("notin1", "dv", true),
			},
			[]v1.Volume{
				makeVolume("vol1", "dv", true),
				makeVolume("vol2", "dv", true),
			},
		),
	)

	DescribeTable("Should return proper admission response", func(newVolumes, oldVolumes []v1.Volume, newDisks, oldDisks []v1.Disk, volumeStatuses []v1.VolumeStatus, expected *admissionv1.AdmissionResponse) {
		newVMI := api.NewMinimalVMI("testvmi")
		newVMI.Spec.Volumes = newVolumes
		newVMI.Spec.Domain.Devices.Disks = newDisks

		result := admitHotplug(newVolumes, oldVolumes, newDisks, oldDisks, volumeStatuses, newVMI, vmiUpdateAdmitter.ClusterConfig)
		Expect(equality.Semantic.DeepEqual(result, expected)).To(BeTrue(), "result: %v and expected: %v do not match", result, expected)
	},
		Entry("Should accept if no volumes are there or added",
			makeVolumes(),
			makeVolumes(),
			makeDisks(),
			makeDisks(),
			[]v1.VolumeStatus{},
			nil),
		Entry("Should reject if #volumes != #disks",
			makeVolumes(1, 2),
			makeVolumes(1, 2),
			makeDisks(1),
			makeDisks(1),
			[]v1.VolumeStatus{makeStatus("0", false)},
			makeExpected("number of disks (1) does not equal the number of volumes (2)", "")),
		Entry("Should reject if we remove a permanent volume",
			makeVolumes(),
			makeVolumes(0),
			makeDisks(),
			makeDisks(0),
			[]v1.VolumeStatus{makeStatus("0", false)},
			makeExpected("Number of permanent volumes has changed", "")),
		Entry("Should reject if we add a disk without a matching volume",
			[]v1.Volume{
				makeVolume("vol1", "dv", true),
				makeVolume("vol2", "dv", true),
			},
			[]v1.Volume{
				makeVolume("vol1", "dv", true),
			},
			[]v1.Disk{
				makeDisk("vol1", false),
				makeDisk("notmatching", false),
			},
			[]v1.Disk{
				makeDisk("vol1", false),
			},
			[]v1.VolumeStatus{
				makeStatus("vol1", false),
				makeStatus("vol2", false),
			},
			makeExpected("Disk volume-name-vol2 does not exist", "")),
		Entry("Should reject if we modify existing volume to be invalid",
			makeVolumes(0, 1),
			makeVolumes(0, 1),
			[]v1.Disk{
				makeDisk("notmatching", false),
				makeDisk("1", false),
			},
			[]v1.Disk{
				makeDisk("0", false),
				makeDisk("1", false),
			},
			[]v1.VolumeStatus{
				makeStatus("0", false),
				makeStatus("1", false),
			},
			makeExpected("permanent disk volume-name-0, changed", "")),
		Entry("Should reject if a hotplug volume changed",
			[]v1.Volume{
				{
					Name: "volume-name-vol1",
					VolumeSource: v1.VolumeSource{
						ContainerDisk: &v1.ContainerDiskSource{},
					},
				},
				makeVolume("vol2", "dv", true),
			},
			[]v1.Volume{
				makeVolume("vol1", "dv", true),
				makeVolume("vol2", "dv", true),
			},
			[]v1.Disk{
				makeDisk("vol1", false),
				makeDisk("vol2", false),
			},
			[]v1.Disk{
				makeDisk("vol1", false),
				makeDisk("vol2", false),
			},
			[]v1.VolumeStatus{
				makeStatus("vol1", true),
				makeStatus("vol2", true),
			},
			makeExpected("hotplug volume volume-name-vol1, changed", "")),
		Entry("Should reject if we add volumes that are not PVC or DV",
			[]v1.Volume{
				makeVolume("0", "dv", false),
				{
					Name: "volume-name-1",
					VolumeSource: v1.VolumeSource{
						ContainerDisk: &v1.ContainerDiskSource{},
					},
				},
			},
			[]v1.Volume{
				makeVolume("0", "dv", false),
			},
			[]v1.Disk{
				makeDisk("1", false),
				makeDisk("0", false),
			},
			[]v1.Disk{
				makeDisk("0", false),
			},
			[]v1.VolumeStatus{
				makeStatus("0", true),
				makeStatus("1", true),
			},
			makeExpected("volume volume-name-1 is not a PVC or DataVolume", "")),
		Entry("Should accept if we add volumes and disk properly",
			makeVolumes(0, 1),
			makeVolumes(0, 1),
			makeDisks(0, 1),
			makeDisks(0, 1),
			[]v1.VolumeStatus{
				makeStatus("0", true),
				makeStatus("1", true),
			},
			nil),
		Entry("Should reject if we add disk with invalid bus",
			makeVolumes(0, 1),
			makeVolumes(0),
			makeDisksInvalidBusLastDisk(0, 1),
			makeDisks(0),
			[]v1.VolumeStatus{
				makeStatus("0", true),
				makeStatus("1", true),
			},
			makeExpected("hotplugged Disk volume-name-1 does not use a scsi bus", "")),
		Entry("Should reject if we add disk with invalid boot order",
			makeVolumes(0, 1),
			makeVolumes(0),
			makeDisksInvalidBootOrder(0, 1),
			makeDisks(0),
			[]v1.VolumeStatus{
				makeStatus("0", true),
				makeStatus("1", true),
			},
			makeExpected("spec.domain.devices.disks[1] must have a boot order > 0, if supplied", "spec.domain.devices.disks[1].bootOrder")),
		Entry("Should accept if memory dump volume exists without matching disk",
			makeVolumesWithMemoryDumpVol(3, 2),
			makeVolumes(0, 1),
			makeDisks(0, 1),
			makeDisks(0, 1),
			[]v1.VolumeStatus{
				makeStatus("0", true),
				makeStatus("1", true),
			},
			nil),
		Entry("Should reject if #volumes != #disks even when there is memory dump volume",
			makeVolumesWithMemoryDumpVol(3, 2),
			makeVolumesWithMemoryDumpVol(3, 2),
			makeDisks(1),
			makeDisks(1),
			[]v1.VolumeStatus{
				makeStatus("3", true),
				makeStatus("2", true),
			},
			makeExpected("number of disks (1) does not equal the number of volumes (2)", "")),
		Entry("Should reject if a cdrom is hotplugged",
			[]v1.Volume{
				makeVolume("vol1", "dv", true),
				makeVolume("vol2", "dv", true),
			},
			[]v1.Volume{
				makeVolume("vol1", "dv", true),
			},
			[]v1.Disk{
				makeDisk("vol1", false),
				makeDisk("vol2", true),
			},
			[]v1.Disk{
				makeDisk("vol1", false),
			},
			[]v1.VolumeStatus{
				makeStatus("vol1", true),
				makeStatus("vol2", true),
			},
			makeExpected("new hotplugged Disk volume-name-vol2 cannot have type cdrom", "")),
		Entry("Should accept a cdrom being ejected",
			[]v1.Volume{
				makeVolume("vol1", "dv", true),
				makeVolume("vol2", "ejectedcdrom", true),
			},
			[]v1.Volume{
				makeVolume("vol1", "dv", true),
				makeVolume("vol2", "dv", true),
			},
			[]v1.Disk{
				makeDisk("vol1", false),
				makeDisk("vol2", true),
			},
			[]v1.Disk{
				makeDisk("vol1", false),
				makeDisk("vol2", true),
			},
			[]v1.VolumeStatus{
				makeStatus("vol1", true),
				makeStatus("vol2", true),
			},
			nil),
		Entry("Should accept a cdrom being inserted",
			[]v1.Volume{
				makeVolume("vol1", "dv", true),
				makeVolume("vol2", "dv", true),
			},
			[]v1.Volume{
				makeVolume("vol1", "dv", true),
				makeVolume("vol2", "ejectedcdrom", true),
			},
			[]v1.Disk{
				makeDisk("vol1", false),
				makeDisk("vol2", true),
			},
			[]v1.Disk{
				makeDisk("vol1", false),
				makeDisk("vol2", true),
			},
			[]v1.VolumeStatus{
				makeStatus("vol1", true),
				makeStatus("vol2", true),
			},
			nil),
	)

	DescribeTable("Admit or deny based on user", func(user string, expected types.GomegaMatcher) {
		vmi := api.NewMinimalVMI("testvmi")
		vmi.Spec.Volumes = makeVolumes(1)
		vmi.Spec.Domain.Devices.Disks = makeDisks(1)
		vmi.Status.VolumeStatus = makeStatuses(1, 0)
		updateVmi := vmi.DeepCopy()
		updateVmi.Spec.Volumes = makeVolumes(2)
		updateVmi.Spec.Domain.Devices.Disks = makeDisks(2)
		updateVmi.Status.VolumeStatus = makeStatuses(2, 1)

		newVMIBytes, _ := json.Marshal(&updateVmi)
		oldVMIBytes, _ := json.Marshal(&vmi)
		ar := &admissionv1.AdmissionReview{
			Request: &admissionv1.AdmissionRequest{
				UserInfo: authv1.UserInfo{Username: user},
				Resource: webhooks.VirtualMachineInstanceGroupVersionResource,
				Object: runtime.RawExtension{
					Raw: newVMIBytes,
				},
				OldObject: runtime.RawExtension{
					Raw: oldVMIBytes,
				},
				Operation: admissionv1.Update,
			},
		}
		resp := vmiUpdateAdmitter.Admit(ar)
		Expect(resp.Allowed).To(expected)
	},
		Entry("Should admit internal sa", "system:serviceaccount:kubevirt:"+components.ApiServiceAccountName, BeTrue()),
		Entry("Should reject regular user", "system:serviceaccount:someNamespace:someUser", BeFalse()),
	)
})
