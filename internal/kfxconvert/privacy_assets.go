package kfxconvert

import (
	"bytes"
	"encoding/binary"
	"encoding/xml"
	"errors"
	"fmt"
	"image/gif"
	"io"
	"strings"
)

func (book *decodedBook) sanitizeAsset(data []byte, mediaType string) ([]byte, error) {
	if book == nil || book.privacy == nil {
		return data, nil
	}
	switch {
	case bytes.HasPrefix(data, []byte("%PDF-")):
		return sanitizePDFBytes(data)
	case len(data) >= 3 && bytes.Equal(data[:3], []byte{0xff, 0xd8, 0xff}):
		return stripJPEGMetadata(data)
	case bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n")):
		return stripPNGMetadata(data)
	case bytes.HasPrefix(data, []byte("GIF87a")), bytes.HasPrefix(data, []byte("GIF89a")):
		return stripGIFMetadata(data)
	case len(data) >= 12 && bytes.Equal(data[:4], []byte("RIFF")) && bytes.Equal(data[8:12], []byte("WEBP")):
		return stripWebPMetadata(data)
	case isSVG(data):
		return stripSVGMetadata(data, book.privacy)
	case mediaType == "audio/mpeg" || bytes.HasPrefix(data, []byte("ID3")):
		return stripMP3Metadata(data)
	case (mediaType == "audio/wav" || mediaType == "audio/x-wav") && len(data) >= 12 &&
		bytes.Equal(data[:4], []byte("RIFF")) && bytes.Equal(data[8:12], []byte("WAVE")):
		return stripWAVMetadata(data)
	case (mediaType == "audio/mp4" || mediaType == "video/mp4") && len(data) >= 12 && bytes.Equal(data[4:8], []byte("ftyp")):
		return stripMP4Metadata(data)
	case strings.HasPrefix(mediaType, "audio/") || strings.HasPrefix(mediaType, "video/"):
		return nil, fmt.Errorf("cannot safely remove metadata from embedded %s without changing media content", mediaType)
	default:
		return data, nil
	}
}

func stripJPEGMetadata(data []byte) ([]byte, error) {
	if len(data) < 4 || !bytes.Equal(data[:2], []byte{0xff, 0xd8}) {
		return nil, errors.New("invalid JPEG header")
	}
	output := append([]byte(nil), data[:2]...)
	for offset := 2; offset < len(data); {
		start := offset
		if data[offset] != 0xff {
			return nil, fmt.Errorf("invalid JPEG marker at byte %d", offset)
		}
		for offset < len(data) && data[offset] == 0xff {
			offset++
		}
		if offset >= len(data) {
			return nil, errors.New("truncated JPEG marker")
		}
		marker := data[offset]
		offset++
		if marker == 0xda || marker == 0xd9 {
			return append(output, data[start:]...), nil
		}
		if marker == 0x01 || marker >= 0xd0 && marker <= 0xd7 {
			output = append(output, data[start:offset]...)
			continue
		}
		if offset+2 > len(data) {
			return nil, errors.New("truncated JPEG segment length")
		}
		length := int(binary.BigEndian.Uint16(data[offset : offset+2]))
		end := offset + length
		if length < 2 || end > len(data) {
			return nil, fmt.Errorf("invalid JPEG segment length at byte %d", start)
		}
		// Preserve JFIF, ICC colour profiles, and Adobe colour transforms. Other
		// application segments and comments can carry EXIF, XMP, IPTC, JUMBF,
		// editor history, or owner identifiers.
		keep := marker < 0xe0 || marker > 0xef || marker == 0xe0 || marker == 0xe2 || marker == 0xee
		if marker == 0xfe {
			keep = false
		}
		if keep {
			output = append(output, data[start:end]...)
		}
		offset = end
	}
	return nil, errors.New("JPEG has no scan or end marker")
}

