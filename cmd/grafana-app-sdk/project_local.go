package main

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"text/template"

	"cuelang.org/go/cue/cuecontext"
	"github.com/grafana/thema"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/grafana/grafana-app-sdk/codegen"
)

//go:embed templates/local/* templates/local/scripts/* templates/local/generated/datasources/*
var localEnvFiles embed.FS

// localEnvConfig is the configuration object used for the generation of local dev env resources
type localEnvConfig struct {
	Port              int                `json:"port" yaml:"port"`
	KubePort          int                `json:"kubePort" yaml:"kubePort"`
	Datasources       []string           `json:"datasources" yaml:"datasources"`
	DatasourceConfigs []dataSourceConfig `json:"datasourceConfigs" yaml:"datasourceConfigs"`
	PluginJSON        map[string]any     `json:"pluginJson" yaml:"pluginJson"`
	PluginSecureJSON  map[string]any     `json:"pluginSecureJson" yaml:"pluginSecureJson"`
	OperatorImage     string             `json:"operatorImage" yaml:"operatorImage"`
}

type dataSourceConfig struct {
	Access    string `json:"access" yaml:"access"`
	Editable  bool   `json:"editable" yaml:"editable"`
	IsDefault bool   `json:"isDefault" yaml:"isDefault"`
	Name      string `json:"name" yaml:"name"`
	Type      string `json:"type" yaml:"type"`
	UID       string `json:"uid" yaml:"uid"`
	URL       string `json:"url" yaml:"url"`
}

func projectLocalEnvInit(cmd *cobra.Command, _ []string) error {
	// Path (optional)
	path, err := cmd.Flags().GetString("path")
	if err != nil {
		return err
	}
	modName, err := getGoModule(filepath.Join(path, "go.mod"))
	if err != nil {
		return err
	}
	return initializeLocalEnvFiles(path, modName, modName)
}

func initializeLocalEnvFiles(basePath, clusterName, operatorImageName string) error {
	localPath := filepath.Join(basePath, "local")

	// Write the default local config file
	cfgTemplate, err := template.ParseFS(localEnvFiles, "templates/local/config.yaml")
	if err != nil {
		return err
	}
	cfgBytes := bytes.Buffer{}
	err = cfgTemplate.Execute(&cfgBytes, map[string]string{
		"OperatorImage": operatorImageName,
	})
	if err != nil {
		return err
	}
	err = writeFile(filepath.Join(localPath, "config.yaml"), cfgBytes.Bytes())
	if err != nil {
		return err
	}

	// Write out all scripts
	scripts, err := localEnvFiles.ReadDir("templates/local/scripts")
	if err != nil {
		return err
	}
	for _, script := range scripts {
		scriptTemplate, err := template.ParseFS(localEnvFiles, filepath.Join("templates", "local", "scripts", script.Name()))
		if err != nil {
			return err
		}
		buf := bytes.Buffer{}
		err = scriptTemplate.Execute(&buf, map[string]string{
			"ClusterName": clusterName,
		})
		if err != nil {
			return err
		}
		err = writeExecutableFile(filepath.Join(localPath, "scripts", script.Name()), buf.Bytes())
		if err != nil {
			return err
		}
	}

	tiltfile, err := generateTiltfile()
	if err != nil {
		return err
	}
	err = writeFileWithOverwriteConfirm(filepath.Join(localPath, "Tiltfile"), tiltfile)
	if err != nil {
		return err
	}

	err = checkAndMakePath(filepath.Join(localPath, "additional"))
	if err != nil {
		return err
	}
	err = checkAndMakePath(filepath.Join(localPath, "mounted-files", "plugin"))
	if err != nil {
		return err
	}

	return nil
}

func projectLocalEnvGenerate(cmd *cobra.Command, _ []string) error {
	// Path (optional)
	path, err := cmd.Flags().GetString("path")
	if err != nil {
		return err
	}
	cuePath, err := cmd.Flags().GetString("cuepath")
	if err != nil {
		return err
	}
	localPath := filepath.Join(path, "local")
	localGenPath := filepath.Join(localPath, "generated")
	absPath, err := filepath.Abs(path)
	if err != nil {
		return err
	}

	// Get config
	config, err := getLocalEnvConfig(localPath)
	if err != nil {
		return err
	}

	// get plugin ID
	pluginID, err := getPluginID(path)
	if err != nil {
		return err
	}

	// Generate the k3d config (this has to be generated, as it needs to mount an absolute path on the host)
	k3dConfig, err := generateK3dConfig(absPath, *config)
	if err != nil {
		return err
	}
	err = writeFile(filepath.Join(localGenPath, "k3d-config.json"), k3dConfig)
	if err != nil {
		return err
	}

	// Generate the k8s YAML bundle
	parser, err := codegen.NewCustomKindParser(thema.NewRuntime(cuecontext.New()), os.DirFS(cuePath))
	if err != nil {
		return err
	}

	k8sYAML, err := generateKubernetesYAML(parser, pluginID, *config)
	if err != nil {
		return err
	}
	err = writeFile(filepath.Join(localGenPath, "dev-bundle.yaml"), k8sYAML)
	if err != nil {
		return err
	}

	return nil
}

