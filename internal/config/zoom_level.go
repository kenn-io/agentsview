package config

//go:generate go run github.com/dmarkham/enumer -type=ZoomLevel -trimprefix=ZoomLevel -output=zoomlevel_enumer.go

import (
	"fmt"
	"strings"
)

// ZoomLevel is a supported UI zoom percentage. JSON and TOML encode it as a number.
type ZoomLevel int

const (
	ZoomLevel67  ZoomLevel = 67
	ZoomLevel75  ZoomLevel = 75
	ZoomLevel80  ZoomLevel = 80
	ZoomLevel90  ZoomLevel = 90
	ZoomLevel100 ZoomLevel = 100
	ZoomLevel110 ZoomLevel = 110
	ZoomLevel120 ZoomLevel = 120
	ZoomLevel125 ZoomLevel = 125
	ZoomLevel130 ZoomLevel = 130
	ZoomLevel150 ZoomLevel = 150
	ZoomLevel175 ZoomLevel = 175
	ZoomLevel200 ZoomLevel = 200
)

// Validate rejects percentages outside the supported zoom steps.
func (z ZoomLevel) Validate() error {
	if !z.IsAZoomLevel() {
		return fmt.Errorf("zoom_level must be one of %s (got %d)",
			strings.Join(ZoomLevelStrings(), ", "), z)
	}
	return nil
}
