/*
Copyright 2022 Google LLC

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    https://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package cmd defines command line utilities for ghpc
package cmd

import (
	"errors"
	"fmt"
	"hpc-toolkit/pkg/config"
	"hpc-toolkit/pkg/logging"
	"hpc-toolkit/pkg/modulewriter"
	"hpc-toolkit/pkg/validators"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"github.com/zclconf/go-cty/cty"
	"gopkg.in/yaml.v3"
)

const msgCLIVars = "Comma-separated list of name=value variables to override YAML configuration. Can be used multiple times."
const msgCLIBackendConfig = "Comma-separated list of name=value variables to set Terraform backend configuration. Can be used multiple times."

func init() {
	createCmd.Flags().StringVarP(&bpFilenameDeprecated, "config", "c", "", "")
	cobra.CheckErr(createCmd.Flags().MarkDeprecated("config",
		"please see the command usage for more details."))

	deploymentFileFlag := "deployment-file"
	createCmd.Flags().StringVarP(&deploymentFile, deploymentFileFlag, "d", "",
		"Toolkit Deployment File.")
	createCmd.Flags().MarkHidden(deploymentFileFlag)
	createCmd.MarkFlagFilename(deploymentFileFlag, "yaml", "yml")
	createCmd.Flags().StringVarP(&outputDir, "out", "o", "",
		"Sets the output directory where the HPC deployment directory will be created.")
	createCmd.Flags().StringSliceVar(&cliVariables, "vars", nil, msgCLIVars)
	createCmd.Flags().StringSliceVar(&cliBEConfigVars, "backend-config", nil, msgCLIBackendConfig)
	createCmd.Flags().StringVarP(&validationLevel, "validation-level", "l", "WARNING", validationLevelDesc)
	createCmd.Flags().StringSliceVar(&validatorsToSkip, "skip-validators", nil, skipValidatorsDesc)
	createCmd.Flags().BoolVarP(&overwriteDeployment, "overwrite-deployment", "w", false,
		"If specified, an existing deployment directory is overwritten by the new deployment. \n"+
			"Note: Terraform state IS preserved. \n"+
			"Note: Terraform workspaces are NOT supported (behavior undefined). \n"+
			"Note: Packer is NOT supported.")
	createCmd.Flags().BoolVar(&forceOverwrite, "force", false,
		"Forces overwrite of existing deployment directory. \n"+
			"If set, --overwrite-deployment is implied. \n"+
			"No validation is performed on the existing deployment directory.")
	rootCmd.AddCommand(createCmd)
}

var (
	bpFilenameDeprecated string
	deploymentFile       string
	outputDir            string
	cliVariables         []string

	cliBEConfigVars     []string
	overwriteDeployment bool
	forceOverwrite      bool
	validationLevel     string
	validationLevelDesc = "Set validation level to one of (\"ERROR\", \"WARNING\", \"IGNORE\")"
	validatorsToSkip    []string
	skipValidatorsDesc  = "Validators to skip"

	createCmd = &cobra.Command{
		Use:               "create BLUEPRINT_NAME",
		Short:             "Create a new deployment.",
		Long:              "Create a new deployment based on a provided blueprint.",
		Run:               runCreateCmd,
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: filterYaml,
	}
)

func runCreateCmd(cmd *cobra.Command, args []string) {
	bp := expandOrDie(args[0], deploymentFile)
	deplDir := filepath.Join(outputDir, bp.DeploymentName())
	checkErr(checkOverwriteAllowed(deplDir, bp, overwriteDeployment, forceOverwrite))
	checkErr(modulewriter.WriteDeployment(bp, deplDir))

	logging.Info("To deploy your infrastructure please run:")
	logging.Info("")
	logging.Info(boldGreen("%s deploy %s"), execPath(), deplDir)
	logging.Info("")
	printAdvancedInstructionsMessage(deplDir)
}

func printAdvancedInstructionsMessage(deplDir string) {
	logging.Info("Find instructions for cleanly destroying infrastructure and advanced manual")
	logging.Info("deployment instructions at:")
	logging.Info("")
	logging.Info(modulewriter.InstructionsPath(deplDir))
}

func expandOrDie(path string, dPath string) config.Blueprint {
	bp, ctx, err := config.NewBlueprint(path)
	if err != nil {
		logging.Fatal(renderError(err, ctx))
	}

	var ds config.DeploymentSettings
	var dCtx config.YamlCtx
	if dPath != "" {
		ds, dCtx, err = config.NewDeploymentSettings(dPath)
		if err != nil {
			logging.Fatal(renderError(err, dCtx))
		}
	}
	if err := setCLIVariables(&ds, cliVariables); err != nil {
		logging.Fatal("Failed to set the variables at CLI: %v", err)
	}
	if err := setBackendConfig(&ds, cliBEConfigVars); err != nil {
		logging.Fatal("Failed to set the backend config at CLI: %v", err)
	}

	mergeDeploymentSettings(&bp, ds)

	checkErr(setValidationLevel(&bp, validationLevel))
	skipValidators(&bp)

	if bp.GhpcVersion != "" {
		logging.Info("ghpc_version setting is ignored.")
	}
	bp.GhpcVersion = GitCommitInfo

	// Expand the blueprint
	if err := bp.Expand(); err != nil {
		logging.Fatal(renderError(err, ctx))
	}

	validateMaybeDie(bp, ctx)
	return bp
}

func validateMaybeDie(bp config.Blueprint, ctx config.YamlCtx) {
	err := validators.Execute(bp)
	if err == nil {
		return
	}
	logging.Error(renderError(err, ctx))

	logging.Error("One or more blueprint validators has failed. See messages above for suggested")
	logging.Error("actions. General troubleshooting guidance and instructions for configuring")
	logging.Error("validators are shown below.")
	logging.Error("")
	logging.Error("- https://goo.gle/hpc-toolkit-troubleshooting")
	logging.Error("- https://goo.gle/hpc-toolkit-validation")
	logging.Error("")
	logging.Error("Validators can be silenced or treated as warnings or errors:")
	logging.Error("")
	logging.Error("- https://goo.gle/hpc-toolkit-validation-levels")
	logging.Error("")

	switch bp.ValidationLevel {
	case config.ValidationWarning:
		{
			logging.Error(boldYellow("Validation failures were treated as a warning, continuing to create blueprint."))
			logging.Error("")
		}
	case config.ValidationError:
		{
			logging.Fatal(boldRed("validation failed due to the issues listed above"))
		}
	}

}

func setCLIVariables(ds *config.DeploymentSettings, s []string) error {
	for _, cliVar := range s {
		arr := strings.SplitN(cliVar, "=", 2)

		if len(arr) != 2 {
			return fmt.Errorf("invalid format: '%s' should follow the 'name=value' format", cliVar)
		}
		// Convert the variable's string literal to its equivalent default type.
		key := arr[0]
		var v config.YamlValue
		if err := yaml.Unmarshal([]byte(arr[1]), &v); err != nil {
			return fmt.Errorf("invalid input: unable to convert '%s' value '%s' to known type", key, arr[1])
		}
		ds.Vars.Set(key, v.Unwrap())
	}
	return nil
}

func setBackendConfig(ds *config.DeploymentSettings, s []string) error {
	if len(s) == 0 {
		return nil // no op
	}
	be := config.TerraformBackend{Type: "gcs"}
	for _, config := range s {
		arr := strings.SplitN(config, "=", 2)

		if len(arr) != 2 {
			return fmt.Errorf("invalid format: '%s' should follow the 'name=value' format", config)
		}

		key, value := arr[0], arr[1]
		switch key {
		case "type":
			be.Type = value
		default:
			be.Configuration.Set(key, cty.StringVal(value))
		}
	}
	ds.TerraformBackendDefaults = be
	return nil
}

func mergeDeploymentSettings(bp *config.Blueprint, ds config.DeploymentSettings) error {
	for k, v := range ds.Vars.Items() {
		bp.Vars.Set(k, v)
	}
	if ds.TerraformBackendDefaults.Type != "" {
		bp.TerraformBackendDefaults = ds.TerraformBackendDefaults
	}
	return nil
}

// SetValidationLevel allows command-line tools to set the validation level
func setValidationLevel(bp *config.Blueprint, s string) error {
	switch s {
	case "ERROR":
		bp.ValidationLevel = config.ValidationError
	case "WARNING":
		bp.ValidationLevel = config.ValidationWarning
	case "IGNORE":
		bp.ValidationLevel = config.ValidationIgnore
	default:
		return errors.New("invalid validation level (\"ERROR\", \"WARNING\", \"IGNORE\")")
	}
	return nil
}

func skipValidators(bp *config.Blueprint) {
	for _, v := range validatorsToSkip {
		bp.SkipValidator(v)
	}
}

func filterYaml(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) != 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	return []string{"yaml", "yml"}, cobra.ShellCompDirectiveFilterFileExt
}

func forceErr(err error) error {
	return config.HintError{
		Err:  err,
		Hint: "Use `--force` to overwrite the deployment anyway. Proceed at your own risk."}
}

// Determines if overwrite is allowed
func checkOverwriteAllowed(depDir string, bp config.Blueprint, overwriteFlag bool, forceFlag bool) error {
	if _, err := os.Stat(depDir); os.IsNotExist(err) || forceFlag {
		return nil // all good, no previous deployment
	}

	if _, err := os.Stat(modulewriter.HiddenGhpcDir(depDir)); os.IsNotExist(err) {
		// hidden ghpc dir does not exist
		return forceErr(fmt.Errorf("folder %q already exists, and it is not a valid GHPC deployment folder", depDir))
	}

	// try to get previous deployment
	expPath := filepath.Join(modulewriter.ArtifactsDir(depDir), modulewriter.ExpandedBlueprintName)
	if _, err := os.Stat(expPath); os.IsNotExist(err) {
		return forceErr(fmt.Errorf("expanded blueprint file %q is missing, this could be a result of changing GHPC version between consecutive deployments", expPath))
	}
	prev, _, err := config.NewBlueprint(expPath)
	if err != nil {
		return forceErr(err)
	}

	if prev.GhpcVersion != bp.GhpcVersion {
		return forceErr(fmt.Errorf(
			"ghpc_version has changed from %q to %q, using different versions of GHPC to update a live deployment is not officially supported",
			prev.GhpcVersion, bp.GhpcVersion))
	}

	if !overwriteFlag {
		return fmt.Errorf("deployment folder %q already exists, use -w to overwrite", depDir)
	}

	newGroups := map[config.GroupName]bool{}
	for _, g := range bp.DeploymentGroups {
		newGroups[g.Name] = true
	}

	for _, g := range prev.DeploymentGroups {
		if !newGroups[g.Name] {
			return forceErr(fmt.Errorf("you are attempting to remove a deployment group %q, which is not supported", g.Name))
		}
	}

	return nil
}
