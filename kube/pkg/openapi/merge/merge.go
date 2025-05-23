/*
Copyright 2017 The Kubernetes Authors.

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

package merge

import (
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/go-openapi/spec"
	"k8s.io/kube-openapi/pkg/util"
)

const gvkKey = "x-kubesphere-group-version-kind"

// usedDefinitionForSpec returns a map with all used definitions in the provided spec as keys and true as values.
func usedDefinitionForSpec(root *spec.Swagger) map[string]bool {
	usedDefinitions := map[string]bool{}
	walkOnAllReferences(func(ref *spec.Ref) {
		if refStr := ref.String(); refStr != "" && strings.HasPrefix(refStr, definitionPrefix) {
			usedDefinitions[refStr[len(definitionPrefix):]] = true
		}
	}, root)
	return usedDefinitions
}

// FilterSpecByPathsWithoutSideEffects removes unnecessary paths and definitions used by those paths.
// i.e. if a Path removed by this function, all definitions used by it and not used
// anywhere else will also be removed.
// It does not modify the input, but the output shares data structures with the input.
func FilterSpecByPathsWithoutSideEffects(sp *spec.Swagger, keepPathPrefixes []string) *spec.Swagger {
	if sp.Paths == nil {
		return sp
	}

	// Walk all references to find all used definitions. This function
	// want to only deal with unused definitions resulted from filtering paths.
	// Thus a definition will be removed only if it has been used before but
	// it is unused because of a path prune.
	initialUsedDefinitions := usedDefinitionForSpec(sp)

	// First remove unwanted paths
	prefixes := util.NewTrie(keepPathPrefixes)
	ret := *sp
	ret.Paths = &spec.Paths{
		VendorExtensible: sp.Paths.VendorExtensible,
		Paths:            map[string]spec.PathItem{},
	}
	for path, pathItem := range sp.Paths.Paths {
		if !prefixes.HasPrefix(path) {
			continue
		}
		ret.Paths.Paths[path] = pathItem
	}

	// Walk all references to find all definition references.
	usedDefinitions := usedDefinitionForSpec(&ret)

	// Remove unused definitions
	ret.Definitions = spec.Definitions{}
	for k, v := range sp.Definitions {
		if usedDefinitions[k] || !initialUsedDefinitions[k] {
			ret.Definitions[k] = v
		}
	}

	return &ret
}

// renameDefinitions renames definition references, without mutating the input.
// The output might share data structures with the input.
func renameDefinitions(s *spec.Swagger, renames map[string]string) *spec.Swagger {
	refRenames := make(map[string]string, len(renames))
	foundOne := false
	for k, v := range renames {
		refRenames[definitionPrefix+k] = definitionPrefix + v
		if _, ok := s.Definitions[k]; ok {
			foundOne = true
		}
	}

	if !foundOne {
		return s
	}

	ret := &spec.Swagger{}
	*ret = *s

	ret = ReplaceReferences(func(ref *spec.Ref) *spec.Ref {
		refName := ref.String()
		if newRef, found := refRenames[refName]; found {
			ret := spec.MustCreateRef(newRef)
			return &ret
		}
		return ref
	}, ret)

	renamedDefinitions := make(spec.Definitions, len(ret.Definitions))
	for k, v := range ret.Definitions {
		if newRef, found := renames[k]; found {
			k = newRef
		}
		renamedDefinitions[k] = v
	}
	ret.Definitions = renamedDefinitions

	return ret
}

// renameParameters renames parameter references, without mutating the input.
// The output might share data structures with the input.
func renameParameters(s *spec.Swagger, renames map[string]string) *spec.Swagger {
	refRenames := make(map[string]string, len(renames))
	foundOne := false
	for k, v := range renames {
		refRenames[parameterPrefix+k] = parameterPrefix + v
		if _, ok := s.Parameters[k]; ok {
			foundOne = true
		}
	}

	if !foundOne {
		return s
	}

	ret := &spec.Swagger{}
	*ret = *s

	ret = ReplaceReferences(func(ref *spec.Ref) *spec.Ref {
		refName := ref.String()
		if newRef, found := refRenames[refName]; found {
			ret := spec.MustCreateRef(newRef)
			return &ret
		}
		return ref
	}, ret)

	renamed := make(map[string]spec.Parameter, len(ret.Parameters))
	for k, v := range ret.Parameters {
		if newRef, found := renames[k]; found {
			k = newRef
		}
		renamed[k] = v
	}
	ret.Parameters = renamed

	return ret
}

// MergeSpecsIgnorePathConflictRenamingDefinitionsAndParameters is the same as
// MergeSpecs except it will ignore any path conflicts by keeping the paths of
// destination. It will rename definition and parameter conflicts.
func MergeSpecsIgnorePathConflictRenamingDefinitionsAndParameters(dest, source *spec.Swagger) error {
	return mergeSpecs(dest, source, true, true, true)
}

// mergeSpecs merges source into dest while resolving conflicts.
// The source is not mutated.
func mergeSpecs(dest, source *spec.Swagger, renameModelConflicts, renameParameterConflicts, ignorePathConflicts bool) (err error) {
	// Paths may be empty, due to [ACL constraints](http://goo.gl/8us55a#securityFiltering).
	if source.Paths == nil {
		// When a source spec does not have any path, that means none of the definitions
		// are used thus we should not do anything
		return nil
	}
	if dest.Paths == nil {
		dest.Paths = &spec.Paths{}
	}
	if ignorePathConflicts {
		keepPaths := []string{}
		hasConflictingPath := false
		for k := range source.Paths.Paths {
			if _, found := dest.Paths.Paths[k]; !found {
				keepPaths = append(keepPaths, k)
			} else {
				hasConflictingPath = true
			}
		}
		if len(keepPaths) == 0 {
			// There is nothing to merge. All paths are conflicting.
			return nil
		}
		if hasConflictingPath {
			source = FilterSpecByPathsWithoutSideEffects(source, keepPaths)
		}
	}

	// Check for model conflicts and rename to make definitions conflict-free (modulo different GVKs)
	usedNames := map[string]bool{}
	for k := range dest.Definitions {
		usedNames[k] = true
	}
	renames := map[string]string{}
DEFINITIONLOOP:
	for k, v := range source.Definitions {
		existing, found := dest.Definitions[k]
		if !found || deepEqualDefinitionsModuloGVKs(&existing, &v) {
			// skip for now, we copy them after the rename loop
			continue
		}

		if !renameModelConflicts {
			return fmt.Errorf("model name conflict in merging OpenAPI spec: %s", k)
		}

		// Reuse previously renamed model if one exists
		var newName string
		i := 1
		for found {
			i++
			newName = fmt.Sprintf("%s_v%d", k, i)
			existing, found = dest.Definitions[newName]
			if found && deepEqualDefinitionsModuloGVKs(&existing, &v) {
				renames[k] = newName
				continue DEFINITIONLOOP
			}
		}

		_, foundInSource := source.Definitions[newName]
		for usedNames[newName] || foundInSource {
			i++
			newName = fmt.Sprintf("%s_v%d", k, i)
			_, foundInSource = source.Definitions[newName]
		}
		renames[k] = newName
		usedNames[newName] = true
	}
	source = renameDefinitions(source, renames)

	// Check for parameter conflicts and rename to make parameters conflict-free
	usedNames = map[string]bool{}
	for k := range dest.Parameters {
		usedNames[k] = true
	}
	renames = map[string]string{}
PARAMETERLOOP:
	for k, p := range source.Parameters {
		existing, found := dest.Parameters[k]
		if !found || reflect.DeepEqual(&existing, &p) {
			// skip for now, we copy them after the rename loop
			continue
		}

		if !renameParameterConflicts {
			return fmt.Errorf("parameter name conflict in merging OpenAPI spec: %s", k)
		}

		// Reuse previously renamed parameter if one exists
		var newName string
		i := 1
		for found {
			i++
			newName = fmt.Sprintf("%s_v%d", k, i)
			existing, found = dest.Parameters[newName]
			if found && reflect.DeepEqual(&existing, &p) {
				renames[k] = newName
				continue PARAMETERLOOP
			}
		}

		_, foundInSource := source.Parameters[newName]
		for usedNames[newName] || foundInSource {
			i++
			newName = fmt.Sprintf("%s_v%d", k, i)
			_, foundInSource = source.Parameters[newName]
		}
		renames[k] = newName
		usedNames[newName] = true
	}
	source = renameParameters(source, renames)

	// Now without conflict (modulo different GVKs), copy definitions to dest
	for k, v := range source.Definitions {
		if existing, found := dest.Definitions[k]; !found {
			if dest.Definitions == nil {
				dest.Definitions = make(spec.Definitions, len(source.Definitions))
			}
			dest.Definitions[k] = v
		} else if merged, changed, err := mergedGVKs(&existing, &v); err != nil {
			return err
		} else if changed {
			existing.Extensions[gvkKey] = merged
		}
	}

	// Now without conflict, copy parameters to dest
	for k, v := range source.Parameters {
		if _, found := dest.Parameters[k]; !found {
			if dest.Parameters == nil {
				dest.Parameters = make(map[string]spec.Parameter, len(source.Parameters))
			}
			dest.Parameters[k] = v
		}
	}

	// Check for path conflicts
	for k, v := range source.Paths.Paths {
		if _, found := dest.Paths.Paths[k]; found {
			return fmt.Errorf("unable to merge: duplicated path %s", k)
		}
		// PathItem may be empty, due to [ACL constraints](http://goo.gl/8us55a#securityFiltering).
		if dest.Paths.Paths == nil {
			dest.Paths.Paths = map[string]spec.PathItem{}
		}
		dest.Paths.Paths[k] = v
	}

	return nil
}

// deepEqualDefinitionsModuloGVKs compares s1 and s2, but ignores the x-kubernetes-group-version-kind extension.
func deepEqualDefinitionsModuloGVKs(s1, s2 *spec.Schema) bool {
	if s1 == nil {
		return s2 == nil
	} else if s2 == nil {
		return false
	}
	if !reflect.DeepEqual(s1.Extensions, s2.Extensions) {
		for k, v := range s1.Extensions {
			if k == gvkKey {
				continue
			}
			if !reflect.DeepEqual(v, s2.Extensions[k]) {
				return false
			}
		}
		len1 := len(s1.Extensions)
		len2 := len(s2.Extensions)
		if _, found := s1.Extensions[gvkKey]; found {
			len1--
		}
		if _, found := s2.Extensions[gvkKey]; found {
			len2--
		}
		if len1 != len2 {
			return false
		}

		if s1.Extensions != nil {
			shallowCopy := *s1
			s1 = &shallowCopy
			s1.Extensions = nil
		}
		if s2.Extensions != nil {
			shallowCopy := *s2
			s2 = &shallowCopy
			s2.Extensions = nil
		}
	}

	return reflect.DeepEqual(s1, s2)
}

// mergedGVKs merges the x-kubernetes-group-version-kind slices and returns the result, and whether
// s1's x-kubernetes-group-version-kind slice was changed at all.
func mergedGVKs(s1, s2 *spec.Schema) (interface{}, bool, error) {
	gvk1, found1 := s1.Extensions[gvkKey]
	gvk2, found2 := s2.Extensions[gvkKey]

	if !found1 {
		return gvk2, found2, nil
	}
	if !found2 {
		return gvk1, false, nil
	}

	slice1, ok := gvk1.([]interface{})
	if !ok {
		return nil, false, fmt.Errorf("expected slice of GroupVersionKinds, got: %+v", slice1)
	}
	slice2, ok := gvk2.([]interface{})
	if !ok {
		return nil, false, fmt.Errorf("expected slice of GroupVersionKinds, got: %+v", slice2)
	}

	ret := make([]interface{}, len(slice1), len(slice1)+len(slice2))
	keys := make([]string, 0, len(slice1)+len(slice2))
	copy(ret, slice1)
	seen := make(map[string]bool, len(slice1))
	for _, x := range slice1 {
		gvk, ok := x.(map[string]interface{})
		if !ok {
			return nil, false, fmt.Errorf(`expected {"group": <group>, "kind": <kind>, "version": <version>}, got: %#v`, x)
		}
		k := fmt.Sprintf("%s/%s.%s", gvk["group"], gvk["version"], gvk["kind"])
		keys = append(keys, k)
		seen[k] = true
	}
	changed := false
	for _, x := range slice2 {
		gvk, ok := x.(map[string]interface{})
		if !ok {
			return nil, false, fmt.Errorf(`expected {"group": <group>, "kind": <kind>, "version": <version>}, got: %#v`, x)
		}
		k := fmt.Sprintf("%s/%s.%s", gvk["group"], gvk["version"], gvk["kind"])
		if seen[k] {
			continue
		}
		ret = append(ret, x)
		keys = append(keys, k)
		changed = true
	}

	if changed {
		sort.Sort(byKeys{ret, keys})
	}

	return ret, changed, nil
}

type byKeys struct {
	values []interface{}
	keys   []string
}

func (b byKeys) Len() int {
	return len(b.values)
}

func (b byKeys) Less(i, j int) bool {
	return b.keys[i] < b.keys[j]
}

func (b byKeys) Swap(i, j int) {
	b.values[i], b.values[j] = b.values[j], b.values[i]
	b.keys[i], b.keys[j] = b.keys[j], b.keys[i]
}

func ReplaceReferences(walkRef func(ref *spec.Ref) *spec.Ref, sp *spec.Swagger) *spec.Swagger {
	walker := &Walker{RefCallback: walkRef, SchemaCallback: SchemaCallBackNoop}
	return walker.WalkRoot(sp)
}

type Walker struct {
	// SchemaCallback will be called on each schema, taking the original schema,
	// and before any other callbacks of the Walker.
	// If the schema needs to be mutated, DO NOT mutate it in-place,
	// always create a copy, mutate, and return it.
	SchemaCallback func(schema *spec.Schema) *spec.Schema

	// RefCallback will be called on each ref.
	// If the ref needs to be mutated, DO NOT mutate it in-place,
	// always create a copy, mutate, and return it.
	RefCallback func(ref *spec.Ref) *spec.Ref
}

type SchemaCallbackFunc func(schema *spec.Schema) *spec.Schema
type RefCallbackFunc func(ref *spec.Ref) *spec.Ref

var SchemaCallBackNoop SchemaCallbackFunc = func(schema *spec.Schema) *spec.Schema {
	return schema
}
var RefCallbackNoop RefCallbackFunc = func(ref *spec.Ref) *spec.Ref {
	return ref
}

func (w *Walker) WalkRoot(swagger *spec.Swagger) *spec.Swagger {
	if swagger == nil {
		return nil
	}

	orig := swagger
	cloned := false
	clone := func() {
		if !cloned {
			cloned = true
			swagger = &spec.Swagger{}
			*swagger = *orig
		}
	}

	parametersCloned := false
	for k, v := range swagger.Parameters {
		if p := w.walkParameter(&v); p != &v {
			if !parametersCloned {
				parametersCloned = true
				clone()
				swagger.Parameters = make(map[string]spec.Parameter, len(orig.Parameters))
				for k2, v2 := range orig.Parameters {
					swagger.Parameters[k2] = v2
				}
			}
			swagger.Parameters[k] = *p
		}
	}

	responsesCloned := false
	for k, v := range swagger.Responses {
		if r := w.walkResponse(&v); r != &v {
			if !responsesCloned {
				responsesCloned = true
				clone()
				swagger.Responses = make(map[string]spec.Response, len(orig.Responses))
				for k2, v2 := range orig.Responses {
					swagger.Responses[k2] = v2
				}
			}
			swagger.Responses[k] = *r
		}
	}

	definitionsCloned := false
	for k, v := range swagger.Definitions {
		if s := w.WalkSchema(&v); s != &v {
			if !definitionsCloned {
				definitionsCloned = true
				clone()
				swagger.Definitions = make(spec.Definitions, len(orig.Definitions))
				for k2, v2 := range orig.Definitions {
					swagger.Definitions[k2] = v2
				}
			}
			swagger.Definitions[k] = *s
		}
	}

	if swagger.Paths != nil {
		if p := w.walkPaths(swagger.Paths); p != swagger.Paths {
			clone()
			swagger.Paths = p
		}
	}

	return swagger
}

func (w *Walker) WalkSchema(schema *spec.Schema) *spec.Schema {
	if schema == nil {
		return nil
	}

	orig := schema
	clone := func() {
		if orig == schema {
			schema = &spec.Schema{}
			*schema = *orig
		}
	}

	// Always run callback on the whole schema first
	// so that SchemaCallback can take the original schema as input.
	schema = w.SchemaCallback(schema)

	if r := w.RefCallback(&schema.Ref); r != &schema.Ref {
		clone()
		schema.Ref = *r
	}

	definitionsCloned := false
	for k, v := range schema.Definitions {
		if s := w.WalkSchema(&v); s != &v {
			if !definitionsCloned {
				definitionsCloned = true
				clone()
				schema.Definitions = make(spec.Definitions, len(orig.Definitions))
				for k2, v2 := range orig.Definitions {
					schema.Definitions[k2] = v2
				}
			}
			schema.Definitions[k] = *s
		}
	}

	propertiesCloned := false
	for k, v := range schema.Properties {
		if s := w.WalkSchema(&v); s != &v {
			if !propertiesCloned {
				propertiesCloned = true
				clone()
				schema.Properties = make(map[string]spec.Schema, len(orig.Properties))
				for k2, v2 := range orig.Properties {
					schema.Properties[k2] = v2
				}
			}
			schema.Properties[k] = *s
		}
	}

	patternPropertiesCloned := false
	for k, v := range schema.PatternProperties {
		if s := w.WalkSchema(&v); s != &v {
			if !patternPropertiesCloned {
				patternPropertiesCloned = true
				clone()
				schema.PatternProperties = make(map[string]spec.Schema, len(orig.PatternProperties))
				for k2, v2 := range orig.PatternProperties {
					schema.PatternProperties[k2] = v2
				}
			}
			schema.PatternProperties[k] = *s
		}
	}

	allOfCloned := false
	for i := range schema.AllOf {
		if s := w.WalkSchema(&schema.AllOf[i]); s != &schema.AllOf[i] {
			if !allOfCloned {
				allOfCloned = true
				clone()
				schema.AllOf = make([]spec.Schema, len(orig.AllOf))
				copy(schema.AllOf, orig.AllOf)
			}
			schema.AllOf[i] = *s
		}
	}

	anyOfCloned := false
	for i := range schema.AnyOf {
		if s := w.WalkSchema(&schema.AnyOf[i]); s != &schema.AnyOf[i] {
			if !anyOfCloned {
				anyOfCloned = true
				clone()
				schema.AnyOf = make([]spec.Schema, len(orig.AnyOf))
				copy(schema.AnyOf, orig.AnyOf)
			}
			schema.AnyOf[i] = *s
		}
	}

	oneOfCloned := false
	for i := range schema.OneOf {
		if s := w.WalkSchema(&schema.OneOf[i]); s != &schema.OneOf[i] {
			if !oneOfCloned {
				oneOfCloned = true
				clone()
				schema.OneOf = make([]spec.Schema, len(orig.OneOf))
				copy(schema.OneOf, orig.OneOf)
			}
			schema.OneOf[i] = *s
		}
	}

	if schema.Not != nil {
		if s := w.WalkSchema(schema.Not); s != schema.Not {
			clone()
			schema.Not = s
		}
	}

	if schema.AdditionalProperties != nil && schema.AdditionalProperties.Schema != nil {
		if s := w.WalkSchema(schema.AdditionalProperties.Schema); s != schema.AdditionalProperties.Schema {
			clone()
			schema.AdditionalProperties = &spec.SchemaOrBool{Schema: s, Allows: schema.AdditionalProperties.Allows}
		}
	}

	if schema.AdditionalItems != nil && schema.AdditionalItems.Schema != nil {
		if s := w.WalkSchema(schema.AdditionalItems.Schema); s != schema.AdditionalItems.Schema {
			clone()
			schema.AdditionalItems = &spec.SchemaOrBool{Schema: s, Allows: schema.AdditionalItems.Allows}
		}
	}

	if schema.Items != nil {
		if schema.Items.Schema != nil {
			if s := w.WalkSchema(schema.Items.Schema); s != schema.Items.Schema {
				clone()
				schema.Items = &spec.SchemaOrArray{Schema: s}
			}
		} else {
			itemsCloned := false
			for i := range schema.Items.Schemas {
				if s := w.WalkSchema(&schema.Items.Schemas[i]); s != &schema.Items.Schemas[i] {
					if !itemsCloned {
						clone()
						schema.Items = &spec.SchemaOrArray{
							Schemas: make([]spec.Schema, len(orig.Items.Schemas)),
						}
						itemsCloned = true
						copy(schema.Items.Schemas, orig.Items.Schemas)
					}
					schema.Items.Schemas[i] = *s
				}
			}
		}
	}

	return schema
}

func (w *Walker) walkParameter(param *spec.Parameter) *spec.Parameter {
	if param == nil {
		return nil
	}

	orig := param
	cloned := false
	clone := func() {
		if !cloned {
			cloned = true
			param = &spec.Parameter{}
			*param = *orig
		}
	}

	if r := w.RefCallback(&param.Ref); r != &param.Ref {
		clone()
		param.Ref = *r
	}
	if s := w.WalkSchema(param.Schema); s != param.Schema {
		clone()
		param.Schema = s
	}
	if param.Items != nil {
		if r := w.RefCallback(&param.Items.Ref); r != &param.Items.Ref {
			param.Items.Ref = *r
		}
	}

	return param
}

func (w *Walker) walkParameters(params []spec.Parameter) ([]spec.Parameter, bool) {
	if params == nil {
		return nil, false
	}

	orig := params
	cloned := false
	clone := func() {
		if !cloned {
			cloned = true
			params = make([]spec.Parameter, len(params))
			copy(params, orig)
		}
	}

	for i := range params {
		if s := w.walkParameter(&params[i]); s != &params[i] {
			clone()
			params[i] = *s
		}
	}

	return params, cloned
}

func (w *Walker) walkResponse(resp *spec.Response) *spec.Response {
	if resp == nil {
		return nil
	}

	orig := resp
	cloned := false
	clone := func() {
		if !cloned {
			cloned = true
			resp = &spec.Response{}
			*resp = *orig
		}
	}

	if r := w.RefCallback(&resp.Ref); r != &resp.Ref {
		clone()
		resp.Ref = *r
	}
	if s := w.WalkSchema(resp.Schema); s != resp.Schema {
		clone()
		resp.Schema = s
	}

	return resp
}

func (w *Walker) walkResponses(resps *spec.Responses) *spec.Responses {
	if resps == nil {
		return nil
	}

	orig := resps
	cloned := false
	clone := func() {
		if !cloned {
			cloned = true
			resps = &spec.Responses{}
			*resps = *orig
		}
	}

	if r := w.walkResponse(resps.ResponsesProps.Default); r != resps.ResponsesProps.Default {
		clone()
		resps.Default = r
	}

	responsesCloned := false
	for k, v := range resps.ResponsesProps.StatusCodeResponses {
		if r := w.walkResponse(&v); r != &v {
			if !responsesCloned {
				responsesCloned = true
				clone()
				resps.ResponsesProps.StatusCodeResponses = make(map[int]spec.Response, len(orig.StatusCodeResponses))
				for k2, v2 := range orig.StatusCodeResponses {
					resps.ResponsesProps.StatusCodeResponses[k2] = v2
				}
			}
			resps.ResponsesProps.StatusCodeResponses[k] = *r
		}
	}

	return resps
}

func (w *Walker) walkOperation(op *spec.Operation) *spec.Operation {
	if op == nil {
		return nil
	}

	orig := op
	cloned := false
	clone := func() {
		if !cloned {
			cloned = true
			op = &spec.Operation{}
			*op = *orig
		}
	}

	parametersCloned := false
	for i := range op.Parameters {
		if s := w.walkParameter(&op.Parameters[i]); s != &op.Parameters[i] {
			if !parametersCloned {
				parametersCloned = true
				clone()
				op.Parameters = make([]spec.Parameter, len(orig.Parameters))
				copy(op.Parameters, orig.Parameters)
			}
			op.Parameters[i] = *s
		}
	}

	if r := w.walkResponses(op.Responses); r != op.Responses {
		clone()
		op.Responses = r
	}

	return op
}

func (w *Walker) walkPathItem(pathItem *spec.PathItem) *spec.PathItem {
	if pathItem == nil {
		return nil
	}

	orig := pathItem
	cloned := false
	clone := func() {
		if !cloned {
			cloned = true
			pathItem = &spec.PathItem{}
			*pathItem = *orig
		}
	}

	if p, changed := w.walkParameters(pathItem.Parameters); changed {
		clone()
		pathItem.Parameters = p
	}
	if op := w.walkOperation(pathItem.Get); op != pathItem.Get {
		clone()
		pathItem.Get = op
	}
	if op := w.walkOperation(pathItem.Head); op != pathItem.Head {
		clone()
		pathItem.Head = op
	}
	if op := w.walkOperation(pathItem.Delete); op != pathItem.Delete {
		clone()
		pathItem.Delete = op
	}
	if op := w.walkOperation(pathItem.Options); op != pathItem.Options {
		clone()
		pathItem.Options = op
	}
	if op := w.walkOperation(pathItem.Patch); op != pathItem.Patch {
		clone()
		pathItem.Patch = op
	}
	if op := w.walkOperation(pathItem.Post); op != pathItem.Post {
		clone()
		pathItem.Post = op
	}
	if op := w.walkOperation(pathItem.Put); op != pathItem.Put {
		clone()
		pathItem.Put = op
	}

	return pathItem
}

func (w *Walker) walkPaths(paths *spec.Paths) *spec.Paths {
	if paths == nil {
		return nil
	}

	orig := paths
	cloned := false
	clone := func() {
		if !cloned {
			cloned = true
			paths = &spec.Paths{}
			*paths = *orig
		}
	}

	pathsCloned := false
	for k, v := range paths.Paths {
		if p := w.walkPathItem(&v); p != &v {
			if !pathsCloned {
				pathsCloned = true
				clone()
				paths.Paths = make(map[string]spec.PathItem, len(orig.Paths))
				for k2, v2 := range orig.Paths {
					paths.Paths[k2] = v2
				}
			}
			paths.Paths[k] = *p
		}
	}

	return paths
}
