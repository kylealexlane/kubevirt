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
 * Copyright 2019 Red Hat, Inc.
 *
 */
package controller

import (
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	v1 "kubevirt.io/api/core/v1"
)

var _ = Describe("Controller", func() {
	makeStatus := func(name string, isHotplug bool) *v1.VolumeStatus {
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

		return &status
	}

	DescribeTable("Should replace a volume", func(vmiSpec v1.VirtualMachineInstanceSpec, newVolume v1.Volume, expectedVMIVolumes []v1.Volume) {
		replaceVolume(&vmiSpec, newVolume)
		Expect(vmiSpec.Volumes).To(Equal(expectedVMIVolumes))
	},
		Entry("valid replace",
			v1.VirtualMachineInstanceSpec{
				Volumes: []v1.Volume{
					{
						Name: "vol1",
						VolumeSource: v1.VolumeSource{
							PersistentVolumeClaim: &v1.PersistentVolumeClaimVolumeSource{},
						},
					},
					{
						Name: "vol2",
						VolumeSource: v1.VolumeSource{
							DataVolume: &v1.DataVolumeSource{},
						},
					},
					{
						Name: "vol3",
						VolumeSource: v1.VolumeSource{
							EjectedCDRom: &v1.EjectedCDRomSource{},
						},
					},
				},
			},
			v1.Volume{
				Name: "vol2",
				VolumeSource: v1.VolumeSource{
					EjectedCDRom: &v1.EjectedCDRomSource{},
				},
			},
			[]v1.Volume{
				{
					Name: "vol1",
					VolumeSource: v1.VolumeSource{
						PersistentVolumeClaim: &v1.PersistentVolumeClaimVolumeSource{},
					},
				},
				{
					Name: "vol2",
					VolumeSource: v1.VolumeSource{
						EjectedCDRom: &v1.EjectedCDRomSource{},
					},
				},
				{
					Name: "vol3",
					VolumeSource: v1.VolumeSource{
						EjectedCDRom: &v1.EjectedCDRomSource{},
					},
				},
			},
		),
		Entry("no matching name",
			v1.VirtualMachineInstanceSpec{
				Volumes: []v1.Volume{
					{
						Name: "vol1",
						VolumeSource: v1.VolumeSource{
							PersistentVolumeClaim: &v1.PersistentVolumeClaimVolumeSource{},
						},
					},
					{
						Name: "vol2",
						VolumeSource: v1.VolumeSource{
							DataVolume: &v1.DataVolumeSource{},
						},
					},
					{
						Name: "vol3",
						VolumeSource: v1.VolumeSource{
							EjectedCDRom: &v1.EjectedCDRomSource{},
						},
					},
				},
			},
			v1.Volume{
				Name: "vol4",
				VolumeSource: v1.VolumeSource{
					EjectedCDRom: &v1.EjectedCDRomSource{},
				},
			},
			[]v1.Volume{
				{
					Name: "vol1",
					VolumeSource: v1.VolumeSource{
						PersistentVolumeClaim: &v1.PersistentVolumeClaimVolumeSource{},
					},
				},
				{
					Name: "vol2",
					VolumeSource: v1.VolumeSource{
						DataVolume: &v1.DataVolumeSource{},
					},
				},
				{
					Name: "vol3",
					VolumeSource: v1.VolumeSource{
						EjectedCDRom: &v1.EjectedCDRomSource{},
					},
				},
			},
		),
	)
	DescribeTable("Should determine if volume is hotpluggable", func(volume v1.Volume, volumeStatus *v1.VolumeStatus, expectedRes bool) {
		res := IsHotpluggableVolume(volume, volumeStatus)
		Expect(res).To(Equal(expectedRes))
	},
		Entry(
			"with a non-hotpluggable datavolume",
			v1.Volume{
				Name: "volume-name-1",
				VolumeSource: v1.VolumeSource{
					DataVolume: &v1.DataVolumeSource{},
				},
			},
			makeStatus("1", false),
			false,
		),
		Entry(
			"with a hotpluggable datavolume",
			v1.Volume{
				Name: "volume-name-1",
				VolumeSource: v1.VolumeSource{
					DataVolume: &v1.DataVolumeSource{
						Hotpluggable: true,
					},
				},
			},
			makeStatus("1", false),
			true,
		),
		Entry(
			"with a non-hotpluggable PVC",
			v1.Volume{
				Name: "volume-name-1",
				VolumeSource: v1.VolumeSource{
					PersistentVolumeClaim: &v1.PersistentVolumeClaimVolumeSource{},
				},
			},
			makeStatus("1", false),
			false,
		),
		Entry(
			"with a hotpluggable PVC",
			v1.Volume{
				Name: "volume-name-1",
				VolumeSource: v1.VolumeSource{
					PersistentVolumeClaim: &v1.PersistentVolumeClaimVolumeSource{
						Hotpluggable: true,
					},
				},
			},
			makeStatus("1", false),
			true,
		),
		Entry(
			"with an ejected cdrom",
			v1.Volume{
				Name: "volume-name-1",
				VolumeSource: v1.VolumeSource{
					EjectedCDRom: &v1.EjectedCDRomSource{},
				},
			},
			makeStatus("1", false),
			true,
		),
		Entry(
			"with a hotpluggable memory dump",
			v1.Volume{
				Name: "volume-name-1",
				VolumeSource: v1.VolumeSource{
					MemoryDump: &v1.MemoryDumpVolumeSource{
						PersistentVolumeClaimVolumeSource: v1.PersistentVolumeClaimVolumeSource{
							Hotpluggable: true,
						},
					},
				},
			},
			makeStatus("1", false),
			true,
		),
		Entry(
			"with the status as hotpluggable",
			v1.Volume{
				Name: "volume-name-1",
				VolumeSource: v1.VolumeSource{
					PersistentVolumeClaim: &v1.PersistentVolumeClaimVolumeSource{
						Hotpluggable: false,
					},
				},
			},
			makeStatus("1", true),
			true,
		),
	)

	DescribeTable("Should get a volume status", func(vmi *v1.VirtualMachineInstance, name string, expectedStatus *v1.VolumeStatus) {
		volumeStatus := GetVolumeStatus(vmi, name)
		Expect(volumeStatus).To(Equal(expectedStatus))
	},
		Entry("valid volume status found",
			&v1.VirtualMachineInstance{
				Status: v1.VirtualMachineInstanceStatus{
					VolumeStatus: []v1.VolumeStatus{
						*makeStatus("1", false),
						*makeStatus("2", false),
						*makeStatus("3", false),
					},
				},
			},
			"volume-name-2",
			makeStatus("2", false),
		),
		Entry("no volume status found",
			&v1.VirtualMachineInstance{
				Status: v1.VirtualMachineInstanceStatus{
					VolumeStatus: []v1.VolumeStatus{
						*makeStatus("1", false),
						*makeStatus("2", false),
						*makeStatus("3", false),
					},
				},
			},
			"does-not-exist",
			nil,
		),
	)
})
