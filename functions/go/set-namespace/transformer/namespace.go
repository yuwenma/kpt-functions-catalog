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
	"reflect"
	"strings"

	"github.com/GoogleContainerTools/kpt-functions-sdk/go/fn"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
)

func SetNamespace(rl *fn.ResourceList) (bool, error) {
	tc := NamespaceTransformer{}
	// Get "namespace" arguments from FunctionConfig
	if ok := tc.Config(rl); !ok {
		return true, nil
	}
	// Transform resources to replace their namespace or namespace related field to the new value.
	oldNamespaces, count := tc.Transform(rl.Items)
	// Write user messages in ResourceList.
	tc.LogResults(rl, count, oldNamespaces)
	return true, nil
}

// LogResults writes user message to ResourceList.Results
func (p *NamespaceTransformer) LogResults(rl *fn.ResourceList, count int, oldNamespaces map[string]struct{}) {
	result := func() *fn.Result {
		if len(p.Errors) != 0 {
			errMsg := strings.Join(p.Errors, "\n")
			return fn.GeneralResult(errMsg, fn.Error)
		}
		if dupErr := fn.CheckResourceDuplication(rl); dupErr != nil {
			return fn.ErrorResult(dupErr)
		}
		if count == 0 {
			msg := fmt.Sprintf("all namespaces are already %q. no value changed", p.NewNamespace)
			return fn.GeneralResult(msg, fn.Info)
		}

		oldNss := []string{}
		for oldNs := range oldNamespaces {
			oldNss = append(oldNss, `"`+oldNs+`"`)
		}
		msg := fmt.Sprintf("namespace %v updated to %q, %d value(s) changed", strings.Join(oldNss, ","), p.NewNamespace, count)
		result := fn.GeneralResult(msg, fn.Info)
		return result
	}()
	rl.Results = append(rl.Results, result)
}

// Config gets the attributes from different FunctionConfig formats.
func (p *NamespaceTransformer) Config(rl *fn.ResourceList) bool {
	err := func(o *fn.KubeObject) error {
		switch {
		case o.IsGVK("", "v1", "ConfigMap"):
			var cm corev1.ConfigMap
			o.As(&cm)
			if cm.Data["namespace"] != "" {
				p.NewNamespace = cm.Data["namespace"]
			} else if cm.Name == "kptfile.kpt.dev" {
				p.NewNamespace = cm.Data["name"]
				if cm.Data["name"] == "" {
					return fmt.Errorf("`data.name` should not be empty")
				}
			} else {
				return fmt.Errorf("`data.namespace` should not be empty")
			}
			p.NamespaceMatcher = cm.Data["namespaceMatcher"]
		default:
			return fmt.Errorf("unknown functionConfig Kind=%v ApiVersion=%v, expect `%v` or `ConfigMap`",
				o.GetKind(), o.GetAPIVersion(), fnConfigKind)
		}
		return nil
	}(rl.FunctionConfig)
	if err != nil {
		rl.Results = append(rl.Results, fn.ErrorConfigObjectResult(err, rl.FunctionConfig))
	}
	return err == nil
}

// Transform replace the existing Namespace object, namespace-scoped resources' `metadata.name` and
// other namespace reference fields to the new value.
func (p *NamespaceTransformer) Transform(objects []*fn.KubeObject) (map[string]struct{}, int) {
	var activeOldNss map[string]struct{}
	count := new(int)
	namespaces, nsObjCounter, originNamespaces := FindAllNamespaces(objects)
	if oldNamespaces, ok := p.GetOldNamespaces(namespaces, nsObjCounter, originNamespaces); ok {
		ReplaceNamespace(objects, oldNamespaces, p.NewNamespace, activeOldNss, count)
		// Update the resources annotation "config.kubernetes.io/depends-on" which may contain old namespace value.
		dependsOnMap := GetDependsOnMap(objects)
		UpdateAnnotation(objects, oldNamespaces, p.NewNamespace, dependsOnMap, activeOldNss)
	}
	return activeOldNss, *count
}

