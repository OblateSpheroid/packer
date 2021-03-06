package command

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	texttemplate "text/template"

	"github.com/hashicorp/hcl/v2/hclwrite"
	hcl2shim "github.com/hashicorp/packer/hcl2template/shim"
	"github.com/hashicorp/packer/template"
	"github.com/posener/complete"
	"github.com/zclconf/go-cty/cty"
)

type HCL2UpgradeCommand struct {
	Meta
}

func (c *HCL2UpgradeCommand) Run(args []string) int {
	ctx, cleanup := handleTermInterrupt(c.Ui)
	defer cleanup()

	cfg, ret := c.ParseArgs(args)
	if ret != 0 {
		return ret
	}

	return c.RunContext(ctx, cfg)
}

func (c *HCL2UpgradeCommand) ParseArgs(args []string) (*HCL2UpgradeArgs, int) {
	var cfg HCL2UpgradeArgs
	flags := c.Meta.FlagSet("hcl2_upgrade", FlagSetNone)
	flags.Usage = func() { c.Ui.Say(c.Help()) }
	cfg.AddFlagSets(flags)
	if err := flags.Parse(args); err != nil {
		return &cfg, 1
	}
	args = flags.Args()
	if len(args) != 1 {
		flags.Usage()
		return &cfg, 1
	}
	cfg.Path = args[0]
	if cfg.OutputFile == "" {
		cfg.OutputFile = cfg.Path + ".pkr.hcl"
	}
	return &cfg, 0
}

const (
	hcl2UpgradeFileHeader = `# This file was autogenerate by the BETA 'packer hcl2_upgrade' command. We
# recommend double checking that everything is correct before going forward. We
# also recommend treating this file as disposable. The HCL2 blocks in this
# file can be moved to other files. For example, the variable blocks could be
# moved to their own 'variables.pkr.hcl' file, etc. Those files need to be
# suffixed with '.pkr.hcl' to be visible to Packer. To use multiple files at
# once they also need to be in the same folder. 'packer inspect folder/'
# will describe to you what is in that folder.

# All generated input variables will be of string type as this how Packer JSON
# views them; you can later on change their type. Read the variables type
# constraints documentation
# https://www.packer.io/docs/from-1.5/variables#type-constraints for more info.
`

	sourcesHeader = `
# source blocks are generated from your builders; a source can be referenced in
# build blocks. A build block runs provisioner and post-processors onto a
# source. Read the documentation for source blocks here:
# https://www.packer.io/docs/from-1.5/blocks/source`

	buildHeader = `
# a build block invokes sources and runs provisionning steps on them. The
# documentation for build blocks can be found here:
# https://www.packer.io/docs/from-1.5/blocks/build
build {
`
)

