package controller

import "fmt"

// CurrentDatabaseSchemaVersion identifies the physical Controller database
// contract. It is intentionally independent from the control-wire protocol
// version and from snapshot payload versions.
const CurrentDatabaseSchemaVersion uint32 = 12

const databaseSchemaLayout = "relational"

// DatabaseSchemaFingerprint is the marker written into schema_meta and the
// backup manifest. Keeping its construction here prevents a schema bump from
// requiring manual edits in several unrelated packages.
func DatabaseSchemaFingerprint() string {
	return fmt.Sprintf("asterferry-controller-db-v%d-%s", CurrentDatabaseSchemaVersion, databaseSchemaLayout)
}

func databaseSchemaMarker() string {
	return fmt.Sprintf("%d/%s", CurrentDatabaseSchemaVersion, DatabaseSchemaFingerprint())
}