func getLocalEnvConfig(localPath string) (*localEnvConfig, error) {
	// Read config (try YAML first, then JSON)
	config := localEnvConfig{}
	if _, err := os.Stat(filepath.Join(localPath, "config.yaml")); err == nil {
		cfgBytes, err := os.ReadFile(filepath.Join(localPath, "config.yaml"))
		if err != nil {
			return nil, err
		}
		err = yaml.Unmarshal(cfgBytes, &config)
		if err != nil {
			return nil, err
		}
	} else if _, err = os.Stat(filepath.Join(localPath, "config.json")); err == nil {
		cfgBytes, err := os.ReadFile(filepath.Join(localPath, "config.json"))
		if err != nil {
			return nil, err
		}
		err = json.Unmarshal(cfgBytes, &config)
		if err != nil {
			return nil, err
		}
	} else {
		return nil, fmt.Errorf("nether %s/config.yaml nor %s/config.json not found, please run `grafana-app-sdk project local init` to generate", localPath, localPath)
	}
	return &config, nil
}

func getPluginID(rootPath string) (string, error) {
	pluginJSONPath := filepath.Join(rootPath, "plugin", "src", "plugin.json")
	if _, err := os.Stat(pluginJSONPath); err != nil {
		return "", fmt.Errorf("could not locate file %s", pluginJSONPath)
	}
	pluginJSONFile, err := os.ReadFile(pluginJSONPath)
	if err != nil {
		return "", err
	}

	type pluginJSON struct {
		ID string `json:"id"`
	}
	um := pluginJSON{}
	err = json.Unmarshal(pluginJSONFile, &um)
	return um.ID, err
}

func generateK3dConfig(projectRoot string, config localEnvConfig) ([]byte, error) {
	k3dConfigTmpl, err := template.ParseFS(localEnvFiles, "templates/local/generated/k3d-config.json")
	if err != nil {
		return nil, err
	}
	buf := &bytes.Buffer{}
	err = k3dConfigTmpl.Execute(buf, map[string]string{
		"ProjectRoot": projectRoot,
		"BindPort":    strconv.Itoa(config.Port),
	})
	return buf.Bytes(), err
}

type yamlGenProperties struct {
	PluginID       string
	PluginIDKube   string
	CRDs           []yamlGenPropsCRD
	Services       []yamlGenPropsService
	JSONData       map[string]string
	SecureJSONData map[string]string
	Datasources    []dataSourceConfig
	OperatorImage  string
}

type yamlGenPropsCRD struct {
	MachineName       string
	PluralMachineName string
	Group             string
}

type yamlGenPropsService struct {
	KubeName string
}

type crdYAML struct {
	Spec struct {
		Group string `yaml:"group"`
		Names struct {
			Kind   string `yaml:"kind"`
			Plural string `yaml:"plural"`
		} `yaml:"names"`
	} `yaml:"spec"`
}

var kubeReplaceRegexp = regexp.MustCompile(`[^a-z0-9\-]`)

