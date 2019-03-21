// Copyright (c) Microsoft Corporation. All rights reserved.
// Licensed under the MIT license.

package kubernetesupgrade

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"path"
	"time"

	"github.com/Azure/aks-engine/pkg/api"
	"github.com/Azure/aks-engine/pkg/armhelpers"
	"github.com/Azure/aks-engine/pkg/engine"
	"github.com/Azure/aks-engine/pkg/engine/transform"
	"github.com/Azure/aks-engine/pkg/i18n"
	"github.com/Azure/aks-engine/pkg/operations"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

// Compiler to verify QueueMessageProcessor implements OperationsProcessor
var _ UpgradeNode = &UpgradeMasterNode{}

// UpgradeMasterNode upgrades a Kubernetes 1.5 master node to 1.6
type UpgradeMasterNode struct {
	Translator              *i18n.Translator
	logger                  *logrus.Entry
	TemplateMap             map[string]interface{}
	ParametersMap           map[string]interface{}
	UpgradeContainerService *api.ContainerService
	SubscriptionID          string
	ResourceGroup           string
	Client                  armhelpers.AKSEngineClient
	kubeConfig              string
	timeout                 time.Duration
}

// DeleteNode takes state/resources of the master/agent node from ListNodeResources
// backs up/preserves state as needed by a specific version of Kubernetes and then deletes
// the node.
// The 'drain' flag is not used for deleting master nodes.
func (kmn *UpgradeMasterNode) DeleteNode(vmName *string, drain bool) error {
	return operations.CleanDeleteVirtualMachine(kmn.Client, kmn.logger, kmn.SubscriptionID, kmn.ResourceGroup, *vmName)
}

// CreateNode creates a new master/agent node with the targeted version of Kubernetes
func (kmn *UpgradeMasterNode) CreateNode(ctx context.Context, poolName string, masterNo int) error {
	templateVariables := kmn.TemplateMap["variables"].(map[string]interface{})

	templateVariables["masterOffset"] = masterNo
	masterOffsetVar := templateVariables["masterOffset"]
	kmn.logger.Infof("Master offset: %v", masterOffsetVar)

	templateVariables["masterCount"] = masterNo + 1
	masterOffset := templateVariables["masterCount"]
	kmn.logger.Infof("Master pool set count to: %v temporarily during upgrade...", masterOffset)

	// NOTE: Keep this line commented out--it's only for debugging.
	// WriteTemplate(kmn.Translator, kmn.UpgradeContainerService, kmn.TemplateMap, kmn.ParametersMap)

	random := rand.New(rand.NewSource(time.Now().UnixNano()))
	deploymentSuffix := random.Int31()
	deploymentName := fmt.Sprintf("master-%s-%d", time.Now().Format("06-01-02T15.04.05"), deploymentSuffix)

	_, err := kmn.Client.DeployTemplate(
		ctx,
		kmn.ResourceGroup,
		deploymentName,
		kmn.TemplateMap,
		kmn.ParametersMap)
	return err
}

// Validate will verify the that master node has been upgraded as expected.
func (kmn *UpgradeMasterNode) Validate(vmName *string) error {
	if vmName == nil || *vmName == "" {
		kmn.logger.Warningf("VM name was empty. Skipping node condition check")
		return nil
	}

	if kmn.UpgradeContainerService.Properties.MasterProfile == nil {
		kmn.logger.Warningf("Master profile was empty. Skipping node condition check")
		return nil
	}

	masterURL := kmn.UpgradeContainerService.Properties.MasterProfile.FQDN

	client, err := kmn.Client.GetKubernetesClient(masterURL, kmn.kubeConfig, interval, kmn.timeout)
	if err != nil {
		return err
	}

	ch := make(chan struct{}, 1)
	go func() {
		for {
			masterNode, err := client.GetNode(*vmName)
			if err != nil {
				kmn.logger.Infof("Master VM: %s status error: %v", *vmName, err)
				time.Sleep(time.Second * 5)
			} else if isNodeReady(masterNode) {
				kmn.logger.Infof("Master VM: %s is ready", *vmName)
				ch <- struct{}{}
			} else {
				kmn.logger.Infof("Master VM: %s not ready yet...", *vmName)
				time.Sleep(time.Second * 5)
			}
		}
	}()

	for {
		select {
		case <-ch:
			return nil
		case <-time.After(kmn.timeout):
			kmn.logger.Errorf("Node was not ready within %v", kmn.timeout)
			return errors.Errorf("Node was not ready within %v", kmn.timeout)
		}
	}
}

// WriteTemplate writes the template and artifacts to an "Upgrade" folder under the output directory.
// This is used for debugging.
func WriteTemplate(
	translator *i18n.Translator,
	upgradeContainerService *api.ContainerService,
	templateMap map[string]interface{}, parametersMap map[string]interface{}) {
	updatedTemplateJSON, _ := json.Marshal(templateMap)
	parametersJSON, _ := json.Marshal(parametersMap)

	templateapp, err := transform.PrettyPrintArmTemplate(string(updatedTemplateJSON))
	if err != nil {
		logrus.Fatalf("error pretty printing template: %s \n", err.Error())
	}
	parametersapp, e := transform.PrettyPrintJSON(string(parametersJSON))
	if e != nil {
		logrus.Fatalf("error pretty printing template parameters: %s \n", e.Error())
	}
	outputDirectory := path.Join("_output", upgradeContainerService.Properties.MasterProfile.DNSPrefix, "Upgrade")
	writer := &engine.ArtifactWriter{
		Translator: translator,
	}
	if err := writer.WriteTLSArtifacts(upgradeContainerService, "vlabs", templateapp, parametersapp, outputDirectory, false, false); err != nil {
		logrus.Fatalf("error writing artifacts: %s\n", err.Error())
	}
}
