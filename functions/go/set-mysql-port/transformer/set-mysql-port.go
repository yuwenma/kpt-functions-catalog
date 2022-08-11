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
	corev1 "k8s.io/api/core/v1"
)

const (
	GeneratorBuiltinIdentifier = fn.KptUseOnlyPrefix + "generator-builtin-only"
	PortSuffix                 = ".port"
)

var _ fn.Runner = &SetMysqlPort{}

type SetMysqlPort struct {
	Spec MySqlPortSpec
}

type MySqlPortSpec struct {
	Port int32 `json:"port,omitempty" yaml:"port,omitempty"`
}

func IsIniConfigMap(o *fn.KubeObject) bool {
	if o.GetKind() != "ConfigMap" {
		return false
	}
	if o.GetAnnotation(GeneratorBuiltinIdentifier) == "" {
		return false
	}
	return true
}

func (t *SetMysqlPort) UpdatePortInINIConfigMap(ctx *fn.Context, o *fn.KubeObject) {
	newData := map[string]string{}
	data := o.NestedStringMapOrDie("data")
	for key, value := range data {
		if strings.HasSuffix(key, PortSuffix) {
			newData[key] = string(t.Spec.Port)
			sectionKey := strings.Split(key, ".")
			ctx.ResultInfo(fmt.Sprintf("update INI field %v from %v to %v", sectionKey[1]+"."+sectionKey[2], value, t.Spec.Port), nil)
		} else {
			newData[key] = value
		}
	}
	o.SetNestedStringMapOrDie(newData, "data")

}

func (t *SetMysqlPort) Run(ctx *fn.Context, fnConfig *fn.KubeObject, items fn.KubeObjects) {
	for _, object := range items {
		if IsIniConfigMap(object) {
			t.UpdatePortInINIConfigMap(ctx, object)
			continue
		}
		if object.GetKind() == "StatefulSet" {
			var podSpec corev1.PodSpec
			podSpecObject := object.GetMap("spec").GetMap("template").GetMap("spec")
			podSpecObject.AsOrDie(&podSpec)
			for _, container := range podSpec.Containers {
				for _, port := range container.Ports {
					port.ContainerPort = t.Spec.Port
				}
			}
			podSpecObject.SetNestedField(podSpec)
		}
	}
}
