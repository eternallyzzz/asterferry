// Package wireversion names the independent wire generations shipped by the
// current Controller and AFDP data plane.
package wireversion

const (
	Control     byte = 1
	Data        byte = 2
	ControlALPN      = "asterferry-control/2"
	DataALPN         = "asterferry-data/2"
)

const Display = "AFDP/2 + control/2"