func (c *HCL2UpgradeCommand) RunContext(buildCtx context.Context, cla *HCL2UpgradeArgs) int {

	out := &bytes.Buffer{}
	var output io.Writer
	if err := os.MkdirAll(filepath.Dir(cla.OutputFile), 0); err != nil {
		c.Ui.Error(fmt.Sprintf("Failed to create output directory: %v", err))
		return 1
	}
	if f, err := os.Create(cla.OutputFile); err == nil {
		output = f
		defer f.Close()
	} else {
		c.Ui.Error(fmt.Sprintf("Failed to create output file: %v", err))
		return 1
	}

	if _, err := output.Write([]byte(hcl2UpgradeFileHeader)); err != nil {
		c.Ui.Error(fmt.Sprintf("Failed to write to file: %v", err))
		return 1
	}

	hdl, ret := c.GetConfigFromJSON(&cla.MetaArgs)
	if ret != 0 {
		return ret
	}

	core := hdl.(*CoreWrapper).Core
	if err := core.Initialize(); err != nil {
		c.Ui.Error(fmt.Sprintf("Initialization error, continuing: %v", err))
	}
	tpl := core.Template

	// Output variables section

	variables := []*template.Variable{}
	{
		// sort variables to avoid map's randomness

		for _, variable := range tpl.Variables {
			variables = append(variables, variable)
		}
		sort.Slice(variables, func(i, j int) bool {
			return variables[i].Key < variables[j].Key
		})
	}

	for _, variable := range variables {
		variablesContent := hclwrite.NewEmptyFile()
		variablesBody := variablesContent.Body()

		variableBody := variablesBody.AppendNewBlock("variable", []string{variable.Key}).Body()
		variableBody.SetAttributeRaw("type", hclwrite.Tokens{&hclwrite.Token{Bytes: []byte("string")}})

		if variable.Default != "" || !variable.Required {
			variableBody.SetAttributeValue("default", hcl2shim.HCL2ValueFromConfigValue(variable.Default))
		}
		if isSensitiveVariable(variable.Key, tpl.SensitiveVariables) {
			variableBody.SetAttributeValue("sensitive", cty.BoolVal(true))
		}
		variablesBody.AppendNewline()
		out.Write(transposeTemplatingCalls(variablesContent.Bytes()))
	}

	fmt.Fprintln(out, `# "timestamp" template function replacement`)
	fmt.Fprintln(out, `locals { timestamp = regex_replace(timestamp(), "[- TZ:]", "") }`)

	// Output sources section

	builders := []*template.Builder{}
	{
		// sort builders to avoid map's randomnes
		for _, builder := range tpl.Builders {
			builders = append(builders, builder)
		}
		sort.Slice(builders, func(i, j int) bool {
			return builders[i].Type+builders[i].Name < builders[j].Type+builders[j].Name
		})
	}

	out.Write([]byte(sourcesHeader))

	for i, builderCfg := range builders {
		sourcesContent := hclwrite.NewEmptyFile()
		body := sourcesContent.Body()

		body.AppendNewline()
		if !c.Meta.CoreConfig.Components.BuilderStore.Has(builderCfg.Type) {
			c.Ui.Error(fmt.Sprintf("unknown builder type: %q\n", builderCfg.Type))
			return 1
		}
		if builderCfg.Name == "" || builderCfg.Name == builderCfg.Type {
			builderCfg.Name = fmt.Sprintf("autogenerated_%d", i+1)
		}
		sourceBody := body.AppendNewBlock("source", []string{builderCfg.Type, builderCfg.Name}).Body()

		jsonBodyToHCL2Body(sourceBody, builderCfg.Config)

		_, _ = out.Write(transposeTemplatingCalls(sourcesContent.Bytes()))
	}

	// Output build section
	out.Write([]byte(buildHeader))

	buildContent := hclwrite.NewEmptyFile()
	buildBody := buildContent.Body()
	if tpl.Description != "" {
		buildBody.SetAttributeValue("description", cty.StringVal(tpl.Description))
		buildBody.AppendNewline()
	}

	sourceNames := []string{}
	for _, builder := range builders {
		sourceNames = append(sourceNames, fmt.Sprintf("source.%s.%s", builder.Type, builder.Name))
	}
	buildBody.SetAttributeValue("sources", hcl2shim.HCL2ValueFromConfigValue(sourceNames))
	buildBody.AppendNewline()
	_, _ = buildContent.WriteTo(out)

	for _, provisioner := range tpl.Provisioners {
		provisionerContent := hclwrite.NewEmptyFile()
		body := provisionerContent.Body()

		buildBody.AppendNewline()
		block := body.AppendNewBlock("provisioner", []string{provisioner.Type})
		cfg := provisioner.Config
		if len(provisioner.Except) > 0 {
			cfg["except"] = provisioner.Except
		}
		if len(provisioner.Only) > 0 {
			cfg["only"] = provisioner.Only
		}
		if provisioner.MaxRetries != "" {
			cfg["max_retries"] = provisioner.MaxRetries
		}
		if provisioner.Timeout > 0 {
			cfg["timeout"] = provisioner.Timeout.String()
		}
		jsonBodyToHCL2Body(block.Body(), cfg)

		out.Write(transposeTemplatingCalls(provisionerContent.Bytes()))
	}
	for _, pps := range tpl.PostProcessors {
		postProcessorContent := hclwrite.NewEmptyFile()
		body := postProcessorContent.Body()

		switch len(pps) {
		case 0:
			continue
		case 1:
		default:
			body = body.AppendNewBlock("post-processors", nil).Body()
		}
		for _, pp := range pps {
			ppBody := body.AppendNewBlock("post-processor", []string{pp.Type}).Body()
			if pp.KeepInputArtifact != nil {
				ppBody.SetAttributeValue("keep_input_artifact", cty.BoolVal(*pp.KeepInputArtifact))
			}
			cfg := pp.Config
			if len(pp.Except) > 0 {
				cfg["except"] = pp.Except
			}
			if len(pp.Only) > 0 {
				cfg["only"] = pp.Only
			}
			if pp.Name != "" && pp.Name != pp.Type {
				cfg["name"] = pp.Name
			}
			jsonBodyToHCL2Body(ppBody, cfg)
		}

		_, _ = out.Write(transposeTemplatingCalls(postProcessorContent.Bytes()))
	}

	_, _ = out.Write([]byte("}\n"))

	_, _ = output.Write(hclwrite.Format(out.Bytes()))

	c.Ui.Say(fmt.Sprintf("Successfully created %s ", cla.OutputFile))

	return 0
}

