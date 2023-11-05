package main

import (
	"context"
	"errors"
	"fmt"
	"github.com/Azure/azure-sdk-for-go/services/compute/mgmt/2020-06-01/compute"
	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad-autoscaler/plugins/base"
	"github.com/hashicorp/nomad-autoscaler/sdk"
	"github.com/hashicorp/nomad-autoscaler/sdk/helper/nomad"
	"github.com/hashicorp/nomad-autoscaler/sdk/helper/ptr"
	"github.com/hashicorp/nomad-autoscaler/sdk/helper/scaleutils"
	"github.com/hashicorp/nomad/api"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"
)

type TargetPlugin struct {
	logger          hclog.Logger
	AzureController *AzureController
	clusterUtils    *scaleutils.ClusterScaleUtils
}

func (t *TargetPlugin) SetConfig(config map[string]string) error {
	t.AzureController = &AzureController{}
	if err := t.AzureController.init(config); err != nil {
		return fmt.Errorf("cannot set config, %s", err.Error())
	}

	clusterUtils, err := scaleutils.NewClusterScaleUtils(nomad.ConfigFromNamespacedMap(config), t.logger)
	if err != nil {
		return err
	}

	t.clusterUtils = clusterUtils
	t.clusterUtils.ClusterNodeIDLookupFunc = azureNodeIDMap

	t.logger.Debug("config is set")
	return nil
}

func (t *TargetPlugin) PluginInfo() (*base.PluginInfo, error) {
	return &base.PluginInfo{
		Name:       pluginName,
		PluginType: sdk.PluginTypeTarget,
	}, nil
}

func (t *TargetPlugin) Scale(action sdk.ScalingAction, config map[string]string) error {
	if action.Count == sdk.StrategyActionMetaValueDryRunCount {
		return nil
	}

	resourceGroupListStr, ok := config[configKeyResourceGroupList]
	if !ok {
		return fmt.Errorf("required config param %s not found", configKeyResourceGroupList)
	}
	resourceGroupList := strings.Split(resourceGroupListStr, ",")

	vmScaleSetListStr, ok := config[configKeyVMSSList]
	if !ok {
		return fmt.Errorf("required config param %s not found", configKeyVMSSList)
	}
	vmScaleSetList := strings.Split(vmScaleSetListStr, ",")
	t.logger.Debug("scale triggered", configKeyResourceGroupList, resourceGroupList, configKeyVMSSList, vmScaleSetList)

	var totalVMSSCapacity int64
	for idx, vmScaleSet := range vmScaleSetList {
		ctx := context.Background()
		currVMSS, err := t.AzureController.vmss.Get(ctx, resourceGroupList[idx], vmScaleSet)
		if err != nil {
			return fmt.Errorf("failed to get Azure vmss: %v", err)
		}
		totalVMSSCapacity = totalVMSSCapacity + ptr.PtrToInt64(currVMSS.Sku.Capacity)
	}
	num, direction := calculateScaleDirection(totalVMSSCapacity, action.Count)
	modulo := num / int64(len(vmScaleSetList))
	reminder := num % int64(len(vmScaleSetList))
	t.logger.Debug("scale direction calculated", "modulo", modulo, "reminder", reminder)

	var wg sync.WaitGroup
	switch direction {
	case "out":
		log := t.logger.With("action", "scale_out")
		wg.Add(len(vmScaleSetList))
		for idx, vmScaleSet := range vmScaleSetList {
			count := modulo
			if reminder > 0 {
				count++
				reminder--
			}

			if count > 0 {
				log.Info("creating Azure ScaleSet instances", "vmss_name", vmScaleSet, "desired_count", count)
				ctx := context.Background()
				go t.AzureController.scaleOut(ctx, resourceGroupList[idx], vmScaleSet, count, &wg, log)
			} else {
				wg.Done()
				log.Debug("no new Azure ScaleSet instance needed", "vmss_name", vmScaleSet, "desired_count", count)
			}
		}
		wg.Wait()
		log.Info("successfully performed and verified scaling out")
	case "in":
		log := t.logger.With("action", "scale_in")
		wg.Add(len(vmScaleSetList))
		var err error
		var remoteIDs []string
		for idx, vmScaleSet := range vmScaleSetList {
			log.Debug("collection Azure ScaleSet instances IDs", "resource_group", resourceGroupList[idx], "vmss_name", vmScaleSet)
			ctx := context.Background()
			remoteIDs, err = t.AzureController.getRemoteIds(ctx, resourceGroupList[idx], vmScaleSet, remoteIDs)
			if err != nil {
				return fmt.Errorf("failed to egt remote ids in tasks: %v", err)
			}
		}

		log.Debug("running pre scale tasks", "IDs", remoteIDs)
		ids, err := t.clusterUtils.RunPreScaleInTasksWithRemoteCheck(context.Background(), config, remoteIDs, int(num))
		if err != nil {
			return fmt.Errorf("failed to perform pre-scale Nomad scale in tasks: %v", err)
		}

		instanceIDs := make(map[string][]string)
		for _, node := range ids {
			if idx := strings.LastIndex(node.RemoteResourceID, "_"); idx != -1 {
				for _, vmScaleSet := range vmScaleSetList {
					if strings.EqualFold(node.RemoteResourceID[0:idx], vmScaleSet) {
						instanceIDs[vmScaleSet] = append(instanceIDs[vmScaleSet], node.RemoteResourceID[idx+1:])
					}
				}
			} else {
				return errors.New("failed to get instance-id from remoteId")
			}
		}

		for idx, vmScaleSet := range vmScaleSetList {
			ctx := context.Background()
			if len(instanceIDs[vmScaleSet]) > 0 {
				log.Debug("deleting Azure ScaleSet instances", "instances", instanceIDs[vmScaleSet], "vmss_name", vmScaleSet)
				go t.AzureController.scaleIn(ctx, resourceGroupList[idx], vmScaleSet, instanceIDs[vmScaleSet], &wg, log)
			} else {
				wg.Done()
				log.Debug("no deletion Azure ScaleSet instance needed", "vmss_name", vmScaleSet)
			}
		}

		wg.Wait()
		log.Debug("running post scale tasks", "IDs", remoteIDs)
		if err = t.clusterUtils.RunPostScaleInTasks(context.Background(), config, ids); err != nil {
			return fmt.Errorf("failed to perform post-scale Nomad scale in tasks: %v", err)
		}
		log.Info("successfully deleted Azure ScaleSet instances")
	default:
		t.logger.Info("scaling not required", "current_count", num, "strategy_count", action.Count)
		return nil
	}

	return nil
}

