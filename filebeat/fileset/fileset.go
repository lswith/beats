/*
Package fileset contains the code that loads Filebeat modules (which are
composed of filesets).
*/

package fileset

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"text/template"

	"github.com/elastic/beats/libbeat/common"
)

// Fileset struct is the representation of a fileset.
type Fileset struct {
	name       string
	mcfg       *ModuleConfig
	fcfg       *FilesetConfig
	modulePath string
	manifest   *manifest
	vars       map[string]interface{}
}

// New allocates a new Fileset object with the given configuration.
func New(
	modulesPath string,
	name string,
	mcfg *ModuleConfig,
	fcfg *FilesetConfig) (*Fileset, error) {

	modulePath := filepath.Join(modulesPath, mcfg.Module)
	if _, err := os.Stat(modulePath); os.IsNotExist(err) {
		return nil, fmt.Errorf("Module %s (%s) doesn't exist.", mcfg.Module, modulePath)
	}

	return &Fileset{
		name:       name,
		mcfg:       mcfg,
		fcfg:       fcfg,
		modulePath: modulePath,
	}, nil
}

// Read reads the manifest file and evaluates the variables.
func (fs *Fileset) Read() error {
	var err error
	fs.manifest, err = fs.readManifest()
	if err != nil {
		return err
	}

	fs.vars, err = fs.evaluateVars()
	if err != nil {
		return err
	}

	pipeline_id, err := fs.getPipelineID()
	if err != nil {
		return err
	}
	fs.vars["beat"] = map[string]interface{}{
		"pipeline_id": pipeline_id,
	}

	return nil
}

// manifest structure is the representation of the manifest.yml file from the
// fileset.
type manifest struct {
	ModuleVersion  string                   `config:"module_version"`
	Vars           []map[string]interface{} `config:"var"`
	IngestPipeline string                   `config:"ingest_pipeline"`
	Prospector     string                   `config:"prospector"`
}

// readManifest reads the manifest file of the fileset.
func (fs *Fileset) readManifest() (*manifest, error) {
	cfg, err := common.LoadFile(filepath.Join(fs.modulePath, fs.name, "manifest.yml"))
	if err != nil {
		return nil, fmt.Errorf("Error reading manifest file: %v", err)
	}
	var manifest manifest
	err = cfg.Unpack(&manifest)
	if err != nil {
		return nil, fmt.Errorf("Error unpacking manifest: %v", err)
	}
	return &manifest, nil
}

// evaluateVars resolves the fileset variables.
func (fs *Fileset) evaluateVars() (map[string]interface{}, error) {
	var err error
	vars := map[string]interface{}{}
	vars["builtin"], err = fs.getBuiltinVars()
	if err != nil {
		return nil, err
	}

	for _, vals := range fs.manifest.Vars {
		var exists bool
		name, exists := vals["name"].(string)
		if !exists {
			return nil, fmt.Errorf("Variable doesn't have a string 'name' key")
		}

		value, exists := vals["default"]
		if !exists {
			return nil, fmt.Errorf("Variable %s doesn't have a 'default' key", name)
		}

		// evaluate OS specific vars
		osVals, exists := vals["os"].(map[string]interface{})
		if exists {
			osVal, exists := osVals[runtime.GOOS]
			if exists {
				value = osVal
			}
		}

		vars[name], err = resolveVariable(vars, value)
		if err != nil {
			return nil, fmt.Errorf("Error resolving variables on %s: %v", name, err)
		}
	}

	// overrides from the config
	for name, val := range fs.fcfg.Var {
		vars[name] = val
	}

	return vars, nil
}

// resolveVariable considers the value as a template so it can refer to built-in variables
// as well as other variables defined before them.
func resolveVariable(vars map[string]interface{}, value interface{}) (interface{}, error) {
	switch v := value.(type) {
	case string:
		return applyTemplate(vars, v)
	case []interface{}:
		transformed := []interface{}{}
		for _, val := range v {
			s, ok := val.(string)
			if ok {
				transf, err := applyTemplate(vars, s)
				if err != nil {
					return nil, fmt.Errorf("array: %v", err)
				}
				transformed = append(transformed, transf)
			} else {
				transformed = append(transformed, val)
			}
		}
		return transformed, nil
	}
	return value, nil
}

