/*
 * Copyright (c) 2020-2022, NVIDIA CORPORATION.  All rights reserved.
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

package lm

import (
	"fmt"
	"log"

	"github.com/NVIDIA/gpu-feature-discovery/internal/mig"
	"github.com/NVIDIA/gpu-feature-discovery/internal/nvml"
	spec "github.com/NVIDIA/k8s-device-plugin/api/config/v1"
)

// Constants representing different MIG strategies.
const (
	MigStrategyNone   = "none"
	MigStrategySingle = "single"
	MigStrategyMixed  = "mixed"
)

// migResource is used to track MIG devices for labelling under the single and mixed strategies.
// This allows a particular resource name to be associated with an nvml.Device and count.
type migResource struct {
	name   spec.ResourceName
	device nvml.Device
	count  int
}

// NewResourceLabeler creates a labeler for available GPU resources.
// These include full GPU labels as well as labels specific to the mig-strategy specified.
func NewResourceLabeler(nvmlLib nvml.Nvml, config *spec.Config) (Labeler, error) {
	count, err := nvmlLib.GetDeviceCount()
	if err != nil {
		return nil, fmt.Errorf("error getting device count: %v", err)
	}
	// If no GPUs are detected, we return an empty labeler
	if count == 0 {
		return empty{}, nil
	}

	fullGPULabeler, err := newGPULabelers(nvmlLib, config)
	if err != nil {
		return nil, fmt.Errorf("failed to construct GPU labeler: %v", err)
	}

	if *config.Flags.MigStrategy == spec.MigStrategyNone {
		return fullGPULabeler, nil
	}

	migLabeler, err := newMigLabeler(nvmlLib, config)
	if err != nil {
		return nil, fmt.Errorf("failed to construct MIG resource labeler: %v", err)
	}

	labelers := Merge(
		fullGPULabeler,
		migLabeler,
	)

	return labelers, nil

}

// MigDeviceCounts maintains a count of unique MIG device types across all GPUs on a node
type MigDeviceCounts map[string]int

// newMigLabeler creates a labeler for MIG devices.
// The labeler created depends on the migStrategy.
func newMigLabeler(nvmlLib nvml.Nvml, config *spec.Config) (Labeler, error) {
	var err error
	var labeler Labeler
	switch *config.Flags.MigStrategy {
	case MigStrategyNone:
		labeler = empty{}
	case MigStrategySingle:
		labeler, err = newMigStrategySingleLabeler(nvmlLib, config)
		if err != nil {
			return nil, fmt.Errorf("failed to create labeler for mig-strategy=single: %v", err)
		}
	case MigStrategyMixed:
		labeler, err = newMigStrategyMixedLabeler(nvmlLib, config)
		if err != nil {
			return nil, fmt.Errorf("failed to create labeler for mig-strategy=mixed: %v", err)
		}
	default:
		return nil, fmt.Errorf("unknown strategy: %v", *config.Flags.MigStrategy)
	}

	labelers := Merge(
		migStrategyLabeler(*config.Flags.MigStrategy),
		labeler,
	)

	return labelers, nil
}

// newGPULabelers creates a set of labelers for full GPUs
func newGPULabelers(nvmlLib nvml.Nvml, config *spec.Config) (Labeler, error) {
	count, err := nvmlLib.GetDeviceCount()
	if err != nil {
		return nil, fmt.Errorf("error getting device count: %v", err)
	}

	if count == 0 {
		return nil, fmt.Errorf("no GPU devices detected")
	}

	var labelers list
	for i := uint(0); i < count; i++ {
		device, err := nvmlLib.NewDevice(i)
		if err != nil {
			return nil, fmt.Errorf("error getting device: %v", err)
		}
		l, err := NewGPUResourceLabeler(config, device, int(count))
		if err != nil {
			return nil, fmt.Errorf("failed to construct labeler: %v", err)
		}

		labelers = append(labelers, l)
		// TODO: We only process one device
		break
	}

	return labelers.Labels()
}

func newMigStrategySingleLabeler(nvmlLib nvml.Nvml, config *spec.Config) (Labeler, error) {
	deviceInfo := mig.NewDeviceInfo(nvmlLib)
	migEnabledDevices, err := deviceInfo.GetDevicesWithMigEnabled()
	if err != nil {
		return nil, fmt.Errorf("unabled to retrieve list of MIG-enabled devices: %v", err)
	}
	// No devices have migEnabled=true. This is equivalent to the `none` MIG strategy
	if len(migEnabledDevices) == 0 {
		return empty{}, nil
	}

	hasEmpty, err := deviceInfo.AnyMigEnabledDeviceIsEmpty()
	if err != nil {
		return nil, fmt.Errorf("failed to check for empty MIG-enabled devices: %v", err)
	}
	// If any migEnabled=true device is empty, we return the set of mig-strategy-invalid labels.
	if hasEmpty {
		return newInvalidMigStrategyLabeler(nvmlLib, "at least one MIG device is enabled but empty")
	}

	migDisabledDevices, err := deviceInfo.GetDevicesWithMigDisabled()
	if err != nil {
		return nil, fmt.Errorf("unabled to retrieve list of non-MIG-enabled devices: %v", err)
	}
	// If we have a mix of mig-enabled and mig-disabled device we return the set of mig-strategy-invalid labels
	if len(migDisabledDevices) != 0 {
		return newInvalidMigStrategyLabeler(nvmlLib, "devices with MIG enabled and disable detected")
	}

	migs, err := deviceInfo.GetAllMigDevices()
	if err != nil {
		return nil, fmt.Errorf("unable to retrieve list of MIG devices: %v", err)
	}

	// Add new MIG related labels on each individual MIG type
	fullGPUResourceName := spec.ResourceName("nvidia.com/gpu")
	resources := make(map[string]migResource)
	for _, mig := range migs {
		name, err := getMigDeviceName(mig)
		if err != nil {
			return nil, fmt.Errorf("unable to parse MIG device name: %v", err)
		}

		resource, exists := resources[name]
		// For the first ocurrence we update the device reference and the resource name
		if !exists {
			resource.device = mig
			resource.name = fullGPUResourceName
		}
		// We increase the count
		resource.count++

		resources[name] = resource
	}

	// Multiple resources mean that we have more than one MIG profile defined. Return the set of mig-strategy-invalid labels.
	if len(resources) != 1 {
		return newInvalidMigStrategyLabeler(nvmlLib, "more than one MIG device type present on node")
	}

	return newMIGDeviceLabelers(resources, config)
}

func newInvalidMigStrategyLabeler(nvml nvml.Nvml, reason string) (Labeler, error) {
	log.Printf("WARNING: Invalid configuration detected for mig-strategy=single: %v", reason)

	device, err := nvml.NewDevice(0)
	if err != nil {
		return nil, fmt.Errorf("error getting device: %v", err)
	}

	model, err := device.GetName()
	if err != nil {
		return nil, fmt.Errorf("failed to get device model: %v", err)
	}

	rl := resourceLabeler{
		resourceName: "nvidia.com/gpu",
	}

	labels := rl.productLabel(model, "MIG", "INVALID")

	rl.updateLabel(labels, "count", 0)
	rl.updateLabel(labels, "memory", 0)

	return labels, nil
}

// migStrategyMixed
func newMigStrategyMixedLabeler(nvmlLib nvml.Nvml, config *spec.Config) (Labeler, error) {
	deviceInfo := mig.NewDeviceInfo(nvmlLib)

	// Enumerate the MIG devices on this node. In mig.strategy=mixed we ignore devices
	// configured with migEnabled=true but exposing no MIG devices.
	migs, err := deviceInfo.GetAllMigDevices()
	if err != nil {
		return nil, fmt.Errorf("unable to retrieve list of MIG devices: %v", err)
	}

	// Add new MIG related labels on each individual MIG type
	resources := make(map[string]migResource)
	for _, mig := range migs {
		name, err := getMigDeviceName(mig)
		if err != nil {
			return nil, fmt.Errorf("unable to parse MIG device name: %v", err)
		}

		resource, exists := resources[name]
		// For the first ocurrence we update the device reference and the resource name
		if !exists {
			resource.device = mig
			resource.name = spec.ResourceName("nvidia.com/mig-" + name)
		}
		// We increase the count
		resource.count++

		resources[name] = resource
	}

	return newMIGDeviceLabelers(resources, config)
}

func newMIGDeviceLabelers(resources map[string]migResource, config *spec.Config) (Labeler, error) {
	var labelers list
	for _, resource := range resources {
		l, err := NewMIGResourceLabeler(resource.name, config, resource.device, resource.count)
		if err != nil {
			return nil, fmt.Errorf("failed to construct labeler: %v", err)
		}

		labelers = append(labelers, l)
	}

	return labelers, nil
}

// getMigDeviceName() returns the canonical name of the MIG device
func getMigDeviceName(mig nvml.Device) (string, error) {
	attr, err := mig.GetAttributes()
	if err != nil {
		return "", err
	}

	g := attr.GpuInstanceSliceCount
	gb := ((attr.MemorySizeMB + 1024 - 1) / 1024)
	r := fmt.Sprintf("%dg.%dgb", g, gb)

	return r, nil
}