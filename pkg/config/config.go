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

// Package config manages and updates the ghpc input config
package config

import (
	"bytes"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/agext/levenshtein"
	"github.com/hashicorp/hcl/v2"
	"github.com/pkg/errors"
	"github.com/zclconf/go-cty/cty"
	"gopkg.in/yaml.v3"

	"hpc-toolkit/pkg/modulereader"
)

const (
	expectedVarFormat        string = "$(vars.var_name) or $(module_id.output_name)"
	expectedModFormat        string = "$(module_id) or $(group_id.module_id)"
	unexpectedConnectionKind string = "connectionKind must be useConnection or deploymentConnection"
	maxHintDist              int    = 3 // Maximum Levenshtein distance where we suggest a hint
)

// map[moved module path]replacing module path
var movedModules = map[string]string{
	"community/modules/scheduler/cloud-batch-job":        "modules/scheduler/batch-job-template",
	"community/modules/scheduler/cloud-batch-login-node": "modules/scheduler/batch-login-node",
	"community/modules/scheduler/htcondor-configure":     "community/modules/scheduler/htcondor-setup",
	"community/modules/scripts/spack-install":            "community/modules/scripts/spack-setup",
}

// GroupName is the name of a deployment group
type GroupName string

// Validate checks that the group name is valid
func (n GroupName) Validate() error {
	if n == "" {
		return EmptyGroupName
	}

	if !regexp.MustCompile(`^\w(-*\w)*$`).MatchString(string(n)) {
		return fmt.Errorf("invalid character(s) found in group name %q.\n"+
			"Allowed : alphanumeric, '_', and '-'; can not start/end with '-'", n)
	}
	return nil
}

// DeploymentGroup defines a group of Modules that are all executed together
type DeploymentGroup struct {
	Name             GroupName        `yaml:"group"`
	TerraformBackend TerraformBackend `yaml:"terraform_backend,omitempty"`
	Modules          []Module         `yaml:"modules"`
	// DEPRECATED fields
	deprecatedKind interface{} `yaml:"kind,omitempty"` //lint:ignore U1000 keep in the struct for backwards compatibility
}

// Kind returns the kind of all the modules in the group.
// If the group contains modules of different kinds, it returns UnknownKind
func (g DeploymentGroup) Kind() ModuleKind {
	if len(g.Modules) == 0 {
		return UnknownKind
	}
	k := g.Modules[0].Kind
	for _, m := range g.Modules {
		if m.Kind != k {
			return UnknownKind
		}
	}
	return k
}

// Module return the module with the given ID
func (bp *Blueprint) Module(id ModuleID) (*Module, error) {
	var mod *Module
	bp.WalkModulesSafe(func(_ ModulePath, m *Module) {
		if m.ID == id {
			mod = m
		}
	})
	if mod == nil {
		return nil, UnknownModuleError{id}
	}
	return mod, nil
}

func hintSpelling(s string, dict []string, err error) error {
	best, minDist := "", maxHintDist+1
	for _, w := range dict {
		d := levenshtein.Distance(s, w, nil)
		if d < minDist {
			best, minDist = w, d
		}
	}
	if minDist <= maxHintDist {
		return HintError{fmt.Sprintf("did you mean %q?", best), err}
	}
	return err

}

// ModuleGroup returns the group containing the module
func (bp Blueprint) ModuleGroup(mod ModuleID) (DeploymentGroup, error) {
	for _, g := range bp.DeploymentGroups {
		for _, m := range g.Modules {
			if m.ID == mod {
				return g, nil
			}
		}
	}
	return DeploymentGroup{}, UnknownModuleError{mod}
}

// ModuleGroupOrDie returns the group containing the module; panics if unfound
func (bp Blueprint) ModuleGroupOrDie(mod ModuleID) DeploymentGroup {
	g, err := bp.ModuleGroup(mod)
	if err != nil {
		panic(fmt.Errorf("module %s not found in blueprint: %s", mod, err))
	}
	return g
}

// GroupIndex returns the index of the input group in the blueprint
// return -1 if not found
func (bp Blueprint) GroupIndex(n GroupName) int {
	for i, g := range bp.DeploymentGroups {
		if g.Name == n {
			return i
		}
	}
	return -1
}

// Group returns the deployment group with a given name
func (bp Blueprint) Group(n GroupName) (DeploymentGroup, error) {
	idx := bp.GroupIndex(n)
	if idx == -1 {
		return DeploymentGroup{}, fmt.Errorf("could not find group %s in blueprint", n)
	}
	return bp.DeploymentGroups[idx], nil
}

