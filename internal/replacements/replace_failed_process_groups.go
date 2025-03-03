/*
 * replace_failed_process_groups.go
 *
 * This source file is part of the FoundationDB open source project
 *
 * Copyright 2021 Apple Inc. and the FoundationDB project authors
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

package replacements

import (
	"fmt"
	"time"

	fdbv1beta2 "github.com/FoundationDB/fdb-kubernetes-operator/api/v1beta2"
	"github.com/go-logr/logr"
)

func getMaxReplacements(cluster *fdbv1beta2.FoundationDBCluster, maxReplacements int) int {
	// The maximum number of replacements will be the defined number in the cluster spec
	// minus all currently ongoing replacements e.g. process groups marked for removal but
	// not fully excluded.
	removalCount := 0
	for _, processGroupStatus := range cluster.Status.ProcessGroups {
		if processGroupStatus.IsMarkedForRemoval() && !processGroupStatus.IsExcluded() {
			// Count all removals that are in-flight.
			removalCount++
		}
	}

	return maxReplacements - removalCount
}

// ReplaceFailedProcessGroups flags failed processes groups for removal and returns an indicator
// of whether any processes were thus flagged.
func ReplaceFailedProcessGroups(log logr.Logger, cluster *fdbv1beta2.FoundationDBCluster, status *fdbv1beta2.FoundationDBStatus, hasDesiredFaultTolerance bool) bool {
	// Automatic replacements are disabled, so we don't have to check anything further
	if !cluster.GetEnableAutomaticReplacements() {
		return false
	}

	maxReplacements := getMaxReplacements(cluster, cluster.GetMaxConcurrentAutomaticReplacements())
	hasReplacement := false
	crashLoopContainerProcessGroups := cluster.GetCrashLoopContainerProcessGroups()

	for _, processGroupStatus := range cluster.Status.ProcessGroups {
		// If a process group is already marked for removal we can skip it here.
		if processGroupStatus.IsMarkedForRemoval() {
			continue
		}

		if processGroupStatus.IsUnderMaintenance(status.Cluster.MaintenanceZone) {
			log.Info(
				"Skip process group that is in maintenance zone",
				"processGroupID", processGroupStatus.ProcessGroupID,
				"maintenance zone", processGroupStatus.FaultDomain)
			continue
		}

		var shouldBeIgnored bool
		for _, targets := range crashLoopContainerProcessGroups {
			if _, ok := targets[processGroupStatus.ProcessGroupID]; ok {
				shouldBeIgnored = true
				break
			}

			if _, ok := targets["*"]; ok {
				shouldBeIgnored = true
				break
			}
		}

		if shouldBeIgnored {
			continue
		}

		failureCondition, failureTime := processGroupStatus.NeedsReplacement(cluster.GetFailureDetectionTimeSeconds(), cluster.GetTaintReplacementTimeSeconds())
		if failureTime == 0 {
			continue
		}

		skipExclusion := false
		if len(processGroupStatus.Addresses) == 0 {
			if !hasDesiredFaultTolerance {
				log.Info(
					"Skip process group with missing address",
					"processGroupID", processGroupStatus.ProcessGroupID,
					"failureTime", time.Unix(failureTime, 0).UTC().String())
				continue
			}

			// Since the process groups doesn't contain any addresses we have to skip exclusion.
			// The assumption here is that this is safe since we assume that the process group was never scheduled onto any node
			// otherwise the process group should have an address associated.
			skipExclusion = true
			log.Info(
				"Replace process group with missing address",
				"processGroupID", processGroupStatus.ProcessGroupID,
				"failureTime", time.Unix(failureTime, 0).UTC().String())
		}

		// We are not allowed to replace additional process groups.
		if maxReplacements <= 0 {
			log.Info("Detected replace process group but cannot replace it because we hit the replacement limit",
				"processGroupID", processGroupStatus.ProcessGroupID,
				"failureCondition", failureCondition,
				"faultDomain", processGroupStatus.FaultDomain,
				"reason", fmt.Sprintf("automatic replacement detected failure time: %s", time.Unix(failureTime, 0).UTC().String()))
			continue
		}

		log.Info("Replace process group",
			"processGroupID", processGroupStatus.ProcessGroupID,
			"failureCondition", failureCondition,
			"faultDomain", processGroupStatus.FaultDomain,
			"reason", fmt.Sprintf("automatic replacement detected failure time: %s", time.Unix(failureTime, 0).UTC().String()))

		processGroupStatus.MarkForRemoval()
		hasReplacement = true
		processGroupStatus.ExclusionSkipped = skipExclusion
		maxReplacements--
	}

	return hasReplacement
}
