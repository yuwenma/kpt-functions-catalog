// Copyright 2022 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
package transformer

import (
	"fmt"
	"strings"

	"github.com/GoogleContainerTools/kpt-functions-sdk/go/fn"
	"k8s.io/apimachinery/pkg/util/sets"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Run provides the main workflow to update the ResourceList.Items "namespace" value.
func Run(rl *fn.ResourceList) (bool, error) {
	tc := SetNamespace{}
	// Get "namespace" arguments from FunctionConfig
	err := tc.Config(rl.FunctionConfig)
	if err != nil {
		rl.Results = append(rl.Results, fn.ErrorConfigObjectResult(err, rl.FunctionConfig))
		return true, nil
	}
	// Update "namespace" to the proper resources.
	results := tc.Transform(rl.Items)
	rl.Results = append(rl.Results, results...)
	return true, nil
}

// SetNamespace defines structs to parse KRM resource "SetNamespace" (the custom function config) and "ConfigMap" data.
// it provides the method "Config" to read the function configs from ResourceList.FunctionConfig
// it provides the method "Transform" to change the "namespace" and update the "config.kubernetes.io/depends-on" annotation.
type SetNamespace struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`
	NewNamespace      string            `json:"namespace,omitempty"`
	NamespaceMatcher  string            `json:"namespaceMatcher,omitempty"`
	Data              map[string]string `json:"data,omitempty"`
}

// Config gets the new namespace value different FunctionConfig type. It accepts three formats:
// 1. A ConfigMap object's .data.namespace
// 2. A ConfigMap named "kptfile.kpt.dev" object's .data.name
// 3. A SetNamespace object's .namespace
func (p *SetNamespace) Config(o *fn.KubeObject) error {
	switch {
	case o.IsEmpty():
		return fmt.Errorf("FunctionConfig is missing. Expect %T or %T", corev1.ConfigMap{}, SetNamespace{})
	case o.IsGVK("", "v1", "ConfigMap"):
		o.AsOrDie(&p)
		p.NamespaceMatcher = p.Data["namespaceMatcher"]
		if p.Data["namespace"] != "" {
			p.NewNamespace = p.Data["namespace"]
			return nil
		}
		if p.Data["name"] != "" && p.Name == "kptfile.kpt.dev" {
			p.NewNamespace = p.Data["name"]
			return nil
		}
		if p.Name == "kptfile.kpt.dev" {
			return fmt.Errorf("`data.name` should not be empty")
		}
		return fmt.Errorf("`data.namespace` should not be empty")
	case o.IsGVK(fnConfigGroup, fnConfigVersion, fnConfigKind):
		o.AsOrDie(&p)
		if p.NewNamespace == "" {
			return fmt.Errorf("`namespace` should not be empty")
		}
	default:
		return fmt.Errorf("unknown functionConfig Kind=%v ApiVersion=%v, expect `%v` or `ConfigMap`",
			o.GetKind(), o.GetAPIVersion(), fnConfigKind)
	}
	return nil
}

// Transform contains two workflows to replace the "namespace" fields
// 1. replace a matching namespace via "namespaceMatcher" config
// 2. replace all namespaces with origin constraints.
func (p *SetNamespace) Transform(objects fn.KubeObjects) fn.Results {
	// Skip local resource which `kpt live apply` skips.
	objects = objects.WhereNot(func(o *fn.KubeObject) bool { return o.IsLocalConfig() })

	// Store resources' GKNN before the namespace change. This map will be used to determine whether a resource which other
	// resources depends on has its namespace changes.
	dependsOnMap := MapGKNNBeforeChange(objects)

	// Only replace matching namespace. This allows the resourcelist.items to have more than one origin namespace value.
	if p.NamespaceMatcher != "" {
		return ReplaceNamespace(objects, p.NewNamespace, dependsOnMap, p.NamespaceMatcher)
	}

	// Replace all namespaces. This requires the resource origin namespace to be the same.
	warnResults, err := ValidateOrigin(objects)
	if err != nil {
		return []*fn.Result{fn.ErrorResult(err)}
	}
	var results fn.Results
	if warnResults != nil {
		results = append(results, warnResults...)
	}
	results = append(results, ReplaceNamespace(objects, p.NewNamespace, dependsOnMap)...)
	return results
}

// ReplaceNamespace provides the actual workflow to replace the namespace, update depends-on anntations and
// add the result messages.
func ReplaceNamespace(objects fn.KubeObjects, newNs string, dependsOnMap map[string]struct{}, nsMatcher ...string) fn.Results {
	var results fn.Results
	count, oldNss := WalkAndReplace(objects, newNs, nsMatcher...)
	results = AddSummaryResult(results, count, newNs, oldNss...)

	// Update the depends-on annotation.
	dependsOnCount := UpdateAnnotation(objects, dependsOnMap, newNs, nsMatcher...)
	results = AddAnnotationResult(results, dependsOnCount, newNs)
	return results
}

// ValidateOrigin adds the constraints for general replacement. It requires all namespace-scoped resource to have the same
// origin namespace.
// If a resource does not have upstream origin, it gives warnings (the resource will still be updated).
func ValidateOrigin(objects fn.KubeObjects) (fn.Results, error) {
	var results fn.Results
	originNss := sets.NewString()
	namespaceScoped := objects.Where(func(o *fn.KubeObject) bool { return o.IsNamespaceScoped() })
	for _, o := range namespaceScoped {
		if o.HasUpstreamOrigin() {
			origin := o.GetOriginId()
			// This should rarely happen.
			if origin.Namespace == fn.UnkownNamespace {
				return nil, fmt.Errorf("%v is namespace-scoped, but has cluster-scoped or unknown scoepd origin %v",
					o.ShortString(), origin.String())
			}
			originNss.Insert(origin.Namespace)
		} else {
			results = append(results, fn.GeneralResult(fmt.Sprintf(
				"%v does not have upstream origin.", o.ShortString()), fn.Warning))
		}
	}
	if len(originNss) > 1 {
		return nil, fmt.Errorf(
			"unable to use origin `namespace` to match. expect a single value, found %v. please use `namespaceMatcher`"+
				"to specify the namespace value you want to change",
			originNss.List())
	}
	return results, nil
}

// WalkAndReplace iterate each KRM resource and updates the "namespace" fields.
func WalkAndReplace(objects fn.KubeObjects, newNs string, matchers ...string) (int, []string) {
	count := 0
	oldnss := sets.NewString()
	VisitAll(objects, func(origin string, currentPtr *string) {
		// Skip if the resource is a cluster scoped or unknown scoped resource.
		if origin == fn.UnkownNamespace {
			return
		}
		if *currentPtr == "" || *currentPtr == newNs {
			return
		}
		// matcher not given, update all.
		change := false
		if len(matchers) == 0 {
			change = true
		} else {
			for i := range matchers {
				if matchers[i] == *currentPtr {
					change = true
				}
			}
		}
		if change {
			oldnss.Insert(*currentPtr)
			*currentPtr = newNs
			count += 1
		}
	})
	return count, oldnss.List()
}

// VisitAll applies "visitor" function to both namespace scoped and cluster scoped resource.
func VisitAll(objects fn.KubeObjects, visitor func(origin string, currentPtr *string)) {
	VisitSpecialClusterResource(objects, visitor)
	VisitNamespaceResource(objects, visitor)
}

// VisitSpecialClusterResource applies "visitor" function to some special cluster-scoped resource that
// have sub fields meaning "namespace".
func VisitSpecialClusterResource(objects fn.KubeObjects, visitor func(origin string, currentPtr *string)) {
	clusterScoped := objects.Where(func(o *fn.KubeObject) bool { return o.IsClusterScoped() })
	for _, o := range clusterScoped {
		switch {
		case o.IsGVK("", "v1", "Namespace"):
			name := o.GetName()
			nsPtr := &name
			visitor(o.GetOriginId().Name, nsPtr)
			o.SetName(*nsPtr)
		case o.IsGVK("apiextensions.k8s.io", "v1", "CustomResourceDefinition"):
			namespace := o.NestedStringOrDie("spec", "conversion", "webhook", "clientConfig", "service", "namespace")
			nsPtr := &namespace
			visitor("", nsPtr)
			o.SetNestedStringOrDie(*nsPtr, "spec", "conversion", "webhook", "clientConfig", "service", "namespace")
		case o.IsGVK("apiregistration.k8s.io", "v1", "APIService"):
			namespace := o.NestedStringOrDie("spec", "service", "namespace")
			nsPtr := &namespace
			visitor("", nsPtr)
			o.SetNestedStringOrDie(*nsPtr, "spec", "service", "namespace")
		case o.GetKind() == "ClusterRoleBinding" || o.GetKind() == "RoleBinding":
			subjects := o.GetSlice("subjects")
			for _, s := range subjects {
				namespace := s.NestedStringOrDie("namespace")
				nsPtr := &namespace
				visitor("", nsPtr)
				s.SetNestedStringOrDie(*nsPtr, "namespace")
			}
		default:
			// skip the cluster scoped resource
		}
	}
}

// VisitNamespaceResource applies "visitor" to namespace-scoped resource.
// We made a hypothesis here that if a unknown scoped resource has a non-empty metadata.namespace, the resource will be
// treated as namespace scoped.
func VisitNamespaceResource(objects fn.KubeObjects, visitor func(origin string, currentPtr *string)) {
	namespaceScoped := objects.Where(func(o *fn.KubeObject) bool { return o.IsNamespaceScoped() })
	for _, o := range namespaceScoped {
		namespace := o.GetNamespace()
		nsPtr := &namespace
		visitor(o.GetOriginId().Namespace, nsPtr)
		o.SetNamespace(*nsPtr)
	}
}

// MapGKNNBeforeChange stores each namespace-scoped resource's Group, Kind, Namespace and Name.
// This map will be used later to align the depends-on annotation.
func MapGKNNBeforeChange(objects fn.KubeObjects) map[string]struct{} {
	dependsOnMap := map[string]struct{}{}
	for _, o := range objects {
		id := o.GetId()
		if id.Namespace == fn.UnkownNamespace {
			continue
		}
		key := fmt.Sprintf("%v/namespaces/%v/%v/%v", id.Group, id.Namespace, id.Kind, id.Name)
		dependsOnMap[key] = struct{}{}
	}
	return dependsOnMap
}

// hasDependsOnAnnotation checks whether a resource has namespace-scoped depends-on annotation.
func hasDependsOnAnnotation(o *fn.KubeObject) bool {
	return o.GetAnnotations()[dependsOnAnnotation] != "" && !namespacedResourcePattern.MatchString(
		o.GetAnnotations()[dependsOnAnnotation])
}

// UpdateAnnotation updates the depends-on annotations whose referred resources are updated.
func UpdateAnnotation(objects fn.KubeObjects, dependsOnMap map[string]struct{}, newNs string, matchers ...string) int {
	count := 0
	for _, o := range objects.Where(hasDependsOnAnnotation) {
		segments := strings.Split(o.GetAnnotations()[dependsOnAnnotation], "/")
		dependsOnkey := dependsOnKeyPattern(segments[groupIdx], segments[kindIdx], segments[nameIdx])
		if _, ok := dependsOnMap[dependsOnkey]; ok {
			if segments[namespaceIdx] == newNs {
				continue
			}
			change := false
			if len(matchers) == 0 {
				change = true
			} else {
				for i := range matchers {
					if matchers[i] == segments[namespaceIdx] {
						change = true
					}
				}
			}
			if change {
				segments[namespaceIdx] = newNs
				count += 1
				newAnnotation := strings.Join(segments, "/")
				o.SetAnnotation(dependsOnAnnotation, newAnnotation)
			}
		}
	}
	return count
}

// AddSummaryResult provides a user friendly message to summarize the namespace change.
func AddSummaryResult(results fn.Results, count int, newNs string, oldNss ...string) fn.Results {
	if count == 0 {
		return append(results, fn.GeneralResult(
			fmt.Sprintf("all namespaces are already %q. no value changed", newNs), fn.Info))
	}
	return append(results, fn.GeneralResult(fmt.Sprintf("namespace %v updated to %q, %d value(s) changed",
		oldNss, newNs, count), fn.Info))
}

// AddAnnotationResult provides a user friendly message to summarize the depends-on annotation change.
func AddAnnotationResult(results fn.Results, count int, newNs string) fn.Results {
	if count == 0 {
		return append(results, fn.GeneralResult(
			fmt.Sprintf("all `depends-on` annotations are up-to-date. no `namespace` changed"), fn.Info))
	}
	return append(results, fn.GeneralResult(fmt.Sprintf("`depends-on` annotations' namespace updated to %q, %d value(s) changed",
		newNs, count), fn.Info))
}
