/*
Copyright 2026 The gateway-api-openstack Authors.

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

package design

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// The mapping is an inventory, not a declaration of supported features or a
// conformance result. Read upstream sources without importing or running its
// cluster-mutating suite into ordinary unit tests.
func TestCoreMapping(t *testing.T) {
	data, err := os.ReadFile("core-mapping.json")
	if err != nil {
		t.Fatal(err)
	}
	var mapping struct {
		GatewayAPIVersion string `json:"gatewayAPIVersion"`
		Profile           string `json:"profile"`
		Requirements      []struct {
			Name  string   `json:"name"`
			Tests []string `json:"tests"`
		} `json:"requirements"`
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&mapping); err != nil {
		t.Fatal(err)
	}
	module, err := os.ReadFile("../../go.mod")
	if err != nil {
		t.Fatal(err)
	}
	if mapping.Profile != "GATEWAY-HTTP" || mapping.GatewayAPIVersion == "" ||
		!strings.Contains(string(module), "sigs.k8s.io/gateway-api "+mapping.GatewayAPIVersion+"\n") {
		t.Fatal("Core mapping must target GATEWAY-HTTP and the pinned Gateway API version")
	}
	mapped := make(map[string]string)
	categories := make(map[string]bool)
	for _, requirement := range mapping.Requirements {
		if requirement.Name == "" || categories[requirement.Name] || len(requirement.Tests) == 0 {
			t.Fatalf("empty or duplicate requirement: %q", requirement.Name)
		}
		categories[requirement.Name] = true
		for _, name := range requirement.Tests {
			if previous, exists := mapped[name]; name == "" || exists {
				t.Fatalf("empty or duplicate test %q in %q and %q", name, previous, requirement.Name)
			}
			mapped[name] = requirement.Name
		}
	}
	if len(mapped) == 0 {
		t.Fatal("Core mapping is empty")
	}
	source := os.Getenv("GATEWAY_API_CONFORMANCE_SOURCE_DIR")
	if source == "" {
		t.Log("Set GATEWAY_API_CONFORMANCE_SOURCE_DIR to check the inventory against pinned upstream sources")
		return
	}
	core, err := coreTestNames(source)
	if err != nil {
		t.Fatal(err)
	}
	var missing, unexpected []string
	for _, name := range core {
		if _, exists := mapped[name]; !exists {
			missing = append(missing, name)
		}
		delete(mapped, name)
	}
	for name := range mapped {
		unexpected = append(unexpected, name)
	}
	slices.Sort(unexpected)
	if len(missing)+len(unexpected) != 0 {
		t.Fatalf("Core mapping differs from upstream: missing=%v unexpected=%v", missing, unexpected)
	}
	t.Logf("Mapped all %d %s Core tests in %s", len(core), mapping.Profile, mapping.GatewayAPIVersion)
}

func coreTestNames(source string) ([]string, error) {
	profile, err := parser.ParseFile(token.NewFileSet(), filepath.Join(source, "utils/suite/profiles.go"), nil, 0)
	if err != nil {
		return nil, err
	}
	var core []string
	for _, declaration := range profile.Decls {
		decl, ok := declaration.(*ast.GenDecl)
		if !ok || decl.Tok != token.VAR {
			continue
		}
		for _, spec := range decl.Specs {
			value := spec.(*ast.ValueSpec)
			if len(value.Names) != 1 || value.Names[0].Name != "GatewayHTTPConformanceProfile" || len(value.Values) != 1 {
				continue
			}
			literal, ok := value.Values[0].(*ast.CompositeLit)
			if !ok {
				return nil, fmt.Errorf("GatewayHTTPConformanceProfile is not a literal")
			}
			call, ok := literalField(literal, "CoreFeatures").(*ast.CallExpr)
			if !ok {
				return nil, fmt.Errorf("CoreFeatures is not a set constructor")
			}
			core, err = featureNames(call.Args)
			if err != nil {
				return nil, err
			}
		}
	}
	if len(core) == 0 {
		return nil, fmt.Errorf("GATEWAY-HTTP Core features were not found")
	}
	files, err := filepath.Glob(filepath.Join(source, "tests/*.go"))
	if err != nil {
		return nil, err
	}
	names := make(map[string]bool)
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return nil, err
		}
		for _, declaration := range file.Decls {
			decl, ok := declaration.(*ast.GenDecl)
			if !ok || decl.Tok != token.VAR {
				continue
			}
			for _, spec := range decl.Specs {
				for _, value := range spec.(*ast.ValueSpec).Values {
					literal, ok := value.(*ast.CompositeLit)
					if !ok {
						continue
					}
					kind, ok := literal.Type.(*ast.SelectorExpr)
					if !ok || kind.Sel.Name != "ConformanceTest" {
						continue
					}
					name, required, err := conformanceTestFields(literal)
					if err != nil {
						return nil, fmt.Errorf("read %s: %w", filepath.Base(path), err)
					}
					if _, duplicate := names[name]; duplicate {
						return nil, fmt.Errorf("duplicate upstream test %q", name)
					}
					isCore := true
					for _, feature := range required {
						isCore = isCore && slices.Contains(core, feature)
					}
					names[name] = isCore
				}
			}
		}
	}
	var result []string
	for name, isCore := range names {
		if isCore {
			result = append(result, name)
		}
	}
	slices.Sort(result)
	if len(result) == 0 {
		return nil, fmt.Errorf("no GATEWAY-HTTP Core tests were found")
	}
	return result, nil
}

func literalField(literal *ast.CompositeLit, name string) ast.Expr {
	for _, element := range literal.Elts {
		field, ok := element.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		key, ok := field.Key.(*ast.Ident)
		if ok && key.Name == name {
			return field.Value
		}
	}
	return nil
}

func conformanceTestFields(literal *ast.CompositeLit) (string, []string, error) {
	nameValue, ok := literalField(literal, "ShortName").(*ast.BasicLit)
	if !ok || nameValue.Kind != token.STRING {
		return "", nil, fmt.Errorf("ShortName is not a string literal")
	}
	name, err := strconv.Unquote(nameValue.Value)
	if err != nil {
		return "", nil, err
	}
	features, ok := literalField(literal, "Features").(*ast.CompositeLit)
	if !ok {
		return "", nil, fmt.Errorf("Features is not a slice literal")
	}
	required, err := featureNames(features.Elts)
	return name, required, err
}

func featureNames(expressions []ast.Expr) ([]string, error) {
	result := make([]string, 0, len(expressions))
	for _, expression := range expressions {
		feature, ok := expression.(*ast.SelectorExpr)
		if !ok {
			return nil, fmt.Errorf("feature is not a named feature constant")
		}
		result = append(result, feature.Sel.Name)
	}
	return result, nil
}

func TestCoreInventoryExcludesExtendedAndOtherProfiles(t *testing.T) {
	directory := t.TempDir()
	for path, contents := range map[string]string{
		"utils/suite/profiles.go": `package suite
var GatewayHTTPConformanceProfile = ConformanceProfile{
 CoreFeatures: sets.New(features.SupportGateway, features.SupportHTTPRoute, features.SupportReferenceGrant),
}`,
		"tests/core.go": `package tests
var Core = suite.ConformanceTest{ShortName: "Core", Features: []features.FeatureName{features.SupportGateway, features.SupportHTTPRoute}}
var Extended = suite.ConformanceTest{ShortName: "Extended", Features: []features.FeatureName{features.SupportGateway, features.SupportGatewayHTTPListenerIsolation}}
var Mesh = suite.ConformanceTest{ShortName: "Mesh", Features: []features.FeatureName{features.SupportMesh, features.SupportHTTPRoute}}
`,
	} {
		filename := filepath.Join(directory, path)
		if err := os.MkdirAll(filepath.Dir(filename), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filename, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	names, err := coreTestNames(directory)
	if err != nil || !slices.Equal(names, []string{"Core"}) {
		t.Fatalf("Core tests=%v error=%v", names, err)
	}
	duplicate := filepath.Join(directory, "tests/duplicate.go")
	if err := os.WriteFile(duplicate, []byte(`package tests
var Duplicate = suite.ConformanceTest{ShortName: "Core", Features: []features.FeatureName{features.SupportGateway}}
`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := coreTestNames(directory); err == nil {
		t.Fatal("duplicate upstream test was accepted")
	}
}