// TerraformBackend defines the configuration for the terraform state backend
type TerraformBackend struct {
	Type          string
	Configuration Dict
}

// ModuleKind abstracts Toolkit module kinds (presently: packer/terraform)
type ModuleKind struct {
	kind string
}

// UnknownKind is the default value when the user has not specified module kind
var UnknownKind = ModuleKind{kind: ""}

// TerraformKind is the kind for Terraform modules (should be treated as const)
var TerraformKind = ModuleKind{kind: "terraform"}

// PackerKind is the kind for Packer modules (should be treated as const)
var PackerKind = ModuleKind{kind: "packer"}

// IsValidModuleKind ensures that the user has specified a supported kind
func IsValidModuleKind(kind string) bool {
	return kind == TerraformKind.String() || kind == PackerKind.String() ||
		kind == UnknownKind.String()
}

func (mk ModuleKind) String() string {
	return mk.kind
}

// this enum will be used to control how fatal validator failures will be
// treated during blueprint creation
const (
	ValidationError int = iota
	ValidationWarning
	ValidationIgnore
)

func isValidValidationLevel(level int) bool {
	return !(level > ValidationIgnore || level < ValidationError)
}

// Validator defines a validation step to be run on a blueprint
type Validator struct {
	Validator string
	Inputs    Dict `yaml:"inputs,omitempty"`
	Skip      bool `yaml:"skip,omitempty"`
}

// ModuleID is a unique identifier for a module in a blueprint
type ModuleID string

// ModuleIDs is a list of ModuleID
type ModuleIDs []ModuleID

// Module stores YAML definition of an HPC cluster component defined in a blueprint
type Module struct {
	Source   string
	Kind     ModuleKind
	ID       ModuleID
	Use      ModuleIDs                 `yaml:"use,omitempty"`
	Outputs  []modulereader.OutputInfo `yaml:"outputs,omitempty"`
	Settings Dict                      `yaml:"settings,omitempty"`
	// DEPRECATED fields, keep in the struct for backwards compatibility
	RequiredApis     interface{} `yaml:"required_apis,omitempty"`
	WrapSettingsWith interface{} `yaml:"wrapsettingswith,omitempty"`
}

// InfoOrDie returns the ModuleInfo for the module or panics
func (m Module) InfoOrDie() modulereader.ModuleInfo {
	mi, err := modulereader.GetModuleInfo(m.Source, m.Kind.String())
	if err != nil {
		panic(err)
	}
	return mi
}

// Blueprint stores the contents on the User YAML
// omitempty on validation_level ensures that expand will not expose the setting
// unless it has been set to a non-default value; the implementation as an
// integer is primarily for internal purposes even if it can be set in blueprint
type Blueprint struct {
	BlueprintName            string      `yaml:"blueprint_name"`
	GhpcVersion              string      `yaml:"ghpc_version,omitempty"`
	Validators               []Validator `yaml:"validators,omitempty"`
	ValidationLevel          int         `yaml:"validation_level,omitempty"`
	Vars                     Dict
	DeploymentGroups         []DeploymentGroup `yaml:"deployment_groups"`
	TerraformBackendDefaults TerraformBackend  `yaml:"terraform_backend_defaults,omitempty"`
}

// DeploymentSettings are deployment-specific override settings
type DeploymentSettings struct {
	TerraformBackendDefaults TerraformBackend `yaml:"terraform_backend_defaults,omitempty"`
	Vars                     Dict
}

// Expand expands the config in place
func (bp *Blueprint) Expand() error {
	// expand the blueprint in dependency order:
	// BlueprintName -> DefaultBackend -> Vars -> Groups
	if err := bp.checkBlueprintName(); err != nil {
		return err
	}
	if err := checkBackend(Root.Backend, bp.TerraformBackendDefaults); err != nil {
		return err
	}
	if err := bp.expandVars(); err != nil {
		return err
	}
	return bp.expandGroups()
}

// ListUnusedModules provides a list modules that are in the
// "use" field, but not actually used.
func (m Module) ListUnusedModules() ModuleIDs {
	used := map[ModuleID]bool{}
	// Recurse through objects/maps/lists checking each element for having `ProductOfModuleUse` mark.
	cty.Walk(m.Settings.AsObject(), func(p cty.Path, v cty.Value) (bool, error) {
		for _, mod := range IsProductOfModuleUse(v) {
			used[mod] = true
		}
		return true, nil
	})

	unused := ModuleIDs{}
	for _, w := range m.Use {
		if !used[w] {
			unused = append(unused, w)
		}
	}
	return unused
}

