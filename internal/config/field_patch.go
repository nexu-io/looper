package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strings"
)

type fieldPatchOperation struct {
	path string
	kind string
}

// ApplyFieldPatch applies canonical dotted-path edits to a detached copy of
// base. Set values are JSON fragments; unset removes an explicitly configured
// value so normal precedence can reveal the lower layer again.
//
// The complete result is decoded through the regular strict config decoder.
// Unknown fields, incompatible values, duplicate unsets, and overlapping
// parent/child operations are rejected before the caller can persist it.
func ApplyFieldPatch(base PartialConfig, set map[string]json.RawMessage, unset []string) (PartialConfig, error) {
	operations, err := validateFieldPatchOperations(set, unset)
	if err != nil {
		return PartialConfig{}, err
	}

	root, err := partialConfigJSONMap(base)
	if err != nil {
		return PartialConfig{}, err
	}

	for _, operation := range operations {
		switch operation.kind {
		case "set":
			value, err := decodeFieldPatchValue(set[operation.path])
			if err != nil {
				return PartialConfig{}, fmt.Errorf("set %q: %w", operation.path, err)
			}
			if err := setConfigMapPath(root, configFieldPathSegments(operation.path), value); err != nil {
				return PartialConfig{}, fmt.Errorf("set %q: %w", operation.path, err)
			}
		case "unset":
			if err := unsetConfigMapPath(root, configFieldPathSegments(operation.path)); err != nil {
				return PartialConfig{}, fmt.Errorf("unset %q: %w", operation.path, err)
			}
		}
	}

	raw, err := json.Marshal(root)
	if err != nil {
		return PartialConfig{}, fmt.Errorf("encode patched config: %w", err)
	}
	patched, err := decodeJSONConfigFile(raw)
	if err != nil {
		return PartialConfig{}, fmt.Errorf("decode patched config: %w", err)
	}
	return patched, nil
}

func validateFieldPatchOperations(set map[string]json.RawMessage, unset []string) ([]fieldPatchOperation, error) {
	setPaths := make([]string, 0, len(set))
	for path := range set {
		setPaths = append(setPaths, path)
	}
	sort.Strings(setPaths)

	operations := make([]fieldPatchOperation, 0, len(set)+len(unset))
	seen := make(map[string]string, len(set)+len(unset))
	add := func(path string, kind string) error {
		if err := validateFieldPatchPath(path); err != nil {
			return fmt.Errorf("%s %q: %w", kind, path, err)
		}
		if previous, exists := seen[path]; exists {
			if previous == kind {
				return fmt.Errorf("duplicate %s path %q", kind, path)
			}
			return fmt.Errorf("path %q cannot be both set and unset", path)
		}
		seen[path] = kind
		operations = append(operations, fieldPatchOperation{path: path, kind: kind})
		return nil
	}

	for _, path := range setPaths {
		if err := add(path, "set"); err != nil {
			return nil, err
		}
	}
	for _, path := range unset {
		if err := add(path, "unset"); err != nil {
			return nil, err
		}
	}

	// Every ancestor must end at one of the child's dots. Looking those prefixes
	// up in the existing operation map is linear in total path depth instead of
	// comparing every operation pair (large agent.env patches can contain many
	// thousands of independent keys).
	for _, operation := range operations {
		for index := strings.IndexByte(operation.path, '.'); index >= 0; {
			ancestor := operation.path[:index]
			if _, exists := seen[ancestor]; exists {
				return nil, fmt.Errorf("conflicting patch paths %q and %q", ancestor, operation.path)
			}
			next := strings.IndexByte(operation.path[index+1:], '.')
			if next < 0 {
				break
			}
			index += next + 1
		}
	}

	sort.SliceStable(operations, func(left int, right int) bool {
		if operations[left].kind != operations[right].kind {
			return operations[left].kind == "set"
		}
		return operations[left].path < operations[right].path
	})
	return operations, nil
}

func validateFieldPatchPath(path string) error {
	if path == "" {
		return fmt.Errorf("path must not be empty")
	}
	if path != strings.TrimSpace(path) {
		return fmt.Errorf("path must not have surrounding whitespace")
	}
	segments := configFieldPathSegments(path)
	for _, segment := range segments {
		if segment == "" {
			return fmt.Errorf("path contains an empty segment")
		}
		if segment != strings.TrimSpace(segment) {
			return fmt.Errorf("path segment %q has surrounding whitespace", segment)
		}
	}
	return validateFieldPatchPathType(reflect.TypeOf(PartialConfig{}), segments)
}