// transposeTemplatingCalls executes parts of blocks as go template files and replaces
// their result with their hcl2 variant. If something goes wrong the template
// containing the go template string is returned.
func transposeTemplatingCalls(s []byte) []byte {
	fallbackReturn := func(err error) []byte {
		return append([]byte(fmt.Sprintf("\n#could not parse template for following block: %q\n", err)), s...)
	}
	funcMap := texttemplate.FuncMap{
		"timestamp": func() string {
			return "${local.timestamp}"
		},
		"isotime": func() string {
			return "${local.timestamp}"
		},
		"user": func(in string) string {
			return fmt.Sprintf("${var.%s}", in)
		},
		"env": func(in string) string {
			return fmt.Sprintf("${var.%s}", in)
		},
		"build": func(a string) string {
			return fmt.Sprintf("${build.%s}", a)
		},
	}

	tpl, err := texttemplate.New("generated").
		Funcs(funcMap).
		Parse(string(s))

	if err != nil {
		return fallbackReturn(err)
	}

	str := &bytes.Buffer{}
	v := struct {
		HTTPIP   string
		HTTPPort string
	}{
		HTTPIP:   "{{ .HTTPIP }}",
		HTTPPort: "{{ .HTTPPort }}",
	}
	if err := tpl.Execute(str, v); err != nil {
		return fallbackReturn(err)
	}

	return str.Bytes()
}

func jsonBodyToHCL2Body(out *hclwrite.Body, kvs map[string]interface{}) {
	ks := []string{}
	for k := range kvs {
		ks = append(ks, k)
	}
	sort.Strings(ks)

	for _, k := range ks {
		value := kvs[k]

		switch value := value.(type) {
		case map[string]interface{}:
			var mostComplexElem interface{}
			for _, randomElem := range value {
				// HACK: we take the most complex element of that map because
				// in HCL2, map of objects can be bodies, for example:
				// map containing object: source_ami_filter {} ( body )
				// simple string/string map: tags = {} ) ( attribute )
				//
				// if we could not find an object in this map then it's most
				// likely a plain map and so we guess it should be and
				// attribute. Though now if value refers to something that is
				// an object but only contains a string or a bool; we could
				// generate a faulty object. For example a (somewhat invalid)
				// source_ami_filter where only `most_recent` is set.
				switch randomElem.(type) {
				case string, int, float64, bool:
					if mostComplexElem != nil {
						continue
					}
					mostComplexElem = randomElem
				default:
					mostComplexElem = randomElem
				}
			}

			switch mostComplexElem.(type) {
			case string, int, float64, bool:
				out.SetAttributeValue(k, hcl2shim.HCL2ValueFromConfigValue(value))
			default:
				nestedBlockBody := out.AppendNewBlock(k, nil).Body()
				jsonBodyToHCL2Body(nestedBlockBody, value)
			}
		case map[string]string, map[string]int, map[string]float64:
			out.SetAttributeValue(k, hcl2shim.HCL2ValueFromConfigValue(value))
		case []interface{}:
			if len(value) == 0 {
				continue
			}

			var mostComplexElem interface{}
			for _, randomElem := range value {
				// HACK: we take the most complex element of that slice because
				// in hcl2 slices of plain types can be arrays, for example:
				// simple string type: owners = ["0000000000"]
				// object: launch_block_device_mappings {}
				switch randomElem.(type) {
				case string, int, float64, bool:
					if mostComplexElem != nil {
						continue
					}
					mostComplexElem = randomElem
				default:
					mostComplexElem = randomElem
				}
			}
			switch mostComplexElem.(type) {
			case map[string]interface{}:
				// this is an object in a slice; so we unwrap it. We
				// could try to remove any 's' suffix in the key, but
				// this might not work everywhere.
				for i := range value {
					value := value[i].(map[string]interface{})
					nestedBlockBody := out.AppendNewBlock(k, nil).Body()
					jsonBodyToHCL2Body(nestedBlockBody, value)
				}
				continue
			default:
				out.SetAttributeValue(k, hcl2shim.HCL2ValueFromConfigValue(value))
			}
		default:
			out.SetAttributeValue(k, hcl2shim.HCL2ValueFromConfigValue(value))
		}
	}
}

func isSensitiveVariable(key string, vars []*template.Variable) bool {
	for _, v := range vars {
		if v.Key == key {
			return true
		}
	}
	return false
}

func (*HCL2UpgradeCommand) Help() string {
	helpText := `
Usage: packer hcl2_upgrade -output-file=JSON_TEMPLATE.pkr.hcl JSON_TEMPLATE...

  Will transform your JSON template to a HCL2 configuration.
`

	return strings.TrimSpace(helpText)
}

func (*HCL2UpgradeCommand) Synopsis() string {
	return "transform a JSON template into a HCL2 configuration"
}

func (*HCL2UpgradeCommand) AutocompleteArgs() complete.Predictor {
	return complete.PredictNothing
}

func (*HCL2UpgradeCommand) AutocompleteFlags() complete.Flags {
	return complete.Flags{}
}
