package config

import (
	"reflect"
	"sort"
	"strings"
)

// ValueSource identifies the winning configuration precedence layer for a
// canonical field path.
type ValueSource string

const (
	ValueSourceDefault    ValueSource = "default"
	ValueSourceConfigFile ValueSource = "config-file"
	ValueSourceEnv        ValueSource = "env"
	ValueSourceCLI        ValueSource = "cli"
)

func discoverFieldSources(effective Config, file PartialConfig, env PartialConfig, cli PartialConfig) map[string]ValueSource {
	sources := make(map[string]ValueSource)
	mapContainers := make(map[string]struct{})
	collectConfigSchemaPaths(reflect.TypeOf(Config{}), "", sources, mapContainers)
	collectDynamicConfigPaths(reflect.ValueOf(effective), "", func(path string) {
		if path != "" {
			if _, exists := sources[path]; !exists {
				sources[path] = ValueSourceDefault
			}
		}
	})

	applySource := func(partial PartialConfig, source ValueSource) {
		normalized := normalizeLayerPartial(clonePartialConfig(partial))
		collectDynamicConfigPaths(reflect.ValueOf(normalized), "", func(path string) {
			if isCanonicalSourcePath(path, sources, mapContainers) {
				sources[path] = source
			}
		})
	}
	applySource(file, ValueSourceConfigFile)
	applySource(env, ValueSourceEnv)
	applySource(cli, ValueSourceCLI)
	return sources
}

func collectConfigSchemaPaths(current reflect.Type, path string, sources map[string]ValueSource, mapContainers map[string]struct{}) {
	for current.Kind() == reflect.Pointer {
		current = current.Elem()
	}
	switch current.Kind() {
	case reflect.Struct:
		for index := 0; index < current.NumField(); index++ {
			field := current.Field(index)
			name, ok := configJSONFieldName(field)
			if !ok {
				continue
			}
			collectConfigSchemaPaths(field.Type, joinConfigPath(path, name), sources, mapContainers)
		}
	case reflect.Map:
		if path != "" {
			sources[path] = ValueSourceDefault
			mapContainers[path] = struct{}{}
		}
	case reflect.Slice, reflect.Array:
		if path != "" {
			sources[path] = ValueSourceDefault
		}
	default:
		if path != "" {
			sources[path] = ValueSourceDefault
		}
	}
}

func collectDynamicConfigPaths(current reflect.Value, path string, visit func(string)) {
	if !current.IsValid() {
		return
	}
	for current.Kind() == reflect.Interface || current.Kind() == reflect.Pointer {
		if current.IsNil() {
			return
		}
		current = current.Elem()
	}

	switch current.Kind() {
	case reflect.Struct:
		currentType := current.Type()
		for index := 0; index < current.NumField(); index++ {
			fieldType := currentType.Field(index)
			name, ok := configJSONFieldName(fieldType)
			if !ok {
				continue
			}
			fieldValue := current.Field(index)
			if (fieldValue.Kind() == reflect.Pointer || fieldValue.Kind() == reflect.Map || fieldValue.Kind() == reflect.Slice || fieldValue.Kind() == reflect.Interface) && fieldValue.IsNil() {
				continue
			}
			collectDynamicConfigPaths(fieldValue, joinConfigPath(path, name), visit)
		}
	case reflect.Map:
		if path != "" {
			visit(path)
		}
		if current.Type().Key().Kind() != reflect.String {
			return
		}
		keys := current.MapKeys()
		sort.Slice(keys, func(left int, right int) bool {
			return keys[left].String() < keys[right].String()
		})
		for _, key := range keys {
			childPath := joinConfigPath(path, key.String())
			child := current.MapIndex(key)
			if !child.IsValid() {
				continue
			}
			before := false
			for value := child; value.IsValid() && (value.Kind() == reflect.Interface || value.Kind() == reflect.Pointer); value = value.Elem() {
				if value.IsNil() {
					visit(childPath)
					before = true
					break
				}
			}
			if before {
				continue
			}
			kind := dynamicValueKind(child)
			if kind == reflect.Struct || kind == reflect.Map {
				collectDynamicConfigPaths(child, childPath, visit)
			} else {
				visit(childPath)
			}
		}
	case reflect.Slice, reflect.Array:
		if path != "" {
			visit(path)
		}
	default:
		if path != "" {
			visit(path)
		}
	}
}

func dynamicValueKind(value reflect.Value) reflect.Kind {
	for value.IsValid() && (value.Kind() == reflect.Interface || value.Kind() == reflect.Pointer) {
		if value.IsNil() {
			return reflect.Invalid
		}
		value = value.Elem()
	}
	if !value.IsValid() {
		return reflect.Invalid
	}
	return value.Kind()
}

func configJSONFieldName(field reflect.StructField) (string, bool) {
	name := strings.Split(field.Tag.Get("json"), ",")[0]
	if name == "-" {
		return "", false
	}
	if name == "" {
		name = field.Name
	}
	return name, true
}

func isCanonicalSourcePath(path string, schema map[string]ValueSource, mapContainers map[string]struct{}) bool {
	if _, exists := schema[path]; exists {
		return true
	}
	for container := range mapContainers {
		if strings.HasPrefix(path, container+".") {
			return true
		}
	}
	return false
}
