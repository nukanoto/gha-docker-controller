package config

import (
	"fmt"
	"reflect"
	"strings"

	"github.com/moby/moby/api/types/container"
	"gopkg.in/yaml.v3"
)

// hostConfigYAML normalizes case-insensitive HostConfig keys before decoding.
type hostConfigYAML struct {
	container.HostConfig
}

func (h *hostConfigYAML) UnmarshalYAML(node *yaml.Node) error {
	normalized, err := normalizeHostConfigNode(node, reflect.TypeOf(container.HostConfig{}))
	if err != nil {
		return err
	}
	if err := normalized.Decode(&h.HostConfig); err != nil {
		return fmt.Errorf("invalid HostConfig value: %w", err)
	}
	return nil
}

type hostConfigField struct {
	decodePath []string
	typ        reflect.Type
}

func normalizeHostConfigNode(node *yaml.Node, typ reflect.Type) (*yaml.Node, error) {
	if node == nil {
		return nil, fmt.Errorf("hostConfig must not be null")
	}
	if node.Kind == yaml.DocumentNode {
		if len(node.Content) != 1 {
			return nil, fmt.Errorf("hostConfig must contain one YAML value")
		}
		content, err := normalizeHostConfigNode(node.Content[0], typ)
		if err != nil {
			return nil, err
		}
		out := cloneYAMLNode(node)
		out.Content = []*yaml.Node{content}
		return out, nil
	}
	if node.Kind == yaml.AliasNode {
		return normalizeHostConfigNode(node.Alias, typ)
	}

	typ = dereferenceType(typ)
	if typ == nil {
		return cloneYAMLNode(node), nil
	}
	switch node.Kind {
	case yaml.MappingNode:
		switch typ.Kind() {
		case reflect.Struct:
			return normalizeHostConfigStruct(node, typ)
		case reflect.Map:
			return normalizeHostConfigMap(node, typ)
		default:
			return cloneYAMLNode(node), nil
		}
	case yaml.SequenceNode:
		if typ.Kind() != reflect.Slice && typ.Kind() != reflect.Array {
			return cloneYAMLNode(node), nil
		}
		out := cloneYAMLNode(node)
		out.Content = make([]*yaml.Node, len(node.Content))
		for i, child := range node.Content {
			normalized, err := normalizeHostConfigNode(child, typ.Elem())
			if err != nil {
				return nil, err
			}
			out.Content[i] = normalized
		}
		return out, nil
	default:
		return cloneYAMLNode(node), nil
	}
}

func normalizeHostConfigStruct(node *yaml.Node, typ reflect.Type) (*yaml.Node, error) {
	fields, err := hostConfigFields(typ)
	if err != nil {
		return nil, err
	}
	out := cloneYAMLNode(node)
	out.Content = make([]*yaml.Node, 0, len(node.Content))
	seen := make(map[string]string, len(node.Content)/2)
	for i := 0; i < len(node.Content); i += 2 {
		key := node.Content[i]
		if key.Kind != yaml.ScalarNode || key.Tag == "!!null" {
			return nil, fmt.Errorf("HostConfig key at line %d must be a string", key.Line)
		}
		canonical := strings.ToLower(key.Value)
		field, ok := fields[canonical]
		if !ok {
			return nil, fmt.Errorf("unknown HostConfig field %q", key.Value)
		}
		if previous, duplicate := seen[canonical]; duplicate {
			return nil, fmt.Errorf("duplicate HostConfig field %q conflicts with %q", key.Value, previous)
		}
		seen[canonical] = key.Value
		value, err := normalizeHostConfigNode(node.Content[i+1], field.typ)
		if err != nil {
			return nil, fmt.Errorf("HostConfig field %q: %w", key.Value, err)
		}
		appendHostConfigValue(out, field.decodePath, value)
	}
	return out, nil
}

func appendHostConfigValue(root *yaml.Node, path []string, value *yaml.Node) {
	current := root
	for i, name := range path {
		var child *yaml.Node
		for j := 0; j < len(current.Content); j += 2 {
			if current.Content[j].Value == name {
				child = current.Content[j+1]
				break
			}
		}
		if child == nil {
			key := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: name}
			if i == len(path)-1 {
				current.Content = append(current.Content, key, value)
				return
			}
			child = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			current.Content = append(current.Content, key, child)
		}
		current = child
	}
}

func normalizeHostConfigMap(node *yaml.Node, typ reflect.Type) (*yaml.Node, error) {
	out := cloneYAMLNode(node)
	out.Content = make([]*yaml.Node, 0, len(node.Content))
	for i := 0; i < len(node.Content); i += 2 {
		key := cloneYAMLNode(node.Content[i])
		value, err := normalizeHostConfigNode(node.Content[i+1], typ.Elem())
		if err != nil {
			return nil, err
		}
		out.Content = append(out.Content, key, value)
	}
	return out, nil
}

func hostConfigFields(typ reflect.Type) (map[string]hostConfigField, error) {
	fields := make(map[string]hostConfigField)
	if err := collectHostConfigFields(typ, fields); err != nil {
		return nil, err
	}
	return fields, nil
}

func collectHostConfigFields(typ reflect.Type, fields map[string]hostConfigField) error {
	return collectHostConfigFieldsAtPath(typ, nil, fields)
}

func collectHostConfigFieldsAtPath(typ reflect.Type, prefix []string, fields map[string]hostConfigField) error {
	typ = dereferenceType(typ)
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		if field.PkgPath != "" && !field.Anonymous {
			continue
		}
		fieldType := dereferenceType(field.Type)
		if field.Anonymous && fieldType.Kind() == reflect.Struct {
			decodeKey := strings.ToLower(field.Name)
			if err := collectHostConfigFieldsAtPath(field.Type, append(append([]string(nil), prefix...), decodeKey), fields); err != nil {
				return err
			}
			continue
		}
		if field.PkgPath != "" {
			continue
		}
		jsonName := strings.Split(field.Tag.Get("json"), ",")[0]
		if jsonName == "-" {
			continue
		}
		decodeKey := strings.Split(field.Tag.Get("yaml"), ",")[0]
		if decodeKey == "-" {
			continue
		}
		if decodeKey == "" {
			decodeKey = strings.ToLower(field.Name)
		}
		path := append(append([]string(nil), prefix...), decodeKey)
		info := hostConfigField{decodePath: path, typ: field.Type}
		aliases := []string{field.Name}
		if jsonName != "" {
			aliases = append(aliases, jsonName)
		}
		for _, alias := range aliases {
			canonical := strings.ToLower(alias)
			if previous, exists := fields[canonical]; exists && (!reflect.DeepEqual(previous.decodePath, info.decodePath) || previous.typ != info.typ) {
				return fmt.Errorf("HostConfig field aliases conflict for %q", alias)
			}
			fields[canonical] = info
		}
	}
	return nil
}

func dereferenceType(typ reflect.Type) reflect.Type {
	for typ != nil && typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	return typ
}

func cloneYAMLNode(node *yaml.Node) *yaml.Node {
	copy := *node
	copy.Content = nil
	return &copy
}
