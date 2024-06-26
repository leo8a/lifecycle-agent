/*
 * Copyright 2023 Red Hat, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this inputFilePath except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *   http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package precache

import (
	"context"
	"fmt"
	"os"

	k8serrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/go-logr/logr"
	ibuv1 "github.com/openshift-kni/lifecycle-agent/api/imagebasedupgrade/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/openshift-kni/lifecycle-agent/internal/common"

	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1 "k8s.io/api/core/v1"
)

// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;delete
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;delete

// PHandler handles the precaching job
type PHandler struct {
	client.Client
	Log    logr.Logger
	Scheme *runtime.Scheme
}

// Config defines the configuration options for a pre-caching job.
type Config struct {
	ImageList          []string
	NumConcurrentPulls int

	// To run pre-caching job with an adjusted niceness, which affects process scheduling.
	// Niceness values range from -20 (most favorable to the process) to 19 (least favorable to the process).
	NicePriority int

	// To configure the I/O-scheduling class and priority of a process.
	IoNiceClass    int // 0: none, 1: realtime, 2: best-effort, 3: idle
	IoNicePriority int // priority (0..7) in the specified scheduling class, only for the realtime and best-effort classes

	// Allow for environment variables to be passed in
	EnvVars []corev1.EnvVar
}

// NewConfig creates a new Config instance with the provided imageList and optional configuration parameters.
// It initializes the Config with default values and updates specific fields using key-value pairs in args.
// Supported configuration options in args:
//   - "NumConcurrentPulls" (int): Number of concurrent pulls for pre-caching.
//   - "NicePriority" (int): Nice priority for pre-caching.
//   - "IoNiceClass" (int): I/O nice class for pre-caching.
//   - "IoNicePriority" (int): I/O nice priority for pre-caching.
//
// Example usage:
//
//	config := NewConfig(imageList, "NumConcurrentPulls", 10, "NicePriority", 5)
func NewConfig(imageList []string, envVars []corev1.EnvVar, args ...any) *Config {
	instance := &Config{
		ImageList:          imageList,
		NumConcurrentPulls: DefaultMaxConcurrentPulls,
		NicePriority:       DefaultNicePriority,
		IoNiceClass:        DefaultIoNiceClass,
		IoNicePriority:     DefaultIoNicePriority,
		EnvVars:            envVars,
	}

	for i := 0; i < len(args); i += 2 {
		fieldName, value := args[i].(string), args[i+1]

		switch fieldName {
		case "NumConcurrentPulls":
			if NumConcurrentPulls, ok := value.(int); ok {
				instance.NumConcurrentPulls = NumConcurrentPulls
			}
		case "NicePriority":
			if NicePriority, ok := value.(int); ok {
				instance.NicePriority = NicePriority
			}
		case "IoNiceClass":
			if IoNiceClass, ok := value.(int); ok {
				instance.IoNiceClass = IoNiceClass
			}
		case "IoNicePriority":
			if IoNicePriority, ok := value.(int); ok {
				instance.IoNicePriority = IoNicePriority
			}
		}
	}

	return instance
}

// CreateJobAndConfigMap creates a new precache job.
func (h *PHandler) CreateJobAndConfigMap(ctx context.Context, config *Config, ibu *ibuv1.ImageBasedUpgrade) error {
	if len(config.ImageList) == 0 {
		return fmt.Errorf("no images specified for precaching")
	}

	// Generate ConfigMap for list of images to be pre-cached
	cm := renderConfigMap(config.ImageList)
	if err := h.Client.Create(ctx, cm); err != nil {
		if !k8serrors.IsAlreadyExists(err) {
			return fmt.Errorf("failed to create configMap for precache: %w", err)
		}
	}

	job, err := renderJob(config, h.Log, ibu, h.Scheme)
	if err != nil {
		return fmt.Errorf("failed to render precaching job manifest %w", err)
	}
	if err := h.Client.Create(ctx, job); err != nil {
		if !k8serrors.IsAlreadyExists(err) {
			return fmt.Errorf("failed to create precache job: %w", err)
		}
	}

	// Log job details
	h.Log.Info("Precaching", "CreatedJob", job.Name, "ConfigMap", cm.Name)
	return nil
}

// Cleanup deletes precaching resources
func (h *PHandler) Cleanup(ctx context.Context) error {
	// Delete precache job
	if err := deleteJob(ctx, h.Client); err != nil {
		h.Log.Info("Failed to delete precaching job", "name", LcaPrecacheResourceName)
		return err
	}
	// Delete precache ConfigMap
	if err := deleteConfigMap(ctx, h.Client); err != nil {
		h.Log.Info("Failed to delete precaching configmap", "name", LcaPrecacheResourceName)
		return err
	}

	// Delete precaching progress tracker file
	statusFile := common.PathOutsideChroot(StatusFile)
	if _, err := os.Stat(statusFile); err == nil {
		// Progress tracker file exists, attempt to delete it
		if err := os.Remove(statusFile); err != nil {
			h.Log.Error(err, "Failed to delete precaching progress tracker", "file", StatusFile)
		}
	}

	return nil
}
