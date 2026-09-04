package controller

import (
	"time"
)

const backupManifestVersion = 2

type backupManifest struct {
	Version          int                  `json:"version"`
	CreatedAt        time.Time            `json:"created_at"`
	ControllerSchema string               `json:"controller_schema"`
	DatabaseSchema   string               `json:"database_schema,omitempty"`
	Files            []backupManifestFile `json:"files"`
}

type backupManifestFile struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

var backupPayloadFiles = []string{
	"controller.db",
	"controller.json",
	"master.key",
	"ca.key",
	"ca.crt",
	"controller.key",
	"controller.crt",
}

var backupPayloadFilesPostgres = []string{
	"controller.postgres.dump",
	"controller.json",
	"master.key",
	"ca.key",
	"ca.crt",
	"controller.key",
	"controller.crt",
}

func backupPayloadFilesForBackend(backend databaseBackend) []string {
	if backend == databaseBackendPostgres {
		return backupPayloadFilesPostgres
	}
	return backupPayloadFiles
}