// VisitNamespaces iterates the `objects` to execute the `visitor` function on each corresponding namespace field.
func VisitNamespaces(objects []*fn.KubeObject, visitor func(namespace *Namespace)) {
	for _, o := range objects {
		switch {
		// Skip local resource which `kpt live apply` skips.
		case o.IsLocalConfig():
			continue
		case o.IsGVK("", "v1", "Namespace"):
			namespace := o.GetName()
			var originIDs []string
			for _, originResId := range fn.GetOriginResIds(o) {
				originIDs = append(originIDs, originResId.Name)
			}
			visitor(NewNamespace(o, &namespace, originIDs))
			o.SetName(namespace)
		case o.IsGVK("apiextensions.k8s.io", "v1", "CustomResourceDefinition"):
			namespace := o.NestedStringOrDie("spec", "conversion", "webhook", "clientConfig", "service", "namespace")
			visitor(NewNamespace(o, &namespace, nil))
			o.SetNestedStringOrDie(namespace, "spec", "conversion", "webhook", "clientConfig", "service", "namespace")
		case o.IsGVK("apiregistration.k8s.io", "v1", "APIService"):
			namespace := o.NestedStringOrDie("spec", "service", "namespace")
			visitor(NewNamespace(o, &namespace, nil))
			o.SetNestedStringOrDie(namespace, "spec", "service", "namespace")
		case o.GetKind() == "ClusterRoleBinding" || o.GetKind() == "RoleBinding":
			subjects := o.GetSlice("subjects")
			for _, s := range subjects {
				var ns string
				found, _ := s.Get(&ns, "namespace")
				if found {
					visitor(NewNamespace(o, &ns, nil))
					s.SetNestedStringOrDie(ns, "namespace")
				}
			}
			o.SetNestedField(&subjects, "subjects")
		default:
			// skip the cluster scoped resource. We determine if a resource is cluster scoped by
			// checking if the metadata.namespace is configured.
		}
		if o.HasNamespace() {
			// We made the hypothesis that a namespace scoped resource should have its metadata.namespace field setup.
			// All above special types are cluster scoped, except RoleBinding
			namespace := o.GetNamespace()

			var originIDs []string
			for _, originResId := range fn.GetOriginResIds(o) {
				originIDs = append(originIDs, originResId.Namespace)
			}
			visitor(NewNamespace(o, &namespace, originIDs))
			o.SetNamespace(namespace)
		}
	}
}

// FindAllNamespaces iterates the `objects` to list all namespaces and count the number of Namespace objects.
func FindAllNamespaces(objects []*fn.KubeObject) ([]string, map[string]int, []string) {
	nsObjCounter := map[string]int{}
	namespaces := sets.NewString()
	originNamespaces := sets.NewString()
	VisitNamespaces(objects, func(ns *Namespace) {
		if *ns.Ptr == "" {
			return
		}
		if ns.IsNamespace {
			nsObjCounter[*ns.Ptr] += 1
		}
		namespaces.Insert(*ns.Ptr)
		originNamespaces.Insert(ns.OriginNamespaces...)
	})
	return namespaces.List(), nsObjCounter, originNamespaces.List()
}

// ReplaceNamespace iterates the `objects` to replace the `OldNs` with `newNs` on namespace field.
func ReplaceNamespace(objects []*fn.KubeObject, oldNamespaces []string, newNs string, activeOldNss map[string]struct{}, count *int) {
	VisitNamespaces(objects, func(ns *Namespace) {
		if *ns.Ptr == "" {
			return
		}
		for i := range oldNamespaces {
			if *ns.Ptr == oldNamespaces[i] && newNs != oldNamespaces[i] {
				*ns.Ptr = newNs
				*count += 1
				activeOldNss[oldNamespaces[i]] = struct{}{}
			}
		}
	})
}

// GetDependsOnMap iterates `objects` to get the annotation which contains namespace value.
func GetDependsOnMap(objects []*fn.KubeObject) map[string]bool {
	dependsOnMap := map[string]bool{}
	VisitNamespaces(objects, func(ns *Namespace) {
		key := ns.GetDependsOnAnnotation()
		dependsOnMap[key] = true
	})
	return dependsOnMap
}

// UpdateAnnotation updates the `objects`'s "config.kubernetes.io/depends-on" annotation which contains namespace value.
func UpdateAnnotation(objects []*fn.KubeObject, oldNamespaces []string, newNs string, dependsOnMap map[string]bool, activeOldNss map[string]struct{}) {
	VisitNamespaces(objects, func(ns *Namespace) {
		if ns.DependsOnAnnotation == "" || !namespacedResourcePattern.MatchString(ns.DependsOnAnnotation) {
			return
		}
		segments := strings.Split(ns.DependsOnAnnotation, "/")
		dependsOnkey := dependsOnKeyPattern(segments[groupIdx], segments[kindIdx], segments[nameIdx])
		if ok := dependsOnMap[dependsOnkey]; ok {
			for i := range oldNamespaces {
				if segments[namespaceIdx] == oldNamespaces[i] && segments[namespaceIdx] != newNs {
					segments[namespaceIdx] = newNs
					newAnnotation := strings.Join(segments, "/")
					ns.SetDependsOnAnnotation(newAnnotation)
					activeOldNss[oldNamespaces[i]] = struct{}{}
				}
			}
		}
	})
}

