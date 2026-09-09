package config

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// UpdateProviderEnabled changes only a provider's enabled field in the source
// YAML, preserving environment-variable references and unrelated comments.
func UpdateProviderEnabled(path, provider string, enabled bool) error {
	return updateYAML(path, func(root *yaml.Node) error {
		providers := mappingChild(root, "providers")
		if providers == nil {
			return fmt.Errorf("configuration has no providers mapping")
		}
		providerNode := mappingChild(providers, provider)
		if providerNode == nil {
			return fmt.Errorf("provider %q is not configured", provider)
		}
		setMappingChild(providerNode, "enabled", &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: fmt.Sprintf("%t", enabled)})
		return nil
	})
}

// UpdateRouting replaces the routing section in the source YAML.
func UpdateRouting(path string, routing RoutingConfig) error {
	return updateYAML(path, func(root *yaml.Node) error {
		var doc yaml.Node
		data, err := yaml.Marshal(routing)
		if err != nil {
			return err
		}
		if err := yaml.Unmarshal(data, &doc); err != nil {
			return err
		}
		setMappingChild(root, "routing", doc.Content[0])
		return nil
	})
}

// LoadRouting reads only the routing section from a YAML configuration file.
// Environment references are expanded exactly as they are during startup.
func LoadRouting(path string) (RoutingConfig, error) {
	if path == "" {
		return RoutingConfig{}, fmt.Errorf("no configuration file is active")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return RoutingConfig{}, fmt.Errorf("reading config file: %w", err)
	}
	var document struct {
		Routing RoutingConfig `yaml:"routing"`
	}
	if err := yaml.Unmarshal([]byte(expandEnv(string(data))), &document); err != nil {
		return RoutingConfig{}, fmt.Errorf("parsing config file: %w", err)
	}
	if document.Routing.Routes == nil {
		document.Routing.Routes = make(map[string]string)
	}
	if document.Routing.Fallbacks == nil {
		document.Routing.Fallbacks = make(map[string][]string)
	}
	return document.Routing, nil
}

func updateYAML(path string, mutate func(*yaml.Node) error) error {
	if path == "" {
		return fmt.Errorf("no configuration file is active")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading config file: %w", err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("parsing config file: %w", err)
	}
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return fmt.Errorf("configuration root must be a mapping")
	}
	if err := mutate(doc.Content[0]); err != nil {
		return err
	}
	out, err := yaml.Marshal(&doc)
	if err != nil {
		return fmt.Errorf("encoding config file: %w", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("reading config permissions: %w", err)
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".babelgate-config-*")
	if err != nil {
		return fmt.Errorf("creating temporary config: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(info.Mode().Perm()); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(out); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("replacing config file: %w", err)
	}
	return nil
}

func mappingChild(mapping *yaml.Node, key string) *yaml.Node {
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			return mapping.Content[i+1]
		}
	}
	return nil
}

func setMappingChild(mapping *yaml.Node, key string, value *yaml.Node) {
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			mapping.Content[i+1] = value
			return
		}
	}
	mapping.Content = append(mapping.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, value)
}
