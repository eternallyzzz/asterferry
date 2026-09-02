package controller

import "testing"

func TestBindPostgresPlaceholdersSkipsQuotedTextAndComments(t *testing.T) {
	query := `SELECT ?, '?', 'it''s ?' AS literal, "?" AS identifier, ` + "`?`" + ` AS legacy_identifier, -- ?
  ? /* ? */ ?`
	want := `SELECT $1, '?', 'it''s ?' AS literal, "?" AS identifier, ` + "`?`" + ` AS legacy_identifier, -- ?
  $2 /* ? */ $3`
	if got := bindPostgresPlaceholders(query); got != want {
		t.Fatalf("bound query = %q, want %q", got, want)
	}
}

func TestBindPostgresPlaceholdersNumbersAllArguments(t *testing.T) {
	query := "SELECT "
	for index := 0; index < 12; index++ {
		if index > 0 {
			query += ","
		}
		query += "?"
	}
	want := "SELECT $1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12"
	if got := bindPostgresPlaceholders(query); got != want {
		t.Fatalf("bound query = %q, want %q", got, want)
	}
}