// GetOldNamespace finds the existing namespace and make sure the input resourceList.items satisfy the requirements.
// - no more than one Namespace Object can exist in the input resource.items
// - If Namespace object exists, its name is the `oldNs`
// - If `namespaceMatcher` is given, its value is the `oldNs`
// - If neither Namespace object nor `namespaceMatcher` found, all resources should have the same namespace value and
// this value is teh `oldNs`
func (p *NamespaceTransformer) GetOldNamespaces(fromResources []string, nsCount map[string]int, originNamespaces []string) ([]string, bool) {
	if p.NamespaceMatcher != "" {
		if len(nsCount) == 0 {
			return []string{p.NamespaceMatcher}, true
		}
		p.Errors = append(p.Errors,
			"found Namespace objects from the input resources, "+
				"you cannot use `namespaceMatcher` in FunctionConfig together with Namespace objects")
		return nil, false
	}
	if len(nsCount) > 1 {
		msg := fmt.Sprintf("cannot accept more than one Namespace objects from the input resources, found %v",
			nsCount)
		p.Errors = append(p.Errors, msg)
		return nil, false
	}
	if len(nsCount) == 1 {
		// Use the namespace object as the matching namespace if `namespaceMatcher` is not given.
		oldNs := reflect.ValueOf(nsCount).MapKeys()[0].String()
		if nsCount[oldNs] > 1 {
			msg := fmt.Sprintf("found more than one Namespace objects of the same name %q", oldNs)
			p.Errors = append(p.Errors, msg)
			return nil, false
		}
		p.NamespaceMatcher = oldNs
		return append(originNamespaces, oldNs), true
	}
	fromResources = Difference(fromResources, originNamespaces)
	if len(fromResources) > 1 {
		msg := fmt.Sprintf("all namespace-scoped resources should be under the same namespace "+
			"or shown in previous namespaces. Found different namespaces: %q ", strings.Join(fromResources, ","))
		p.Errors = append(p.Errors, msg)
		return nil, false
	}
	if len(fromResources) == 0 {
		msg := "could not find any namespace fields to update. This function requires at least one of Namespace objects or " +
			"namespace-scoped resources to have their namespace field set."
		p.Errors = append(p.Errors, msg)
		return nil, false
	}
	p.NamespaceMatcher = fromResources[0]
	return append(originNamespaces, fromResources[0]), true
}

// Difference returns the a - b slice.
func Difference(a, b []string) (diff []string) {
	m := make(map[string]bool)

	for _, item := range b {
		m[item] = true
	}

	for _, item := range a {
		if _, ok := m[item]; !ok {
			diff = append(diff, item)
		}
	}
	return
}

type NamespaceTransformer struct {
	NewNamespace     string
	NamespaceMatcher string
	DependsOnMap     map[string]bool
	Errors           []string
}

func NewNamespace(obj *fn.KubeObject, namespacePtr *string, originNamespaces []string) *Namespace {
	annotationSetter := func(newAnnotation string) {
		obj.SetAnnotation(dependsOnAnnotation, newAnnotation)
	}
	annotationGetter := func() string {
		group := obj.GetAPIVersion()
		if i := strings.Index(obj.GetAPIVersion(), "/"); i > -1 {
			group = group[:i]
		}
		return dependsOnKeyPattern(group, obj.GetKind(), obj.GetName())
	}
	return &Namespace{
		Ptr:                 namespacePtr, //  obj.GetStringOrDie(path...),
		IsNamespace:         obj.IsGVK("", "v1", "Namespace"),
		DependsOnAnnotation: obj.GetAnnotations()[dependsOnAnnotation],
		OriginNamespaces:    originNamespaces,
		annotationGetter:    annotationGetter,
		annotationSetter:    annotationSetter,
	}
}

type Namespace struct {
	Ptr                 *string
	IsNamespace         bool
	DependsOnAnnotation string
	OriginNamespaces    []string
	annotationGetter    func() string
	annotationSetter    func(newDependsOnAnnotation string)
}

func (n *Namespace) SetDependsOnAnnotation(newDependsOn string) {
	n.annotationSetter(newDependsOn)
}

func (n *Namespace) GetDependsOnAnnotation() string {
	return n.annotationGetter()
}
