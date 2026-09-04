package kfxconvert

import "fmt"

const (
	maximumRasterDimension = 32768
	maximumRasterPixels    = 100_000_000
)

func validateRasterDimensions(width, height int, name string) error {
	if width <= 0 || height <= 0 {
		return fmt.Errorf("%s has invalid dimensions %dx%d", name, width, height)
	}
	if width > maximumRasterDimension || height > maximumRasterDimension ||
		int64(width)*int64(height) > maximumRasterPixels {
		return fmt.Errorf("%s dimensions %dx%d exceed Leafport's safe decoding limit", name, width, height)
	}
	return nil
}