// applyTemplate applies a Golang text/template
func applyTemplate(vars map[string]interface{}, templateString string) (string, error) {
	tpl, err := template.New("text").Parse(templateString)
	if err != nil {
		return "", fmt.Errorf("Error parsing template %s: %v", templateString, err)
	}
	buf := bytes.NewBufferString("")
	err = tpl.Execute(buf, vars)
	if err != nil {
		return "", err
	}
	return buf.String(), nil
}

// getBuiltinVars computes the supported built in variables and groups them
// in a dictionary
func (fs *Fileset) getBuiltinVars() (map[string]interface{}, error) {
	host, err := os.Hostname()
	if err != nil || len(host) == 0 {
		return nil, fmt.Errorf("Error getting the hostname: %v", err)
	}
	split := strings.SplitN(host, ".", 2)
	hostname := split[0]
	domain := ""
	if len(split) > 1 {
		domain = split[1]
	}

	return map[string]interface{}{
		"hostname": hostname,
		"domain":   domain,
	}, nil
}

func (fs *Fileset) getProspectorConfig() (*common.Config, error) {
	path, err := applyTemplate(fs.vars, fs.manifest.Prospector)
	if err != nil {
		return nil, fmt.Errorf("Error expanding vars on the prospector path: %v", err)
	}
	contents, err := ioutil.ReadFile(filepath.Join(fs.modulePath, fs.name, path))
	if err != nil {
		return nil, fmt.Errorf("Error reading prospector file %s: %v", path, err)
	}

	yaml, err := applyTemplate(fs.vars, string(contents))
	if err != nil {
		return nil, fmt.Errorf("Error interpreting the template of the prospector: %v", err)
	}

	cfg, err := common.NewConfigWithYAML([]byte(yaml), "")
	if err != nil {
		return nil, fmt.Errorf("Error reading prospector config: %v", err)
	}

	// overrides
	if len(fs.fcfg.Prospector) > 0 {
		overrides, err := common.NewConfigFrom(fs.fcfg.Prospector)
		if err != nil {
			return nil, fmt.Errorf("Error creating config from prospector overrides: %v", err)
		}
		cfg, err = common.MergeConfigs(cfg, overrides)
		if err != nil {
			return nil, fmt.Errorf("Error applying config overrides: %v", err)
		}
	}

	cfg.PrintDebugf("Merged prospector config for fileset %s/%s", fs.mcfg.Module, fs.name)

	return cfg, nil
}

// getPipelineID returns the Ingest Node pipeline ID
func (fs *Fileset) getPipelineID() (string, error) {
	path, err := applyTemplate(fs.vars, fs.manifest.IngestPipeline)
	if err != nil {
		return "", fmt.Errorf("Error expanding vars on the ingest pipeline path: %v", err)
	}

	return formatPipelineID(fs.mcfg.Module, fs.name, path), nil
}

func (fs *Fileset) GetPipeline() (pipelineID string, content map[string]interface{}, err error) {
	path, err := applyTemplate(fs.vars, fs.manifest.IngestPipeline)
	if err != nil {
		return "", nil, fmt.Errorf("Error expanding vars on the ingest pipeline path: %v", err)
	}

	f, err := os.Open(filepath.Join(fs.modulePath, fs.name, path))
	if err != nil {
		return "", nil, fmt.Errorf("Error reading pipeline file %s: %v", path, err)
	}

	dec := json.NewDecoder(f)
	err = dec.Decode(&content)
	if err != nil {
		return "", nil, fmt.Errorf("Error JSON decoding the pipeline file: %s: %v", path, err)
	}
	return formatPipelineID(fs.mcfg.Module, fs.name, path), content, nil
}

// formatPipelineID generates the ID to be used for the pipeline ID in Elasticsearch
func formatPipelineID(module, fileset, path string) string {
	return fmt.Sprintf("%s-%s-%s", module, fileset, removeExt(filepath.Base(path)))
}

// removeExt returns the file name without the extension. If no dot is found,
// returns the same as the input.
func removeExt(path string) string {
	for i := len(path) - 1; i >= 0 && !os.IsPathSeparator(path[i]); i-- {
		if path[i] == '.' {
			return path[:i]
		}
	}
	return path
}
