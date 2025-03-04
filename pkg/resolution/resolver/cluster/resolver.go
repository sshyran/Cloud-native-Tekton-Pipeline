/*
Copyright 2022 The Tekton Authors

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

package cluster

import (
	"context"
	"errors"
	"fmt"
	"strings"

	resolverconfig "github.com/tektoncd/pipeline/pkg/apis/config/resolver"
	pipelinev1beta1 "github.com/tektoncd/pipeline/pkg/apis/pipeline/v1beta1"
	clientset "github.com/tektoncd/pipeline/pkg/client/clientset/versioned"
	pipelineclient "github.com/tektoncd/pipeline/pkg/client/injection/client"
	resolutioncommon "github.com/tektoncd/pipeline/pkg/resolution/common"
	"github.com/tektoncd/pipeline/pkg/resolution/resolver/framework"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"knative.dev/pkg/logging"
	"sigs.k8s.io/yaml"
)

const (
	disabledError = "cannot handle resolution request, enable-cluster-resolver feature flag not true"

	// LabelValueClusterResolverType is the value to use for the
	// resolution.tekton.dev/type label on resource requests
	LabelValueClusterResolverType string = "cluster"

	// ClusterResolverName is the name that the cluster resolver should be
	// associated with
	ClusterResolverName string = "Cluster"

	configMapName = "cluster-resolver-config"
)

var _ framework.Resolver = &Resolver{}

// Resolver implements a framework.Resolver that can fetch resources from other namespaces.
type Resolver struct {
	pipelineClientSet clientset.Interface
}

// Initialize performs any setup required by the cluster resolver.
func (r *Resolver) Initialize(ctx context.Context) error {
	r.pipelineClientSet = pipelineclient.Get(ctx)
	return nil
}

// GetName returns the string name that the cluster resolver should be
// associated with.
func (r *Resolver) GetName(_ context.Context) string {
	return ClusterResolverName
}

// GetSelector returns the labels that resource requests are required to have for
// the cluster resolver to process them.
func (r *Resolver) GetSelector(_ context.Context) map[string]string {
	return map[string]string{
		resolutioncommon.LabelKeyResolverType: LabelValueClusterResolverType,
	}
}

// ValidateParams returns an error if the given parameter map is not
// valid for a resource request targeting the cluster resolver.
func (r *Resolver) ValidateParams(ctx context.Context, params []pipelinev1beta1.Param) error {
	if r.isDisabled(ctx) {
		return errors.New(disabledError)
	}

	_, err := populateParamsWithDefaults(ctx, params)
	return err
}

// Resolve performs the work of fetching a resource from a namespace with the given
// parameters.
func (r *Resolver) Resolve(ctx context.Context, origParams []pipelinev1beta1.Param) (framework.ResolvedResource, error) {
	if r.isDisabled(ctx) {
		return nil, errors.New(disabledError)
	}

	logger := logging.FromContext(ctx)

	params, err := populateParamsWithDefaults(ctx, origParams)
	if err != nil {
		logger.Infof("cluster resolver parameter(s) invalid: %v", err)
		return nil, err
	}

	var data []byte

	switch params[KindParam] {
	case "task":
		task, err := r.pipelineClientSet.TektonV1beta1().Tasks(params[NamespaceParam]).Get(ctx, params[NameParam], metav1.GetOptions{})
		if err != nil {
			logger.Infof("failed to load task %s from namespace %s: %v", params[NameParam], params[NamespaceParam], err)
			return nil, err
		}
		task.Kind = "Task"
		task.APIVersion = "tekton.dev/v1beta1"
		data, err = yaml.Marshal(task)
		if err != nil {
			logger.Infof("failed to marshal task %s from namespace %s: %v", params[NameParam], params[NamespaceParam], err)
			return nil, err
		}
	case "pipeline":
		pipeline, err := r.pipelineClientSet.TektonV1beta1().Pipelines(params[NamespaceParam]).Get(ctx, params[NameParam], metav1.GetOptions{})
		if err != nil {
			logger.Infof("failed to load pipeline %s from namespace %s: %v", params[NameParam], params[NamespaceParam], err)
			return nil, err
		}
		pipeline.Kind = "Pipeline"
		pipeline.APIVersion = "tekton.dev/v1beta1"
		data, err = yaml.Marshal(pipeline)
		if err != nil {
			logger.Infof("failed to marshal pipeline %s from namespace %s: %v", params[NameParam], params[NamespaceParam], err)
			return nil, err
		}
	default:
		logger.Infof("unknown or invalid resource kind %s", params[KindParam])
		return nil, fmt.Errorf("unknown or invalid resource kind %s", params[KindParam])
	}

	return &ResolvedClusterResource{
		Content:   data,
		Name:      params[NameParam],
		Namespace: params[NamespaceParam],
	}, nil
}

var _ framework.ConfigWatcher = &Resolver{}

// GetConfigName returns the name of the cluster resolver's configmap.
func (r *Resolver) GetConfigName(context.Context) string {
	return configMapName
}

func (r *Resolver) isDisabled(ctx context.Context) bool {
	cfg := resolverconfig.FromContextOrDefaults(ctx)
	if cfg.FeatureFlags.EnableClusterResolver {
		return false
	}

	return true
}

// ResolvedClusterResource implements framework.ResolvedResource and returns
// the resolved file []byte data and an annotation map for any metadata.
type ResolvedClusterResource struct {
	Content   []byte
	Name      string
	Namespace string
}

var _ framework.ResolvedResource = &ResolvedClusterResource{}

// Data returns the bytes of the file resolved from git.
func (r *ResolvedClusterResource) Data() []byte {
	return r.Content
}

// Annotations returns the metadata that accompanies the resource fetched from the cluster.
func (r *ResolvedClusterResource) Annotations() map[string]string {
	return map[string]string{
		ResourceNameAnnotation:      r.Name,
		ResourceNamespaceAnnotation: r.Namespace,
	}
}

// Source is the source reference of the remote data that records where the remote
// file came from including the url, digest and the entrypoint.
func (r ResolvedClusterResource) Source() *pipelinev1beta1.ConfigSource {
	return nil
}

func populateParamsWithDefaults(ctx context.Context, origParams []pipelinev1beta1.Param) (map[string]string, error) {
	conf := framework.GetResolverConfigFromContext(ctx)

	paramsMap := make(map[string]pipelinev1beta1.ParamValue)
	for _, p := range origParams {
		paramsMap[p.Name] = p.Value
	}

	params := make(map[string]string)

	var missingParams []string

	if pKind, ok := paramsMap[KindParam]; !ok || pKind.StringVal == "" {
		if kindVal, ok := conf[DefaultKindKey]; !ok {
			missingParams = append(missingParams, KindParam)
		} else {
			params[KindParam] = kindVal
		}
	} else {
		params[KindParam] = pKind.StringVal
	}
	if kindVal, ok := params[KindParam]; ok && kindVal != "task" && kindVal != "pipeline" {
		return nil, fmt.Errorf("unknown or unsupported resource kind '%s'", kindVal)
	}

	if pName, ok := paramsMap[NameParam]; !ok || pName.StringVal == "" {
		missingParams = append(missingParams, NameParam)
	} else {
		params[NameParam] = pName.StringVal
	}

	if pNS, ok := paramsMap[NamespaceParam]; !ok || pNS.StringVal == "" {
		if nsVal, ok := conf[DefaultNamespaceKey]; !ok {
			missingParams = append(missingParams, NamespaceParam)
		} else {
			params[NamespaceParam] = nsVal
		}
	} else {
		params[NamespaceParam] = pNS.StringVal
	}

	if len(missingParams) > 0 {
		return nil, fmt.Errorf("missing required cluster resolver params: %s", strings.Join(missingParams, ", "))
	}

	if conf[BlockedNamespacesKey] != "" && isInCommaSeparatedList(params[NamespaceParam], conf[BlockedNamespacesKey]) {
		return nil, fmt.Errorf("access to specified namespace %s is blocked", params[NamespaceParam])
	}

	if conf[AllowedNamespacesKey] != "" && !isInCommaSeparatedList(params[NamespaceParam], conf[AllowedNamespacesKey]) {
		return nil, fmt.Errorf("access to specified namespace %s is not allowed", params[NamespaceParam])
	}

	return params, nil
}

func isInCommaSeparatedList(checkVal string, commaList string) bool {
	for _, s := range strings.Split(commaList, ",") {
		if s == checkVal {
			return true
		}
	}
	return false
}
