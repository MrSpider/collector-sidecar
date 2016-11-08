// This file is part of Graylog.
//
// Graylog is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// Graylog is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with Graylog.  If not, see <http://www.gnu.org/licenses/>.

package winlogbeat

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"os/exec"
	"path/filepath"
	"text/template"

	"gopkg.in/yaml.v2"

	"github.com/Graylog2/collector-sidecar/api/graylog"
	"github.com/Graylog2/collector-sidecar/backends"
	"github.com/Graylog2/collector-sidecar/backends/beats"
	"github.com/Graylog2/collector-sidecar/common"
)

func (wlbc *WinLogBeatConfig) snippetsToString() string {
	var buffer bytes.Buffer
	var result bytes.Buffer
	for _, snippet := range wlbc.Beats.Snippets {
		snippetTemplate, err := template.New("snippet").Parse(snippet.Value)
		if err != nil {
			result.WriteString(snippet.Value)
		} else {
			snippetTemplate.Execute(&buffer, wlbc.Beats.Context.Inventory)
			result.WriteString(buffer.String())
		}
		result.WriteString("\n")
	}
	return result.String()
}

func (wlbc *WinLogBeatConfig) Render() bytes.Buffer {
	var result bytes.Buffer

	if wlbc.Beats.Data() == nil {
		return result
	}

	beatsConfig := wlbc.Beats
	wlbc.runMigrations(beatsConfig)
	result.WriteString(beatsConfig.String())
	result.WriteString(wlbc.snippetsToString())

	return result
}

func (wlbc *WinLogBeatConfig) RenderToFile() error {
	stringConfig := wlbc.Render()
	err := common.CreatePathToFile(wlbc.Beats.UserConfig.ConfigurationPath)
	if err != nil {
		return err
	}
	err = ioutil.WriteFile(wlbc.Beats.UserConfig.ConfigurationPath, stringConfig.Bytes(), 0644)
	return err
}

func (wlbc *WinLogBeatConfig) RenderOnChange(response graylog.ResponseCollectorConfiguration) bool {
	newConfig := NewCollectorConfig(wlbc.Beats.Context)

	// holds event inputs
	var eventlogs []interface{}

	newConfig.Beats.Set(wlbc.Beats.Context.UserConfig.Tags, "shipper", "tags")

	for _, output := range response.Outputs {
		if output.Backend == "winlogbeat" {
			for property, value := range output.Properties {
				// ignore tls properties
				if property == "tls" ||
					property == "ca_file" ||
					property == "cert_file" ||
					property == "cert_key_file" ||
					property == "tls_insecure" {
					continue
				}
				newConfig.Beats.Set(value, "output", output.Type, property)
			}
			if wlbc.Beats.PropertyBool(output.Properties["tls"]) {
				if wlbc.Beats.PropertyBool(output.Properties["ca_file"]) {
					newConfig.Beats.Set([]string{wlbc.Beats.PropertyString(output.Properties["ca_file"], 0)}, "output", "logstash", "tls", "certificate_authorities")
				}
				if wlbc.Beats.PropertyBool(output.Properties["cert_file"]) {
					newConfig.Beats.Set(output.Properties["cert_file"], "output", "logstash", "tls", "certificate")
				}
				if wlbc.Beats.PropertyBool(output.Properties["cert_key_file"]) {
					newConfig.Beats.Set(output.Properties["cert_key_file"], "output", "logstash", "tls", "certificate_key")
				}
				if wlbc.Beats.PropertyBool(output.Properties["tls_insecure"]) {
					newConfig.Beats.Set(wlbc.Beats.PropertyBool(output.Properties["tls_insecure"]), "output", "logstash", "tls", "insecure")
				}
			}
		}
	}

	for _, input := range response.Inputs {
		if input.Backend == "winlogbeat" {
			for _, value := range input.Properties {
				var vt []interface{}
				err := yaml.Unmarshal([]byte(wlbc.Beats.PropertyString(value, 0)), &vt)
				if err != nil {
					msg := fmt.Sprintf("Nested YAML is not parsable: '%s'", value)
					wlbc.SetStatus(backends.StatusError, msg)
					log.Errorf("[%s] %s", wlbc.Name(), msg)
					return false
				} else {
					for _, name := range vt {
						eventlogs = append(eventlogs, name)
					}
				}
			}
		}
	}
	newConfig.Beats.Set(eventlogs, "winlogbeat", "event_logs")

	for _, snippet := range response.Snippets {
		if snippet.Backend == "winlogbeat" {
			newConfig.Beats.AppendString(snippet.Id, snippet.Value)
		}
	}

	// gl2_source_collector can't be set with Winlogbeat so we use the shipper name
	if wlbc.Beats.Version[0] >= 5 {
		newConfig.Beats.Set(wlbc.Beats.Context.CollectorId, "name")
	}

	if !wlbc.Beats.Equals(newConfig.Beats) {
		log.Infof("[%s] Configuration change detected, rewriting configuration file.", wlbc.Name())
		wlbc.Beats.Update(newConfig.Beats)
		wlbc.RenderToFile()
		return true
	}

	return false
}

func (wlbc *WinLogBeatConfig) ValidateConfigurationFile() bool {
	output, err := exec.Command(wlbc.ExecPath(), "-configtest", "-c", wlbc.ConfigurationPath()).CombinedOutput()
	soutput := string(output)
	if err != nil {
		log.Errorf("[%s] Error during configuration validation: %s", wlbc.Name(), soutput)
		return false
	}

	return true
}

func (wlbc *WinLogBeatConfig) runMigrations(bc *beats.BeatsConfig) {
	if wlbc.Beats.Version[0] == 5 && wlbc.Beats.Version[1] == 0 {
		// rename ssl properties
		cp := bc.Container
		configurationPath := []string{"output", "logstash", "tls", "certificate_key"}
		for target := 0; target < len(configurationPath); target++ {
			if mmap, ok := cp.(map[string]interface{}); ok {
				if target == len(configurationPath)-1 {
					if mmap["certificate_key"] != nil {
						mmap["key"] = mmap["certificate_key"]
						delete(mmap, "certificate_key")
					}
					if mmap["insecure"] == true {
						mmap["verification_mode"] = "none"
						delete(mmap, "insecure")
					}
				}
				cp = mmap[configurationPath[target]]
			}
		}

		// rename tls -> ssl
		cp = bc.Container
		configurationPath = []string{"output", "logstash", "tls"}
		for target := 0; target < len(configurationPath); target++ {
			if mmap, ok := cp.(map[string]interface{}); ok {
				if target == len(configurationPath)-1 {
					if mmap["tls"] != nil {
						mmap["ssl"] = mmap["tls"]
						delete(mmap, "tls")
					}
				}
				cp = mmap[configurationPath[target]]
			}
		}

		// remove shipper
		cp = bc.Container
		configurationPath = []string{"shipper", "tags"}
		for target := 0; target < len(configurationPath); target++ {
			if mmap, ok := cp.(map[string]interface{}); ok {
				if target == len(configurationPath)-1 {
					bc.Set(mmap["tags"], "tags")
					delete(bc.Container.(map[string]interface{}), "shipper")
				}
				cp = mmap[configurationPath[target]]
			}
		}

		// set cache data path
		dataPath := wlbc.Beats.UserConfig.RunPath
		if dataPath == "" {
			dataPath = filepath.Join(filepath.Dir(wlbc.Beats.UserConfig.BinaryPath), "data")
		}
		bc.Set(dataPath, "path", "data")

		// configure log path
		bc.Set(wlbc.Beats.Context.UserConfig.LogPath, "path", "logs")
	}
}
