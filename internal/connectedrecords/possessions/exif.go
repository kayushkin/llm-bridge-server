package possessions

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"strings"
	"time"
)

// ExifFacts are the few things possession-store reads from a JPEG's EXIF
// block: when the camera took it, where, and which way up it is.
type ExifFacts struct {
	TakenLocal  string // "2006:01:02 15:04:05" as the camera wrote it, no zone; "" when absent
	TakenOffset string // "+08:00" from OffsetTimeOriginal; "" when absent
	Orientation int    // 1 to 8; 1 when absent
	Latitude    *float64
	Longitude   *float64
}

// TakenAt turns the camera's time into an instant. Without an offset in the
// file, zone says where the camera was. ok is false when the file has no date.
func (facts ExifFacts) TakenAt(zone *time.Location) (time.Time, bool, error) {
	if facts.TakenLocal == "" {
		return time.Time{}, false, nil
	}
	if facts.TakenOffset != "" {
		taken, err := time.Parse("2006:01:02 15:04:05-07:00", facts.TakenLocal+facts.TakenOffset)
		return taken, err == nil, err
	}
	if zone == nil {
		return time.Time{}, false, fmt.Errorf("the photo says %s with no time zone; give one", facts.TakenLocal)
	}
	taken, err := time.ParseInLocation("2006:01:02 15:04:05", facts.TakenLocal, zone)
	return taken, err == nil, err
}

// ReadExif reads the EXIF facts of a JPEG. A file that is not a JPEG, or has
// no EXIF block, answers empty facts and no error.
func ReadExif(file []byte) (ExifFacts, error) {
	facts := ExifFacts{Orientation: 1}
	if len(file) < 4 || file[0] != 0xFF || file[1] != 0xD8 {
		return facts, nil
	}
	position := 2
	for position+4 <= len(file) {
		if file[position] != 0xFF {
			return facts, nil
		}
		marker := file[position+1]
		if marker == 0xDA || marker == 0xD9 {
			return facts, nil
		}
		length := int(binary.BigEndian.Uint16(file[position+2:]))
		if length < 2 || position+2+length > len(file) {
			return facts, nil
		}
		segment := file[position+4 : position+2+length]
		if marker == 0xE1 && bytes.HasPrefix(segment, []byte("Exif\x00\x00")) {
			return parseTIFF(segment[6:], facts)
		}
		position += 2 + length
	}
	return facts, nil
}

var errShortExif = errors.New("the EXIF block is cut short")

func parseTIFF(tiff []byte, facts ExifFacts) (ExifFacts, error) {
	if len(tiff) < 8 {
		return facts, errShortExif
	}
	var order binary.ByteOrder
	switch string(tiff[:2]) {
	case "II":
		order = binary.LittleEndian
	case "MM":
		order = binary.BigEndian
	default:
		return facts, fmt.Errorf("the EXIF block has no byte order")
	}
	type entry struct {
		tag, kind uint16
		count     uint32
		value     []byte // the value's bytes, wherever they sit
	}
	sizes := map[uint16]int{1: 1, 2: 1, 3: 2, 4: 4, 5: 8, 7: 1, 9: 4, 10: 8}
	readIFD := func(offset uint32) ([]entry, error) {
		if int(offset)+2 > len(tiff) {
			return nil, errShortExif
		}
		count := int(order.Uint16(tiff[offset:]))
		entries := []entry{}
		for index := 0; index < count; index++ {
			start := int(offset) + 2 + index*12
			if start+12 > len(tiff) {
				return nil, errShortExif
			}
			current := entry{tag: order.Uint16(tiff[start:]), kind: order.Uint16(tiff[start+2:]), count: order.Uint32(tiff[start+4:])}
			size := sizes[current.kind] * int(current.count)
			if size <= 4 {
				current.value = tiff[start+8 : start+8+size]
			} else {
				valueOffset := int(order.Uint32(tiff[start+8:]))
				if valueOffset+size > len(tiff) {
					continue
				}
				current.value = tiff[valueOffset : valueOffset+size]
			}
			entries = append(entries, current)
		}
		return entries, nil
	}
	ascii := func(value []byte) string { return strings.TrimRight(string(value), "\x00 ") }
	rationals := func(value []byte) []float64 {
		numbers := []float64{}
		for start := 0; start+8 <= len(value); start += 8 {
			denominator := order.Uint32(value[start+4:])
			if denominator == 0 {
				return nil
			}
			numbers = append(numbers, float64(order.Uint32(value[start:]))/float64(denominator))
		}
		return numbers
	}
	first, err := readIFD(order.Uint32(tiff[4:]))
	if err != nil {
		return facts, err
	}
	for _, current := range first {
		switch current.tag {
		case 0x0112:
			if len(current.value) >= 2 {
				if orientation := int(order.Uint16(current.value)); orientation >= 1 && orientation <= 8 {
					facts.Orientation = orientation
				}
			}
		case 0x8769:
			exifEntries, err := readIFD(order.Uint32(current.value))
			if err != nil {
				return facts, err
			}
			for _, exifEntry := range exifEntries {
				switch exifEntry.tag {
				case 0x9003:
					facts.TakenLocal = ascii(exifEntry.value)
				case 0x9011:
					facts.TakenOffset = ascii(exifEntry.value)
				}
			}
		case 0x8825:
			gpsEntries, err := readIFD(order.Uint32(current.value))
			if err != nil {
				return facts, err
			}
			var latitudeReference, longitudeReference string
			var latitude, longitude []float64
			for _, gpsEntry := range gpsEntries {
				switch gpsEntry.tag {
				case 1:
					latitudeReference = ascii(gpsEntry.value)
				case 2:
					latitude = rationals(gpsEntry.value)
				case 3:
					longitudeReference = ascii(gpsEntry.value)
				case 4:
					longitude = rationals(gpsEntry.value)
				}
			}
			if len(latitude) == 3 && len(longitude) == 3 && (latitude[0] != 0 || longitude[0] != 0) {
				latitudeValue := latitude[0] + latitude[1]/60 + latitude[2]/3600
				longitudeValue := longitude[0] + longitude[1]/60 + longitude[2]/3600
				if latitudeReference == "S" {
					latitudeValue = -latitudeValue
				}
				if longitudeReference == "W" {
					longitudeValue = -longitudeValue
				}
				facts.Latitude, facts.Longitude = &latitudeValue, &longitudeValue
			}
		}
	}
	if facts.TakenLocal == "0000:00:00 00:00:00" {
		facts.TakenLocal = ""
	}
	return facts, nil
}

// Upright turns an image the way its EXIF orientation says, so it stands as
// the camera's owner saw it.
func Upright(source image.Image, orientation int) image.Image {
	if orientation <= 1 || orientation > 8 {
		return source
	}
	bounds := source.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	swapped := orientation >= 5
	targetWidth, targetHeight := width, height
	if swapped {
		targetWidth, targetHeight = height, width
	}
	target := image.NewRGBA(image.Rect(0, 0, targetWidth, targetHeight))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			var targetX, targetY int
			switch orientation {
			case 2:
				targetX, targetY = width-1-x, y
			case 3:
				targetX, targetY = width-1-x, height-1-y
			case 4:
				targetX, targetY = x, height-1-y
			case 5:
				targetX, targetY = y, x
			case 6:
				targetX, targetY = height-1-y, x
			case 7:
				targetX, targetY = height-1-y, width-1-x
			case 8:
				targetX, targetY = y, width-1-x
			}
			target.Set(targetX, targetY, source.At(bounds.Min.X+x, bounds.Min.Y+y))
		}
	}
	return target
}