// GetUsedDeploymentVars returns a list of deployment vars used in the given value
func GetUsedDeploymentVars(val cty.Value) []string {
	res := []string{}
	for ref := range valueReferences(val) {
		if ref.GlobalVar {
			res = append(res, ref.Name)
		}
	}
	return res
}

// ListUnusedVariables returns a list of variables that are defined but not used
func (bp Blueprint) ListUnusedVariables() []string {
	// Gather all scopes where variables are used
	ns := map[string]cty.Value{
		"vars": bp.Vars.AsObject(),
	}
	bp.WalkModulesSafe(func(_ ModulePath, m *Module) {
		ns["module_"+string(m.ID)] = m.Settings.AsObject()
	})
	for _, v := range bp.Validators {
		ns["validator_"+v.Validator] = v.Inputs.AsObject()
	}

	var used = map[string]bool{
		"labels":          true, // automatically added
		"deployment_name": true, // required
	}
	for _, v := range GetUsedDeploymentVars(cty.ObjectVal(ns)) {
		used[v] = true
	}

	unused := []string{}
	for _, k := range bp.Vars.Keys() {
		if _, ok := used[k]; !ok {
			unused = append(unused, k)
		}
	}
	return unused
}

func checkMovedModule(source string) error {
	if replacement, ok := movedModules[strings.Trim(source, "./")]; ok {
		return fmt.Errorf(
			"a module has moved. %s has been replaced with %s. Please update the source in your blueprint and try again",
			source, replacement)
	}
	return nil
}

// NewBlueprint is a constructor for Blueprint
func NewBlueprint(configFilename string) (Blueprint, YamlCtx, error) {
	bp, ctx, err := importBlueprint(configFilename)
	if err != nil {
		return Blueprint{}, ctx, err
	}
	// if the validation level has been explicitly set to an invalid value
	// in YAML blueprint then silently default to validationError
	if !isValidValidationLevel(bp.ValidationLevel) {
		bp.ValidationLevel = ValidationError
	}
	return bp, ctx, nil
}

func NewDeploymentSettings(deploymentFilename string) (DeploymentSettings, YamlCtx, error) {
	depl, ctx, err := importDeploymentFile(deploymentFilename)
	if err != nil {
		return DeploymentSettings{}, ctx, err
	}
	return depl, ctx, nil
}

// Export exports the internal representation of a blueprint config
func (bp Blueprint) Export(outputFilename string) error {
	var buf bytes.Buffer
	buf.WriteString(YamlLicense)
	buf.WriteString("\n")
	encoder := yaml.NewEncoder(&buf)
	encoder.SetIndent(2)
	err := encoder.Encode(&bp)
	encoder.Close()
	d := buf.Bytes()

	if err != nil {
		return fmt.Errorf("%s: %w", errMsgYamlMarshalError, err)
	}

	err = os.WriteFile(outputFilename, d, 0644)
	if err != nil {
		// hitting this error writing yaml
		return fmt.Errorf("%s, Filename: %s: %w",
			errMsgYamlSaveError, outputFilename, err)
	}
	return nil
}

// addKindToModules sets the kind to 'terraform' when empty.
func (bp *Blueprint) addKindToModules() {
	bp.WalkModulesSafe(func(_ ModulePath, m *Module) {
		if m.Kind == UnknownKind {
			m.Kind = TerraformKind
		}
	})
}

func checkModulesAndGroups(bp Blueprint) error {
	seenMod := map[ModuleID]bool{}
	seenGrp := map[GroupName]bool{}
	errs := Errors{}

	for ig, grp := range bp.DeploymentGroups {
		pg := Root.Groups.At(ig)
		errs.At(pg.Name, grp.Name.Validate())

		if seenGrp[grp.Name] {
			errs.At(pg.Name, fmt.Errorf("%s: %s used more than once", errMsgDuplicateGroup, grp.Name))
		}
		seenGrp[grp.Name] = true

		if len(grp.Modules) == 0 {
			errs.At(pg.Modules, errors.New("deployment group must have at least one module"))
		} else if grp.Kind() == UnknownKind {
			errs.At(pg.Modules, errors.New("mixing modules of differing kinds in a deployment group is not supported"))
		} else if grp.Kind() == PackerKind && len(grp.Modules) > 1 {
			errs.At(pg, HintError{
				Err:  fmt.Errorf("packer group %q has more than 1 module", grp.Name),
				Hint: "separate each packer module into its own deployment group"})
		}

		for im, mod := range grp.Modules {
			pm := pg.Modules.At(im)
			if seenMod[mod.ID] {
				errs.At(pm.ID, fmt.Errorf("%s: %s used more than once", errMsgDuplicateID, mod.ID))
			}
			seenMod[mod.ID] = true
			errs.Add(validateModule(pm, mod, bp))
		}

		errs.Add(checkBackend(pg.Backend, grp.TerraformBackend))
	}
	return errs.OrNil()
}