//nolint:funlen,errcheck,revive
func generateKubernetesYAML(parser *codegen.CustomKindParser, pluginID string, config localEnvConfig) ([]byte, error) {
	output := bytes.Buffer{}
	props := yamlGenProperties{
		PluginID:       pluginID,
		PluginIDKube:   kubeReplaceRegexp.ReplaceAllString(strings.ToLower(pluginID), "-"),
		CRDs:           make([]yamlGenPropsCRD, 0),
		Services:       make([]yamlGenPropsService, 0),
		Datasources:    make([]dataSourceConfig, 0),
		JSONData:       make(map[string]string),
		SecureJSONData: make(map[string]string),
		OperatorImage:  config.OperatorImage,
	}
	props.Services = append(props.Services, yamlGenPropsService{
		KubeName: "grafana",
	})
	if props.OperatorImage != "" {
		props.Services = append(props.Services, yamlGenPropsService{
			KubeName: "operator",
		})
	}
	if props.OperatorImage != "" {
		// Prefix with "localhost/" to ensure that our local build uses our locally-built image
		props.OperatorImage = fmt.Sprintf("localhost/%s", props.OperatorImage)
	}

	// Generate CRD YAML files, add the CRD metadata to the props
	crdFiles, err := generateCRDs(parser, "", "yaml", []string{})
	if err != nil {
		return nil, err
	}
	for _, f := range crdFiles {
		output.Write(append(f.Data, []byte("\n---\n")...))
		yml := crdYAML{}
		err = yaml.Unmarshal(f.Data, &yml)
		if err != nil {
			return nil, err
		}
		props.CRDs = append(props.CRDs, yamlGenPropsCRD{
			MachineName:       strings.ToLower(yml.Spec.Names.Kind),
			PluralMachineName: strings.ToLower(yml.Spec.Names.Plural),
			Group:             yml.Spec.Group,
		})
	}

	// RBAC for CRDs
	tmplRoles, err := template.ParseFS(localEnvFiles, "templates/local/generated/crd_roles.yaml")
	if err != nil {
		return nil, err
	}
	for _, c := range props.CRDs {
		err = tmplRoles.Execute(&output, c)
		if err != nil {
			return nil, err
		}
		output.Write([]byte("\n---\n"))
	}

	// Datasources
	for i, ds := range config.Datasources {
		err := localGenerateDatasourceYAML(ds, i == 0, &props, &output)
		if err != nil {
			return nil, err
		}
		output.WriteString("\n---\n")
	}
	if len(config.DatasourceConfigs) > 0 {
		props.Datasources = append(props.Datasources, config.DatasourceConfigs...)
	}

	// Grafana deployment
	err = localGenerateGrafanaYAML(config, &props, &output)

	// Operator deployment
	if config.OperatorImage != "" {
		output.WriteString("---\n")
		tmplOperator, err := template.ParseFS(localEnvFiles, "templates/local/generated/operator.yaml")
		if err != nil {
			return nil, err
		}
		err = tmplOperator.Execute(&output, props)
		if err != nil {
			return nil, err
		}
	}
	return output.Bytes(), err
}

//nolint:revive
func localGenerateDatasourceYAML(datasource string, isDefault bool, props *yamlGenProperties, out io.Writer) error {
	datasource = strings.ToLower(datasource)
	cfg, ok := localDatasourceConfigs[datasource]
	if !ok {
		return fmt.Errorf("unsupported datasource '%s'", datasource)
	}
	files, ok := localDatasourceFiles[datasource]
	if ok {
		for i, f := range files {
			if i > 0 {
				_, err := out.Write([]byte("\n---\n"))
				if err != nil {
					return err
				}
			}
			tmplDatasourceFile, err := template.ParseFS(localEnvFiles, f)
			if err != nil {
				return err
			}
			err = tmplDatasourceFile.Execute(out, props)
			if err != nil {
				return err
			}
		}
	}
	if isDefault {
		cfg.IsDefault = true
	}
	props.Datasources = append(props.Datasources, cfg)
	return nil
}

func localGenerateGrafanaYAML(config localEnvConfig, props *yamlGenProperties, out io.Writer) error {
	for k, v := range config.PluginJSON {
		val, err := parsePluginJSONValue(v)
		if err != nil {
			return fmt.Errorf("unable to parse pluginJson key '%s'", k)
		}
		props.JSONData[k] = val
	}
	config.PluginSecureJSON["kubeconfig"] = "cluster"
	config.PluginSecureJSON["kubenamespace"] = "default"
	for k, v := range config.PluginSecureJSON {
		val, err := parsePluginJSONValue(v)
		if err != nil {
			return fmt.Errorf("unable to parse pluginSecureJson key '%s'", k)
		}
		props.SecureJSONData[k] = val
	}

	tmplGrafana, err := template.ParseFS(localEnvFiles, "templates/local/generated/grafana.yaml")
	if err != nil {
		return err
	}
	err = tmplGrafana.Execute(out, props)
	if err != nil {
		return err
	}
	return nil
}

func parsePluginJSONValue(v any) (string, error) {
	switch cast := v.(type) {
	case map[string]interface{}, []interface{}:
		val, err := json.Marshal(v)
		if err != nil {
			return "", err
		}
		return string(val), nil
	case string:
		return cast, nil
	case int, int32, int64:
		return strconv.Itoa(v.(int)), nil
	case float32:
		return strconv.FormatFloat(float64(cast), 'E', -1, 32), nil
	case float64:
		return strconv.FormatFloat(cast, 'E', -1, 64), nil
	default:
		return "", fmt.Errorf("unknown type")
	}
}

func generateTiltfile() ([]byte, error) {
	buf := bytes.Buffer{}
	tmplGrafana, err := template.ParseFS(localEnvFiles, "templates/local/Tiltfile")
	if err != nil {
		return nil, err
	}
	err = tmplGrafana.Execute(&buf, struct{}{})
	if err != nil {
		return nil, err
	}
	return buf.Bytes(), err
}
