package controller

import "fmt"

// updateSchemaStatements keeps upgrade state relational because it is
// controller-owned coordination data: it has a fixed schema, CAS semantics,
// and queries by state. runtime_settings remains available for genuinely
// document-shaped operator settings, but update state is not one of those
// documents.
func updateSchemaStatements(types schemaTypes) []string {
	return []string{
		fmt.Sprintf(`CREATE TABLE controller_update_state (
			singleton %s PRIMARY KEY CHECK (singleton=1),
			schema_version %s NOT NULL,
			current_version TEXT NOT NULL,
			channel TEXT NOT NULL,
			deployment TEXT NOT NULL,
			supported %s NOT NULL,
			state TEXT NOT NULL,
			latest_version TEXT NOT NULL DEFAULT '',
			latest_url TEXT NOT NULL DEFAULT '',
			published_at TEXT,
			last_checked_at TEXT,
			last_error TEXT NOT NULL DEFAULT '',
			target_version TEXT NOT NULL DEFAULT '',
			previous_version TEXT NOT NULL DEFAULT '',
			updated_at TEXT NOT NULL
		)`, types.integer, types.integer, types.integer),
		fmt.Sprintf(`CREATE TABLE node_update_states (
			node_id TEXT PRIMARY KEY REFERENCES nodes(id) ON DELETE CASCADE,
			schema_version %s NOT NULL,
			action_id TEXT NOT NULL DEFAULT '',
			current_version TEXT NOT NULL DEFAULT '',
			target_version TEXT NOT NULL DEFAULT '',
			state TEXT NOT NULL,
			supported %s NOT NULL,
			deployment TEXT NOT NULL DEFAULT '',
			reason TEXT NOT NULL DEFAULT '',
			last_error TEXT NOT NULL DEFAULT '',
			updated_at TEXT NOT NULL
		)`, types.integer, types.integer),
		`CREATE INDEX idx_node_update_states_state ON node_update_states(state,updated_at,node_id)`,
	}
}
