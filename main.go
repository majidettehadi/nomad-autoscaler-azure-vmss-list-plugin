package main

import (
	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad-autoscaler/plugins"
)

const (
	pluginName = "azure-vmss-list"

	configKeySubscriptionID = "subscription_id"
	configKeyTenantID       = "tenant_id"
	configKeyClientID       = "client_id"
	configKeySecretKey      = "secret_access_key"
	configKeyVMSSCount      = "vmss_count"

	configKeyResourceGroupList = "resource_group_list"
	configKeyVMSSList          = "vm_scale_set_list"
)

var (
	PluginConfig = &plugins.InternalPluginConfig{
		Factory: func(log hclog.Logger) interface{} {
			return &TargetPlugin{
				logger: log,
			}
		},
	}
)

func main() {
	plugins.Serve(factory)
}

func factory(log hclog.Logger) interface{} {
	return &TargetPlugin{
		logger: log,
	}
}