func stripPNGMetadata(data []byte) ([]byte, error) {
	if len(data) < 8 || !bytes.Equal(data[:8], []byte("\x89PNG\r\n\x1a\n")) {
		return nil, errors.New("invalid PNG header")
	}
	output := append([]byte(nil), data[:8]...)
	drop := map[string]bool{
		"eXIf": true, "tEXt": true, "zTXt": true, "iTXt": true,
		"tIME": true, "caBX": true, "meTa": true,
	}
	for offset := 8; offset < len(data); {
		if offset+12 > len(data) {
			return nil, errors.New("truncated PNG chunk")
		}
		length := int(binary.BigEndian.Uint32(data[offset : offset+4]))
		if length < 0 || length > len(data)-offset-12 {
			return nil, fmt.Errorf("invalid PNG chunk length at byte %d", offset)
		}
		end := offset + 12 + length
		name := string(data[offset+4 : offset+8])
		if !drop[name] {
			output = append(output, data[offset:end]...)
		}
		offset = end
		if name == "IEND" {
			if offset != len(data) {
				return nil, errors.New("unexpected data after PNG IEND")
			}
			return output, nil
		}
	}
	return nil, errors.New("PNG has no IEND chunk")
}

func stripGIFMetadata(data []byte) ([]byte, error) {
	animation, err := gif.DecodeAll(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("decode GIF for metadata cleanup: %w", err)
	}
	var output bytes.Buffer
	if err := gif.EncodeAll(&output, animation); err != nil {
		return nil, fmt.Errorf("encode metadata-free GIF: %w", err)
	}
	return output.Bytes(), nil
}

func stripWebPMetadata(data []byte) ([]byte, error) {
	if len(data) < 12 || !bytes.Equal(data[:4], []byte("RIFF")) || !bytes.Equal(data[8:12], []byte("WEBP")) {
		return nil, errors.New("invalid WebP header")
	}
	var output bytes.Buffer
	output.WriteString("RIFF")
	output.Write(make([]byte, 4))
	output.WriteString("WEBP")
	drop := map[string]bool{"EXIF": true, "XMP ": true, "META": true, "IPTC": true, "C2PA": true}
	for offset := 12; offset < len(data); {
		if offset+8 > len(data) {
			return nil, errors.New("truncated WebP chunk")
		}
		name := string(data[offset : offset+4])
		length := int(binary.LittleEndian.Uint32(data[offset+4 : offset+8]))
		padded := length + length%2
		end := offset + 8 + padded
		if length < 0 || end > len(data) {
			return nil, fmt.Errorf("invalid WebP chunk length at byte %d", offset)
		}
		if !drop[name] {
			chunk := append([]byte(nil), data[offset:end]...)
			if name == "VP8X" && length >= 1 {
				chunk[8] &^= 0x0c // EXIF and XMP presence flags.
			}
			output.Write(chunk)
		}
		offset = end
	}
	result := output.Bytes()
	if uint64(len(result)-8) > uint64(^uint32(0)) {
		return nil, errors.New("WebP exceeds RIFF size limit")
	}
	binary.LittleEndian.PutUint32(result[4:8], uint32(len(result)-8))
	return result, nil
}

func stripSVGMetadata(data []byte, redactor *personalRedactor) ([]byte, error) {
	decoder := xml.NewDecoder(bytes.NewReader(data))
	var output bytes.Buffer
	encoder := xml.NewEncoder(&output)
	skipDepth := 0
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("decode SVG: %w", err)
		}
		switch value := token.(type) {
		case xml.StartElement:
			if skipDepth != 0 {
				skipDepth++
				continue
			}
			if strings.EqualFold(value.Name.Local, "metadata") {
				skipDepth = 1
				continue
			}
			for index := range value.Attr {
				if redactor.matches(value.Attr[index].Value) {
					value.Attr[index].Value = ""
				}
			}
			token = value
		case xml.EndElement:
			if skipDepth != 0 {
				skipDepth--
				continue
			}
		case xml.CharData:
			if skipDepth != 0 {
				continue
			}
			token = xml.CharData(redactor.redact(string(value)))
		default:
			if skipDepth != 0 {
				continue
			}
		}
		if err := encoder.EncodeToken(token); err != nil {
			return nil, err
		}
	}
	if err := encoder.Flush(); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

