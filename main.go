package main

import (
	"fmt"
	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad-autoscaler/plugins"
	"github.com/hashicorp/nomad-autoscaler/plugins/base"
	"github.com/hashicorp/nomad-autoscaler/plugins/builtin/target/azure-vmss/plugin"
	"github.com/hashicorp/nomad-autoscaler/plugins/target"
	"github.com/hashicorp/nomad-autoscaler/sdk"
	"math"
	"strconv"
	"strings"
)

func main() {
	plugins.Serve(factory)
}

func factory(log hclog.Logger) interface{} {
	return NewAzureVMSSListPlugin(log)
}

const (
	pluginName = "azure-vmss-list"

	configKeyVMSSCount         = "vmss_count"
	configKeyResourceGroupList = "resource_group_list"
	configKeyVMSSList          = "vm_scale_set_list"

	configKeyResourceGroup = "resource_group"
	configKeyVMSS          = "vm_scale_set"
)

var (
	PluginConfig = &plugins.InternalPluginConfig{
		Factory: func(l hclog.Logger) interface{} { return NewAzureVMSSListPlugin(l) },
	}

	pluginInfo = &base.PluginInfo{
		Name:       pluginName,
		PluginType: sdk.PluginTypeTarget,
	}
)

// Assert that TargetPlugin meets the target.Target interface.
var _ target.Target = (*TargetPlugin)(nil)

type TargetPlugin struct {
	logger                 hclog.Logger
	azureVMSSTargetPlugins []*plugin.TargetPlugin
}

func NewAzureVMSSListPlugin(log hclog.Logger) *TargetPlugin {
	return &TargetPlugin{
		logger: log,
	}
}

func (t *TargetPlugin) SetConfig(config map[string]string) error {
	vmssCountStr, ok := config[configKeyVMSSCount]
	if !ok {
		return fmt.Errorf("required config param %s not found", configKeyVMSSCount)
	}
	vmssCount, err := strconv.Atoi(vmssCountStr)
	if err == nil {
		return fmt.Errorf("required config param %s is not a number", configKeyVMSSCount)
	}

	for i := 0; i < vmssCount; i++ {
		targetPlan := plugin.NewAzureVMSSPlugin(t.logger)
		if err := targetPlan.SetConfig(config); err != nil {
			return err
		}

		t.azureVMSSTargetPlugins = append(t.azureVMSSTargetPlugins, targetPlan)
	}

	return nil
}

func (t *TargetPlugin) PluginInfo() (*base.PluginInfo, error) {
	return pluginInfo, nil
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

	count := action.Count / int64(len(vmScaleSetList))
	i := action.Count % int64(len(vmScaleSetList))
	for idx, vmScaleSet := range vmScaleSetList {
		var j int64
		if i > 0 {
			j = 1
			i--
		}

		config[configKeyResourceGroup] = resourceGroupList[idx]
		config[configKeyVMSS] = vmScaleSet

		err := t.azureVMSSTargetPlugins[idx].Scale(sdk.ScalingAction{
			Count:     count + j,
			Reason:    action.Reason,
			Error:     action.Error,
			Direction: action.Direction,
			Meta:      action.Meta,
		}, config)
		if err != nil {
			return err
		}
	}

	return nil
}

// Status satisfies the Status function on the target.Target interface.
func (t *TargetPlugin) Status(config map[string]string) (*sdk.TargetStatus, error) {

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

	ready := true
	var totalCapacity int64
	latestTime := int64(math.MinInt64)
	meta := make(map[string]string)
	for idx, vmScaleSet := range vmScaleSetList {
		config[configKeyResourceGroup] = resourceGroupList[idx]
		config[configKeyVMSS] = vmScaleSet

		status, err := t.azureVMSSTargetPlugins[idx].Status(config)
		if err != nil {
			return nil, err
		}

		totalCapacity = totalCapacity + status.Count
		if ready && !status.Ready {
			ready = false
		}
		currentTime, _ := strconv.ParseInt(status.Meta[sdk.TargetStatusMetaKeyLastEvent], 10, 64)
		if currentTime > latestTime {
			latestTime = currentTime
		}
	}

	meta[sdk.TargetStatusMetaKeyLastEvent] = strconv.FormatInt(latestTime, 10)
	resp := sdk.TargetStatus{
		Ready: ready,
		Count: totalCapacity,
		Meta:  meta,
	}
	return &resp, nil
}