// validateModuleUseReferences verifies that any used modules exist and
// are in the correct group
func validateModuleUseReferences(p ModulePath, mod Module, bp Blueprint) error {
	errs := Errors{}
	for iu, used := range mod.Use {
		errs.At(p.Use.At(iu), validateModuleReference(bp, mod, used))
	}
	return errs.OrNil()
}

func checkBackend(bep backendPath, be TerraformBackend) error {
	val, perr := parseYamlString(be.Type)
	if _, is := IsExpressionValue(val); is || perr != nil {
		return BpError{bep.Type, errors.New("can not use expression as a terraform_backend type")}
	}
	return nil
}

// SkipValidator marks validator(s) as skipped,
// if no validator is present, adds one, marked as skipped.
func (bp *Blueprint) SkipValidator(name string) {
	if bp.Validators == nil {
		bp.Validators = []Validator{}
	}
	skipped := false
	for i, v := range bp.Validators {
		if v.Validator == name {
			bp.Validators[i].Skip = true
			skipped = true
		}
	}
	if !skipped {
		bp.Validators = append(bp.Validators, Validator{Validator: name, Skip: true})
	}
}

// InputValueError signifies a problem with the blueprint name.
type InputValueError struct {
	inputKey string
	cause    string
}

func (err InputValueError) Error() string {
	return fmt.Sprintf("%v input error, cause: %v", err.inputKey, err.cause)
}

var matchLabelNameExp *regexp.Regexp = regexp.MustCompile(`^[\p{Ll}\p{Lo}][\p{Ll}\p{Lo}\p{N}_-]{0,62}$`)
var matchLabelValueExp *regexp.Regexp = regexp.MustCompile(`^[\p{Ll}\p{Lo}\p{N}_-]{0,63}$`)

// isValidLabelName checks if a string is a valid name for a GCP label.
// For more information on valid label names, see the docs at:
// https://cloud.google.com/resource-manager/docs/creating-managing-labels#requirements
func isValidLabelName(name string) bool {
	return matchLabelNameExp.MatchString(name)
}

// isValidLabelValue checks if a string is a valid value for a GCP label.
// For more information on valid label values, see the docs at:
// https://cloud.google.com/resource-manager/docs/creating-managing-labels#requirements
func isValidLabelValue(value string) bool {
	return matchLabelValueExp.MatchString(value)
}

func (bp *Blueprint) DeploymentName() string {
	v, _ := bp.Eval(GlobalRef("deployment_name").AsValue()) // ignore errors as we already validated the blueprint
	return v.AsString()
}

func validateDeploymentName(bp Blueprint) error {
	path := Root.Vars.Dot("deployment_name")

	if !bp.Vars.Has("deployment_name") {
		return BpError{path, InputValueError{
			inputKey: "deployment_name",
			cause:    errMsgVarNotFound,
		}}
	}

	v, err := bp.Eval(GlobalRef("deployment_name").AsValue())
	if err != nil {
		return BpError{path, err}
	}
	if v.Type() != cty.String || v.IsNull() || !v.IsKnown() {
		return BpError{path, InputValueError{
			inputKey: "deployment_name",
			cause:    errMsgValueNotString,
		}}
	}

	s := v.AsString()
	if len(s) == 0 {
		return BpError{path, InputValueError{
			inputKey: "deployment_name",
			cause:    errMsgValueEmptyString,
		}}
	}

	// Check that deployment_name is a valid label
	if !isValidLabelValue(s) {
		return BpError{path, InputValueError{
			inputKey: "deployment_name",
			cause:    errMsgLabelValueReqs,
		}}
	}
	return nil
}

// ProjectID returns the project_id
func (bp Blueprint) ProjectID() (string, error) {
	pid := "project_id"
	if !bp.Vars.Has(pid) {
		return "", BpError{Root.Vars, fmt.Errorf("%q variable is not specified", pid)}
	}

	v, err := bp.Eval(GlobalRef(pid).AsValue())
	if err != nil {
		return "", err
	}
	if v.Type() != cty.String {
		return "", BpError{Root.Vars.Dot(pid), fmt.Errorf("%q variable is not a string", pid)}
	}
	return v.AsString(), nil
}

