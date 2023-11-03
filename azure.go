package main

import (
	"context"
	"fmt"
	"github.com/Azure/azure-sdk-for-go/services/compute/mgmt/2020-06-01/compute"
	"github.com/Azure/go-autorest/autorest"
	"github.com/Azure/go-autorest/autorest/azure/auth"
	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad-autoscaler/sdk/helper/ptr"
	"strings"
	"sync"
)

type AzureController struct {
	vmss    compute.VirtualMachineScaleSetsClient
	vmssVMs compute.VirtualMachineScaleSetVMsClient
}

func (ac *AzureController) init(config map[string]string) error {
	tenantID := argsOrEnv(config, configKeyTenantID, "ARM_TENANT_ID")
	clientID := argsOrEnv(config, configKeyClientID, "ARM_CLIENT_ID")
	subscriptionID := argsOrEnv(config, configKeySubscriptionID, "ARM_SUBSCRIPTION_ID")
	secretKey := argsOrEnv(config, configKeySecretKey, "ARM_CLIENT_SECRET")

	var authorizer autorest.Authorizer
	if tenantID != "" && clientID != "" && secretKey != "" {
		var err error
		authorizer, err = auth.NewClientCredentialsConfig(clientID, secretKey, tenantID).Authorizer()
		if err != nil {
			return fmt.Errorf("azure-vmss (ClientCredentials): %s", err)
		}
	} else {
		var err error
		authorizer, err = auth.NewAuthorizerFromEnvironment()
		if err != nil {
			return fmt.Errorf("azure-vmss (EnvironmentCredentials): %s", err)
		}
	}

	vmss := compute.NewVirtualMachineScaleSetsClient(subscriptionID)
	vmss.Sender = autorest.CreateSender()
	vmss.Authorizer = authorizer
	ac.vmss = vmss

	vmssVMs := compute.NewVirtualMachineScaleSetVMsClient(subscriptionID)
	vmssVMs.Sender = autorest.CreateSender()
	vmssVMs.Authorizer = authorizer
	ac.vmssVMs = vmssVMs

	return nil
}

func (ac *AzureController) getRemoteIds(ctx context.Context, resourceGroup string, vmScaleSet string, remoteIDs []string) ([]string, error) {
	pager, err := ac.vmssVMs.List(ctx, resourceGroup, vmScaleSet,
		"startswith(instanceView/statuses/code, 'PowerState') eq true",
		"instanceView/statuses", "instanceView")
	if err != nil {
		return nil, fmt.Errorf("failed to query VMSS instances: %v", err)
	}

	for pager.NotDone() {
		for _, vm := range pager.Values() {
			for _, s := range *vm.VirtualMachineScaleSetVMProperties.InstanceView.Statuses {
				if strings.HasPrefix(*s.Code, "PowerState/") {
					if *s.Code == "PowerState/running" {
						remoteIDs = append(remoteIDs, fmt.Sprintf("%s_%s", vmScaleSet, *vm.InstanceID))
					}
					break
				}
			}
		}

		err := pager.NextWithContext(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to list instances in VMSS: %v", err)
		}
	}

	return remoteIDs, nil
}

func (ac *AzureController) scaleOut(ctx context.Context, resourceGroup string, vmScaleSet string, count int64, wg *sync.WaitGroup, logger hclog.Logger) {
	defer wg.Done()
	if future, err := ac.vmss.Update(ctx, resourceGroup, vmScaleSet, compute.VirtualMachineScaleSetUpdate{
		Sku: &compute.Sku{
			Capacity: ptr.Int64ToPtr(count),
		},
	}); err != nil {
		logger.Error("failed to get the vmss update response: %v", err)
	} else {
		if err = future.WaitForCompletionRef(ctx, ac.vmss.Client); err != nil {
			logger.Error("cannot get the vmss update future response: %v", err)
		}
	}
}

func (ac *AzureController) scaleIn(ctx context.Context, resourceGroup string, vmScaleSet string, instanceIDs []string, wg *sync.WaitGroup, logger hclog.Logger) {
	defer wg.Done()
	if future, err := ac.vmss.DeleteInstances(ctx, resourceGroup, vmScaleSet, compute.VirtualMachineScaleSetVMInstanceRequiredIDs{
		InstanceIds: ptr.StringArrToPtr(instanceIDs),
	}); err != nil {
		logger.Error("failed to scale in Azure ScaleSet: %v", err)
	} else {
		if err = future.WaitForCompletionRef(ctx, ac.vmss.Client); err != nil {
			logger.Error("failed to scale in Azure ScaleSet: %v", err)
		}
	}
}