// IsFieldLevelConfigPath reports whether path identifies a scalar, a complete
// list, or one dynamic map entry. Object and map-container replacement is too
// broad for the curated dashboard patch contract.
func IsFieldLevelConfigPath(path string) bool {
	if err := validateFieldPatchPath(path); err != nil {
		return false
	}
	return isFieldLevelConfigPathType(reflect.TypeOf(PartialConfig{}), configFieldPathSegments(path))
}

func isFieldLevelConfigPathType(current reflect.Type, segments []string) bool {
	for current.Kind() == reflect.Pointer {
		current = current.Elem()
	}
	if len(segments) == 0 {
		return current.Kind() != reflect.Struct && current.Kind() != reflect.Map && current.Kind() != reflect.Interface
	}

	switch current.Kind() {
	case reflect.Struct:
		for index := 0; index < current.NumField(); index++ {
			field := current.Field(index)
			name := strings.Split(field.Tag.Get("json"), ",")[0]
			if name == "" {
				name = field.Name
			}
			if name != "-" && name == segments[0] {
				return isFieldLevelConfigPathType(field.Type, segments[1:])
			}
		}
		return false
	case reflect.Map:
		if current.Key().Kind() != reflect.String || len(segments) != 1 || segments[0] == "" {
			return false
		}
		return isFieldLevelConfigPathType(current.Elem(), nil)
	case reflect.Interface:
		return false
	default:
		return false
	}
}

// Config paths use unescaped dots as separators. Environment-variable names
// are validated separately and therefore cannot contain dots.
func configFieldPathSegments(path string) []string {
	return strings.Split(path, ".")
}

func validateFieldPatchPathType(current reflect.Type, segments []string) error {
	for current.Kind() == reflect.Pointer {
		current = current.Elem()
	}
	if len(segments) == 0 {
		return nil
	}

	switch current.Kind() {
	case reflect.Struct:
		for index := 0; index < current.NumField(); index++ {
			field := current.Field(index)
			name := strings.Split(field.Tag.Get("json"), ",")[0]
			if name == "" {
				name = field.Name
			}
			if name == "-" || name != segments[0] {
				continue
			}
			if len(segments) == 1 {
				return nil
			}
			return validateFieldPatchPathType(field.Type, segments[1:])
		}
		return fmt.Errorf("unknown config field %q", segments[0])
	case reflect.Map:
		if current.Key().Kind() != reflect.String {
			return fmt.Errorf("config map does not use string keys")
		}
		if len(segments) == 1 {
			return nil
		}
		return validateFieldPatchPathType(current.Elem(), segments[1:])
	case reflect.Interface:
		return nil
	case reflect.Slice, reflect.Array:
		return fmt.Errorf("cannot address inside a config list; set or unset the complete list")
	default:
		return fmt.Errorf("cannot address inside scalar config field")
	}
}

func partialConfigJSONMap(partial PartialConfig) (map[string]any, error) {
	raw, err := json.Marshal(partial)
	if err != nil {
		return nil, fmt.Errorf("encode config for patch: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var root map[string]any
	if err := decoder.Decode(&root); err != nil {
		return nil, fmt.Errorf("decode config for patch: %w", err)
	}
	if root == nil {
		root = map[string]any{}
	}
	return root, nil
}

func decodeFieldPatchValue(raw json.RawMessage) (any, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, fmt.Errorf("value must contain one JSON value")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("invalid JSON value: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return nil, fmt.Errorf("value must contain one JSON value")
	}
	return value, nil
}

func setConfigMapPath(root map[string]any, segments []string, value any) error {
	current := root
	for index, segment := range segments {
		if index == len(segments)-1 {
			current[segment] = value
			return nil
		}
		child, exists := current[segment]
		if !exists || child == nil {
			next := map[string]any{}
			current[segment] = next
			current = next
			continue
		}
		next, ok := child.(map[string]any)
		if !ok {
			return fmt.Errorf("path segment %q contains a non-object value", segment)
		}
		current = next
	}
	return nil
}

func unsetConfigMapPath(root map[string]any, segments []string) error {
	current := root
	for index, segment := range segments {
		if index == len(segments)-1 {
			delete(current, segment)
			return nil
		}
		child, exists := current[segment]
		if !exists || child == nil {
			return nil
		}
		next, ok := child.(map[string]any)
		if !ok {
			return fmt.Errorf("path segment %q contains a non-object value", segment)
		}
		current = next
	}
	return nil
}