// checkBlueprintName returns an error if blueprint_name does not comply with
// requirements for correct GCP label values.
func (bp *Blueprint) checkBlueprintName() error {
	if len(bp.BlueprintName) == 0 {
		return BpError{Root.BlueprintName, InputValueError{
			inputKey: "blueprint_name",
			cause:    errMsgValueEmptyString,
		}}
	}

	if !isValidLabelValue(bp.BlueprintName) {
		return BpError{Root.BlueprintName, InputValueError{
			inputKey: "blueprint_name",
			cause:    errMsgLabelValueReqs,
		}}
	}

	return nil
}

// productOfModuleUseMark is a "mark" applied to values that are result of `use`.
// Should not be used directly, use AsProductOfModuleUse and IsProductOfModuleUse instead.
type productOfModuleUseMark struct {
	mods string
}

// AsProductOfModuleUse marks a value as a result of `use` of given modules.
func AsProductOfModuleUse(v cty.Value, mods ...ModuleID) cty.Value {
	s := make([]string, len(mods))
	for i, m := range mods {
		s[i] = string(m)
	}
	sort.Strings(s)
	return v.Mark(productOfModuleUseMark{strings.Join(s, ",")})
}

// IsProductOfModuleUse returns list of modules that contributed (by `use`) to this value.
func IsProductOfModuleUse(v cty.Value) []ModuleID {
	mark, marked := HasMark[productOfModuleUseMark](v)
	if !marked {
		return []ModuleID{}
	}

	s := strings.Split(mark.mods, ",")
	mods := make([]ModuleID, len(s))
	for i, m := range s {
		mods[i] = ModuleID(m)
	}
	return mods
}

// WalkModules walks all modules in the blueprint and calls the walker function
func (bp *Blueprint) WalkModules(walker func(ModulePath, *Module) error) error {
	for ig := range bp.DeploymentGroups {
		g := &bp.DeploymentGroups[ig]
		for im := range g.Modules {
			p := Root.Groups.At(ig).Modules.At(im)
			m := &g.Modules[im]
			if err := walker(p, m); err != nil {
				return err
			}
		}
	}
	return nil
}

func (bp *Blueprint) WalkModulesSafe(walker func(ModulePath, *Module)) {
	bp.WalkModules(func(p ModulePath, m *Module) error {
		walker(p, m)
		return nil
	})
}

// validate every module setting in the blueprint containing a reference
func validateModuleSettingReferences(p ModulePath, m Module, bp Blueprint) error {
	errs := Errors{}
	for k, v := range m.Settings.Items() {
		for r, rp := range valueReferences(v) {
			errs.At(
				p.Settings.Dot(k).Cty(rp),
				validateModuleSettingReference(bp, m, r))
		}
	}
	return errs.OrNil()
}

func varsTopologicalOrder(vars Dict) ([]string, error) {
	// 0, 1, 2 - unvisited, on stack, exited
	used := map[string]int{} // default is 0 - unvisited
	res := []string{}

	// walk vars in reverse topological order
	var dfs func(string) error
	dfs = func(n string) error {
		used[n] = 1 // put on stack
		v := vars.Get(n)
		for ref, rp := range valueReferences(v) {
			p := Root.Vars.Dot(n).Cty(rp)

			if !ref.GlobalVar {
				return BpError{p, fmt.Errorf("non-global variable %q referenced in expression", ref.Name)}
			}

			if used[ref.Name] == 1 {
				return BpError{p, fmt.Errorf("cyclic dependency detected: %q -> %q", n, ref.Name)}
			}

			if used[ref.Name] == 0 {
				if err := dfs(ref.Name); err != nil {
					return err
				}
			}
		}
		used[n] = 2 // remove from stack and add to result
		res = append(res, n)
		return nil
	}

	for n := range vars.Items() {
		if used[n] == 0 { // unvisited
			if err := dfs(n); err != nil {
				return nil, err
			}
		}
	}
	return res, nil
}

func (bp *Blueprint) evalVars() (Dict, error) {
	order, err := varsTopologicalOrder(bp.Vars)
	if err != nil {
		return Dict{}, err
	}

	res := map[string]cty.Value{}
	ctx := hcl.EvalContext{
		Variables: map[string]cty.Value{},
		Functions: functions()}
	for _, n := range order {
		ctx.Variables["var"] = cty.ObjectVal(res)
		ev, err := eval(bp.Vars.Get(n), &ctx)
		if err != nil {
			return Dict{}, BpError{Root.Vars.Dot(n), err}
		}
		res[n] = ev
	}
	return NewDict(res), nil
}
