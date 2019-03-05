// Copyright 2015 Google Inc. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package processors

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/kubernetes-incubator/metrics-server/metrics/core"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	v1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
)

var batches = []*core.DataBatch{
	{
		Timestamp: time.Now(),
		MetricSets: map[string]*core.MetricSet{
			core.PodContainerKey("ns1", "pod1", "c1"): {
				Labels: map[string]string{
					core.LabelMetricSetType.Key: core.MetricSetTypePodContainer,
					core.LabelPodName.Key:       "pod1",
					core.LabelNamespaceName.Key: "ns1",
					core.LabelContainerName.Key: "c1",
				},
				MetricValues: map[string]core.MetricValue{},
			},

			core.PodKey("ns1", "pod1"): {
				Labels: map[string]string{
					core.LabelMetricSetType.Key: core.MetricSetTypePod,
					core.LabelPodName.Key:       "pod1",
					core.LabelNamespaceName.Key: "ns1",
				},
				MetricValues: map[string]core.MetricValue{},
			},
		},
	},
	{
		Timestamp: time.Now(),
		MetricSets: map[string]*core.MetricSet{
			core.PodContainerKey("ns1", "pod1", "c1"): {
				Labels: map[string]string{
					core.LabelMetricSetType.Key: core.MetricSetTypePodContainer,
					core.LabelPodName.Key:       "pod1",
					core.LabelNamespaceName.Key: "ns1",
					core.LabelContainerName.Key: "c1",
				},
				MetricValues: map[string]core.MetricValue{},
			},
		},
	},
	{
		Timestamp: time.Now(),
		MetricSets: map[string]*core.MetricSet{
			core.PodKey("ns1", "pod1"): {
				Labels: map[string]string{
					core.LabelMetricSetType.Key: core.MetricSetTypePod,
					core.LabelPodName.Key:       "pod1",
					core.LabelNamespaceName.Key: "ns1",
				},
				MetricValues: map[string]core.MetricValue{},
			},
		},
	},
}

func TestPodEnricher(t *testing.T) {
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pod1",
			Namespace: "ns1",
		},
		Spec: corev1.PodSpec{
			NodeName: "node1",
			Containers: []corev1.Container{
				{
					Name:  "c1",
					Image: "gcr.io/google_containers/pause:2.0",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    *resource.NewMilliQuantity(100, resource.DecimalSI),
							corev1.ResourceMemory: *resource.NewQuantity(555, resource.DecimalSI),
						},
					},
				},
				{
					Name:  "nginx",
					Image: "gcr.io/google_containers/pause:2.0",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    *resource.NewMilliQuantity(333, resource.DecimalSI),
							corev1.ResourceMemory: *resource.NewQuantity(1000, resource.DecimalSI),
						},
						Limits: corev1.ResourceList{
							corev1.ResourceCPU:    *resource.NewMilliQuantity(2222, resource.DecimalSI),
							corev1.ResourceMemory: *resource.NewQuantity(3333, resource.DecimalSI),
						},
					},
				},
			},
		},
	}

	store := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
	podLister := v1listers.NewPodLister(store)
	store.Add(&pod)
	podBasedEnricher := PodBasedEnricher{podLister: podLister}

	var err error
	for _, batch := range batches {
		batch, err = podBasedEnricher.Process(batch)
		assert.NoError(t, err)

		podAggregator := PodAggregator{}
		batch, err = podAggregator.Process(batch)
		assert.NoError(t, err)

		podMs, found := batch.MetricSets[core.PodKey("ns1", "pod1")]
		assert.True(t, found)
		checkRequests(t, podMs, 433, 1555)
		checkLimits(t, podMs, 2222, 3333)

		containerMs, found := batch.MetricSets[core.PodContainerKey("ns1", "pod1", "c1")]
		assert.True(t, found)
		checkRequests(t, containerMs, 100, 555)
		checkLimits(t, containerMs, 0, 0)
	}
}

func checkRequests(t *testing.T, ms *core.MetricSet, cpu, mem int64) {
	cpuVal, found := ms.MetricValues[core.MetricCpuRequest.Name]
	assert.True(t, found)
	assert.Equal(t, cpu, cpuVal.IntValue)

	memVal, found := ms.MetricValues[core.MetricMemoryRequest.Name]
	assert.True(t, found)
	assert.Equal(t, mem, memVal.IntValue)
}

func checkLimits(t *testing.T, ms *core.MetricSet, cpu, mem int64) {
	cpuVal, found := ms.MetricValues[core.MetricCpuLimit.Name]
	assert.True(t, found)
	assert.Equal(t, cpu, cpuVal.IntValue)

	memVal, found := ms.MetricValues[core.MetricMemoryLimit.Name]
	assert.True(t, found)
	assert.Equal(t, mem, memVal.IntValue)
}
