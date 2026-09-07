package domain

import (
	"reflect"
	"strings"
	"testing"
)

func TestRuntimeMetricCatalogMatchesTypedStruct(t *testing.T) {
	typeOfMetrics := reflect.TypeOf(RuntimeMetrics{})
	fields := make(map[string]reflect.StructField, typeOfMetrics.NumField())
	for index := 0; index < typeOfMetrics.NumField(); index++ {
		field := typeOfMetrics.Field(index)
		name := strings.Split(field.Tag.Get("json"), ",")[0]
		fields[name] = field
	}
	catalog := RuntimeMetricCatalog()
	if len(catalog) != typeOfMetrics.NumField() {
		t.Fatalf("metric catalog has %d entries, typed struct has %d fields", len(catalog), typeOfMetrics.NumField())
	}
	seenJSON := make(map[string]struct{}, len(catalog))
	seenSQL := make(map[string]struct{}, len(catalog))
	for _, metric := range catalog {
		if metric.JSONName == "" || metric.SQLColumn == "" || metric.Kind == "" {
			t.Fatalf("incomplete metric descriptor: %#v", metric)
		}
		if _, exists := seenJSON[metric.JSONName]; exists {
			t.Fatalf("duplicate metric JSON name %q", metric.JSONName)
		}
		if _, exists := seenSQL[metric.SQLColumn]; exists {
			t.Fatalf("duplicate metric SQL column %q", metric.SQLColumn)
		}
		seenJSON[metric.JSONName] = struct{}{}
		seenSQL[metric.SQLColumn] = struct{}{}
		field, ok := fields[metric.JSONName]
		if !ok {
			t.Fatalf("catalog metric %q has no typed field", metric.JSONName)
		}
		switch metric.Kind {
		case RuntimeMetricBoolean:
			if field.Type.Kind() != reflect.Bool {
				t.Fatalf("boolean metric %q has type %s", metric.JSONName, field.Type)
			}
		case RuntimeMetricGauge, RuntimeMetricCounter:
			if field.Type.Kind() != reflect.Uint64 {
				t.Fatalf("numeric metric %q has type %s", metric.JSONName, field.Type)
			}
		default:
			t.Fatalf("unknown metric kind %q", metric.Kind)
		}
	}
}
