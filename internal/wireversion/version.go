// Package wireversion names the independent wire generations shipped by the
// current Controller and AFDP data plane.
package wireversion

const (
	Control     byte = 1
	Data        byte = 1
	ControlALPN      = "asterferry-control/1"
	DataALPN         = "asterferry-data/1"
)

const Display = "AFDP/1 + control/1"
