package protocol

// Control/data generations are intentionally independent of the retired v6
// relay codec. Version remains exported for the legacy package users while new
// Controller and data-plane code uses these explicit identifiers.
const (
	ControlVersion byte = 1
	DataVersion    byte = 1
	DataALPN            = "asterferry-data/1"
	ControlALPN         = "asterferry-control/1"
)
