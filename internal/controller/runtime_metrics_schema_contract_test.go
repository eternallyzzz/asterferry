package controller

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"asterferry/internal/domain"
)

func TestRuntimeMetricCatalogMatchesObservedSchemaAndOpenAPI(t *testing.T) {
	store, err := openTestStore(filepath.Join(t.TempDir(), "metrics-schema.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	rows, err := store.db.QueryContext(context.Background(), `PRAGMA table_info(observed_states)`)
	if err != nil {
		t.Fatal(err)
	}
	columns := map[string]struct{}{}
	for rows.Next() {
		var cid int
		var name, columnType string
		var notNull, primaryKey int
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			_ = rows.Close()
			t.Fatal(err)
		}
		columns[name] = struct{}{}
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	for _, metric := range domain.RuntimeMetricCatalog() {
		if _, ok := columns[metric.SQLColumn]; !ok {
			t.Fatalf("metric %q is missing from observed_states", metric.SQLColumn)
		}
	}
	for _, name := range []string{"openapi.yaml", filepath.Join("..", "..", "api", "openapi.yaml")} {
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		text := string(data)
		for _, metric := range domain.RuntimeMetricCatalog() {
			if !strings.Contains(text, "        "+metric.JSONName+":") {
				t.Fatalf("metric %q is missing from %s", metric.JSONName, name)
			}
		}
	}
}
