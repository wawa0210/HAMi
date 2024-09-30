/*
Copyright 2024 The HAMi Authors.

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
package scheduler

import (
	"fmt"
	"sort"
	"strings"

	"github.com/Project-HAMi/HAMi/pkg/device"
	"github.com/Project-HAMi/HAMi/pkg/scheduler/config"
	"github.com/Project-HAMi/HAMi/pkg/scheduler/policy"
	"github.com/Project-HAMi/HAMi/pkg/util"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

func viewStatus(usage NodeUsage) {
	klog.Info("devices status")
	for _, val := range usage.Devices.DeviceLists {
		klog.InfoS("device status", "device id", val.Device.ID, "device detail", val)
	}
}

func checkType(annos map[string]string, d util.DeviceUsage, n util.ContainerDeviceRequest) (bool, bool) {
	//General type check, NVIDIA->NVIDIA MLU->MLU
	klog.Infoln("Type contains", d.Type, n.Type)
	if !strings.Contains(d.Type, n.Type) {
		return false, false
	}
	for idx, val := range device.GetDevices() {
		found, pass, numaAssert := val.CheckType(annos, d, n)
		klog.Infoln("idx", idx, found, pass)
		if found {
			return pass, numaAssert
		}
	}
	klog.Infof("Unrecognized device %s", n.Type)
	return false, false
}

func checkUUID(annos map[string]string, d util.DeviceUsage, n util.ContainerDeviceRequest) bool {
	devices, ok := device.GetDevices()[n.Type]
	if !ok {
		klog.Errorf("can not get device for %s type", n.Type)
		return false
	}
	result := devices.CheckUUID(annos, d)
	klog.V(2).Infof("checkUUID result is %v for %s type", result, n.Type)
	return result
}

func checkDevice(request *util.ContainerDeviceRequest, annos map[string]string, device util.DeviceUsage) (bool, string) {
	if !checkUUID(annos, device, *request) {
		return false, fmt.Sprintf("card uuid mismatch: current device: %v", device)
	}

	if device.Count <= device.Used {
		return false, fmt.Sprintf("device count %v is less than used count: %v", device.Count, device.Used)
	}

	memreq := getMemoryRequest(request, device)
	if device.Totalmem-device.Usedmem < memreq {
		return false, fmt.Sprintf("card insufficient remaining memory: device=%v, totalmem=%v, usedmem=%v, memreq=%v", device.ID, device.Totalmem, device.Usedmem, memreq)
	}

	if device.Totalcore-device.Usedcores < request.Coresreq {
		return false, fmt.Sprintf("card insufficient remaining cores: device=%v, totalcore=%v, usedcore=%v, coresreq=%v", device.ID, device.Totalcore, device.Usedcores, request.Coresreq)
	}

	// Coresreq=100 indicates it want this card exclusively
	if device.Totalcore == 100 && request.Coresreq == 100 && device.Used > 0 {
		return false, fmt.Sprintf("the container wants exclusive access to an entire card, but the card is already in use, device=%v, used=%v", device.ID, device.Used)
	}

	// You can't allocate core=0 job to an already full GPU
	if device.Totalcore != 0 && device.Usedcores == device.Totalcore && request.Coresreq == 0 {
		return false, fmt.Sprintf("can't allocate core=0 job to an already full GPU, device=%v, usedcores=%v, totalcore=%v", device.ID, device.Usedcores, device.Totalcore)
	}
	return true, ""
}

// getMemoryRequest returns the memory request for the container.
// if Memreq is greater than 0, return Memreq
// if MemPercentagereq is not 101 and Memreq is 0, return MemPercentagereq * device.Totalmem / 100
// otherwise, return 0.
func getMemoryRequest(request *util.ContainerDeviceRequest, device util.DeviceUsage) int32 {
	memreq := int32(0)

	if request.Memreq > 0 {
		memreq = request.Memreq
	}
	if request.MemPercentagereq != 101 && request.Memreq == 0 {

		memreq = device.Totalmem * request.MemPercentagereq / 100
	}
	return memreq
}

func fitInCertainDevice(node *NodeUsage, request util.ContainerDeviceRequest, annos map[string]string, pod *corev1.Pod) (bool, map[string]util.ContainerDevices) {
	k := request
	originReq := k.Nums
	prevnuma := -1
	klog.InfoS("Allocating device for container request", "pod", klog.KObj(pod), "card request", k)
	var tmpDevs map[string]util.ContainerDevices
	tmpDevs = make(map[string]util.ContainerDevices)
	for i := len(node.Devices.DeviceLists) - 1; i >= 0; i-- {
		klog.InfoS("scoring pod", "pod", klog.KObj(pod), "Memreq", k.Memreq, "MemPercentagereq", k.MemPercentagereq, "Coresreq", k.Coresreq, "Nums", k.Nums, "device index", i, "device", node.Devices.DeviceLists[i].Device.ID)
		found, numa := checkType(annos, *node.Devices.DeviceLists[i].Device, k)
		if !found {
			klog.InfoS("card type mismatch,continuing...", "pod", klog.KObj(pod), "device type", (node.Devices.DeviceLists[i].Device).Type, "request type", k.Type)
			continue
		}
		if numa && prevnuma != node.Devices.DeviceLists[i].Device.Numa {
			klog.InfoS("Numa not fit, resotoreing", "pod", klog.KObj(pod), "k.nums", k.Nums, "numa", numa, "prevnuma", prevnuma, "device numa", node.Devices.DeviceLists[i].Device.Numa)
			k.Nums = originReq
			prevnuma = node.Devices.DeviceLists[i].Device.Numa
			tmpDevs = make(map[string]util.ContainerDevices)
		}
		if request.Coresreq > 100 {
			klog.ErrorS(nil, "core limit can't exceed 100,will reset to 100", "pod", klog.KObj(pod))
			request.Coresreq = 100
		}

		if k.Nums > 0 {
			klog.InfoS("first fitted", "pod", klog.KObj(pod), "device", node.Devices.DeviceLists[i].Device.ID)
			k.Nums--
			tmpDevs[k.Type] = append(tmpDevs[k.Type], util.ContainerDevice{
				Idx:       int(node.Devices.DeviceLists[i].Device.Index),
				UUID:      node.Devices.DeviceLists[i].Device.ID,
				Type:      k.Type,
				Usedmem:   getMemoryRequest(&request, *node.Devices.DeviceLists[i].Device),
				Usedcores: k.Coresreq,
			})
		}
		if k.Nums == 0 {
			klog.InfoS("device allocate success", "pod", klog.KObj(pod), "allocate device", tmpDevs)
			return true, tmpDevs
		}
	}
	return false, tmpDevs
}

func fitInDevices(node *NodeUsage, requests util.ContainerDeviceRequests, annos map[string]string, pod *corev1.Pod, devinput *util.PodDevices) (bool, string) {
	//devmap := make(map[string]util.ContainerDevices)
	devs := util.ContainerDevices{}
	total, totalCore, totalMem := int32(0), int32(0), int32(0)
	free, freeCore, freeMem := int32(0), int32(0), int32(0)
	sums := 0
	// computer all device score for one node
	for index := range node.Devices.DeviceLists {
		node.Devices.DeviceLists[index].ComputeScore(requests)
	}
	//This loop is for requests for different devices
	for _, k := range requests {
		sums += int(k.Nums)
		if int(k.Nums) > len(node.Devices.DeviceLists) {
			exceedMessage := fmt.Sprintf("request devices exceed the total number devices on the node. pod: %s, request devices: %d, node devices: %d", klog.KObj(pod), k.Nums, len(node.Devices.DeviceLists))
			klog.InfoS(exceedMessage)
			return false, exceedMessage
		}
		sort.Sort(node.Devices)
		fit, tmpDevs := fitInCertainDevice(node, k, annos, pod)
		if fit {
			for _, val := range tmpDevs[k.Type] {
				total += node.Devices.DeviceLists[val.Idx].Device.Count
				totalCore += node.Devices.DeviceLists[val.Idx].Device.Totalcore
				totalMem += node.Devices.DeviceLists[val.Idx].Device.Totalmem
				free += node.Devices.DeviceLists[val.Idx].Device.Count - node.Devices.DeviceLists[val.Idx].Device.Used
				freeCore += node.Devices.DeviceLists[val.Idx].Device.Totalcore - node.Devices.DeviceLists[val.Idx].Device.Usedcores
				freeMem += node.Devices.DeviceLists[val.Idx].Device.Totalmem - node.Devices.DeviceLists[val.Idx].Device.Usedmem

				node.Devices.DeviceLists[val.Idx].Device.Used++
				node.Devices.DeviceLists[val.Idx].Device.Usedcores += val.Usedcores
				node.Devices.DeviceLists[val.Idx].Device.Usedmem += val.Usedmem
			}
			devs = append(devs, tmpDevs[k.Type]...)
		} else {
			return false, "device not fit"
		}
		(*devinput)[k.Type] = append((*devinput)[k.Type], devs)
	}
	return true, ""
}

func (s *Scheduler) calcScore(nodes *map[string]*NodeUsage, nums util.PodDeviceRequests, annos map[string]string, task *corev1.Pod) (*policy.NodeScoreList, error) {
	userNodePolicy := config.NodeSchedulerPolicy
	if annos != nil {
		if value, ok := annos[policy.NodeSchedulerPolicyAnnotationKey]; ok {
			userNodePolicy = value
		}
	}
	res := policy.NodeScoreList{
		Policy:   userNodePolicy,
		NodeList: make([]*policy.NodeScore, 0),
	}

	//func calcScore(nodes *map[string]*NodeUsage, errMap *map[string]string, nums util.PodDeviceRequests, annos map[string]string, task *corev1.Pod) (*NodeScoreList, error) {
	//	res := make(NodeScoreList, 0, len(*nodes))
	for nodeID, node := range *nodes {
		viewStatus(*node)
		score := policy.NodeScore{NodeID: nodeID, Devices: make(util.PodDevices), Score: 0}
		score.ComputeScore(node.Devices)

		//This loop is for different container request
		ctrfit := false
		ctrReason := ""
		for ctrid, n := range nums {
			sums := 0
			for _, k := range n {
				sums += int(k.Nums)
			}

			if sums == 0 {
				for idx := range score.Devices {
					if len(score.Devices[idx]) <= ctrid {
						score.Devices[idx] = append(score.Devices[idx], util.ContainerDevices{})
					}
					score.Devices[idx][ctrid] = append(score.Devices[idx][ctrid], util.ContainerDevice{})
					continue
				}
			}
			klog.V(5).InfoS("fitInDevices", "pod", klog.KObj(task), "node", nodeID)
			fit, reason := fitInDevices(node, n, annos, task, &score.Devices)
			ctrfit = fit
			ctrReason = reason
			if !fit {
				klog.InfoS("calcScore:node not fit pod", "pod", klog.KObj(task), "node", nodeID, "reason", reason)
				break
			}
		}
		score.Reason = ctrReason
		if ctrfit {
			res.NodeList = append(res.NodeList, &score)
		} else {
			res.UnelectedNodeList = append(res.UnelectedNodeList, &score)
		}
	}
	return &res, nil
}