func (t *TargetPlugin) Status(config map[string]string) (*sdk.TargetStatus, error) {
	ready, err := t.clusterUtils.IsPoolReady(config)
	if err != nil {
		return nil, fmt.Errorf("failed to run Nomad node readiness check: %v", err)
	}
	if !ready {
		return &sdk.TargetStatus{Ready: ready}, nil
	}

	resourceGroupListStr, ok := config[configKeyResourceGroupList]
	if !ok {
		return nil, fmt.Errorf("required config param %s not found", configKeyResourceGroupList)
	}
	resourceGroupList := strings.Split(resourceGroupListStr, ",")

	vmScaleSetListStr, ok := config[configKeyVMSSList]
	if !ok {
		return nil, fmt.Errorf("required config param %s not found", configKeyVMSSList)
	}
	vmScaleSetList := strings.Split(vmScaleSetListStr, ",")

	ready = true
	var totalCapacity int64
	latestTime := int64(math.MinInt64)
	for idx, vmScaleSet := range vmScaleSetList {
		ctx := context.Background()
		vmss, err := t.AzureController.vmss.Get(ctx, resourceGroupList[idx], vmScaleSet)
		if err != nil {
			return nil, fmt.Errorf("failed to get Azure ScaleSet: %v", err)
		}

		instanceView, err := t.AzureController.vmss.GetInstanceView(ctx, resourceGroupList[idx], vmScaleSet)
		if err != nil {
			return nil, fmt.Errorf("failed to get Azure ScaleSet Instance View: %v", err)
		}

		resp := sdk.TargetStatus{
			Ready: true,
			Count: ptr.PtrToInt64(vmss.Sku.Capacity),
			Meta:  make(map[string]string),
		}

		processInstanceView(instanceView, &resp)
		totalCapacity = totalCapacity + resp.Count
		if ready && !resp.Ready {
			ready = false
		}
		if len(resp.Meta[sdk.TargetStatusMetaKeyLastEvent]) > 0 {
			currentTime, _ := strconv.ParseInt(resp.Meta[sdk.TargetStatusMetaKeyLastEvent], 10, 64)
			if currentTime > latestTime {
				latestTime = currentTime
			}
		}
	}

	meta := make(map[string]string)
	meta[sdk.TargetStatusMetaKeyLastEvent] = strconv.FormatInt(latestTime, 10)
	resp := sdk.TargetStatus{
		Ready: ready,
		Count: totalCapacity,
		Meta:  meta,
	}
	return &resp, nil
}

func argsOrEnv(args map[string]string, key, env string) string {
	if value, ok := args[key]; ok {
		return value
	}
	return os.Getenv(env)
}

func azureNodeIDMap(n *api.Node) (string, error) {
	if val, ok := n.Attributes["unique.platform.azure.name"]; ok {
		return val, nil
	}

	// Fallback to meta tag.
	if val, ok := n.Meta["unique.platform.azure.name"]; ok {
		return val, nil
	}

	return "", fmt.Errorf("attribute %q not found", "unique.platform.azure.name")
}

func calculateScaleDirection(vmssDesired, strategyDesired int64) (int64, string) {

	if strategyDesired < vmssDesired {
		return vmssDesired - strategyDesired, "in"
	}
	if strategyDesired > vmssDesired {
		return strategyDesired, "out"
	}
	return 0, ""
}

func processInstanceView(instanceView compute.VirtualMachineScaleSetInstanceView, status *sdk.TargetStatus) {

	for _, instanceStatus := range *instanceView.VirtualMachine.StatusesSummary {
		if *instanceStatus.Code != "ProvisioningState/succeeded" {
			status.Ready = false
		}
	}

	latestTime := int64(math.MinInt64)
	for _, instanceStatus := range *instanceView.Statuses {
		if *instanceStatus.Code != "ProvisioningState/succeeded" {
			status.Ready = false
		}

		// Time isn't always populated, especially if the activity has not yet
		// finished :).
		if instanceStatus.Time != nil {
			currentTime := instanceStatus.Time.Time.UnixNano()
			if currentTime > latestTime {
				latestTime = currentTime
				status.Meta[sdk.TargetStatusMetaKeyLastEvent] = strconv.FormatInt(currentTime, 10)
			}
		}
	}
}