func stripMP3Metadata(data []byte) ([]byte, error) {
	start := 0
	if len(data) >= 10 && bytes.Equal(data[:3], []byte("ID3")) {
		for _, value := range data[6:10] {
			if value&0x80 != 0 {
				return nil, errors.New("invalid ID3 synchsafe size")
			}
		}
		size := int(data[6])<<21 | int(data[7])<<14 | int(data[8])<<7 | int(data[9])
		start = 10 + size
		if data[5]&0x10 != 0 {
			start += 10
		}
		if start > len(data) {
			return nil, errors.New("truncated ID3 tag")
		}
	}
	end := len(data)
	if end-start >= 128 && bytes.Equal(data[end-128:end-125], []byte("TAG")) {
		end -= 128
	}
	if end-start >= 32 && bytes.Equal(data[end-32:end-24], []byte("APETAGEX")) {
		size := int(binary.LittleEndian.Uint32(data[end-20 : end-16]))
		if size < 32 || size > end-start {
			return nil, errors.New("invalid APE tag size")
		}
		end -= size
	}
	if start >= end {
		return nil, errors.New("audio contains metadata but no MPEG frames")
	}
	return append([]byte(nil), data[start:end]...), nil
}

func stripWAVMetadata(data []byte) ([]byte, error) {
	if len(data) < 12 || !bytes.Equal(data[:4], []byte("RIFF")) || !bytes.Equal(data[8:12], []byte("WAVE")) {
		return nil, errors.New("invalid WAV header")
	}
	var output bytes.Buffer
	output.WriteString("RIFF")
	output.Write(make([]byte, 4))
	output.WriteString("WAVE")
	drop := map[string]bool{"ID3 ": true, "id3 ": true, "iXML": true, "bext": true, "XMP ": true, "DISP": true}
	for offset := 12; offset < len(data); {
		if offset+8 > len(data) {
			return nil, errors.New("truncated WAV chunk")
		}
		name := string(data[offset : offset+4])
		length := int(binary.LittleEndian.Uint32(data[offset+4 : offset+8]))
		end := offset + 8 + length + length%2
		if length < 0 || end > len(data) {
			return nil, fmt.Errorf("invalid WAV chunk length at byte %d", offset)
		}
		remove := drop[name]
		if name == "LIST" && length >= 4 && bytes.Equal(data[offset+8:offset+12], []byte("INFO")) {
			remove = true
		}
		if !remove {
			output.Write(data[offset:end])
		}
		offset = end
	}
	result := output.Bytes()
	if uint64(len(result)-8) > uint64(^uint32(0)) {
		return nil, errors.New("WAV exceeds RIFF size limit")
	}
	binary.LittleEndian.PutUint32(result[4:8], uint32(len(result)-8))
	return result, nil
}

func stripMP4Metadata(data []byte) ([]byte, error) {
	result := append([]byte(nil), data...)
	if err := scrubMP4Atoms(result, 0, len(result), "root"); err != nil {
		return nil, err
	}
	return result, nil
}

func scrubMP4Atoms(data []byte, start, end int, parent string) error {
	containers := map[string]bool{
		"moov": true, "trak": true, "mdia": true, "minf": true,
		"edts": true, "dinf": true, "mvex": true, "moof": true,
		"traf": true, "mfra": true,
	}
	for offset := start; offset < end; {
		if offset+8 > end {
			return fmt.Errorf("truncated MP4 atom below %s", parent)
		}
		size := uint64(binary.BigEndian.Uint32(data[offset : offset+4]))
		header := 8
		if size == 1 {
			if offset+16 > end {
				return errors.New("truncated extended MP4 atom")
			}
			size = binary.BigEndian.Uint64(data[offset+8 : offset+16])
			header = 16
		} else if size == 0 {
			size = uint64(end - offset)
		}
		if size < uint64(header) || size > uint64(end-offset) {
			return fmt.Errorf("invalid MP4 atom size at byte %d", offset)
		}
		typeName := string(data[offset+4 : offset+8])
		atomEnd := offset + int(size)
		if typeName == "udta" || typeName == "meta" || typeName == "uuid" || typeName == "free" || typeName == "skip" {
			copy(data[offset+4:offset+8], "free")
			clear(data[offset+header : atomEnd])
		} else if containers[typeName] {
			if err := scrubMP4Atoms(data, offset+header, atomEnd, typeName); err != nil {
				return err
			}
		}
		offset = atomEnd
	}
	return nil
}
