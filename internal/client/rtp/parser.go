package rtp

import (
	"encoding/binary"
	"fmt"
)

const (
	PayloadTypeH264 = 96
	PayloadTypeOpus = 111
)

type Header struct {
	Version       uint8
	PayloadType   uint8
	Sequence      uint16
	Timestamp     uint32
	SSRC          uint32
	Marker        bool
	ParticipantID uint32
	Payload       []byte
}

func ParsePacket(data []byte) (*Header, error) {
	if len(data) < 12 {
		return nil, fmt.Errorf("packet too short")
	}

	header := &Header{}
	header.Version = (data[0] >> 6) & 0x03
	padding := (data[0] >> 5) & 0x01
	extension := (data[0] >> 4) & 0x01
	csrcCount := data[0] & 0x0F

	header.Marker = ((data[1] >> 7) & 0x01) != 0
	header.PayloadType = data[1] & 0x7F

	header.Sequence = binary.BigEndian.Uint16(data[2:4])
	header.Timestamp = binary.BigEndian.Uint32(data[4:8])
	header.SSRC = binary.BigEndian.Uint32(data[8:12])

	offset := 12 + int(csrcCount)*4

	if extension != 0 {
		if len(data) < offset+4 {
			return nil, fmt.Errorf("extension header incomplete")
		}

		extID := binary.BigEndian.Uint16(data[offset : offset+2])
		extLen := binary.BigEndian.Uint16(data[offset+2:offset+4]) * 4

		if extID == 1 && extLen == 4 {
			if len(data) >= offset+8 {
				header.ParticipantID = binary.LittleEndian.Uint32(data[offset+4 : offset+8])
			}
		}
		offset += 4 + int(extLen)
	}

	if padding != 0 && len(data) > 0 {
		paddingLen := int(data[len(data)-1])
		header.Payload = data[offset : len(data)-paddingLen]
	} else {
		header.Payload = data[offset:]
	}

	return header, nil
}

func (h *Header) IsVideoPacket() bool {
	return h.PayloadType == PayloadTypeH264
}

func (h *Header) IsAudioPacket() bool {
	return h.PayloadType == PayloadTypeOpus
}

func (h *Header) GetNALType() byte {
	if len(h.Payload) > 0 {
		return h.Payload[0] & 0x1F
	}
	return 0
}

func GetNALTypeName(nalType byte) string {
	switch nalType {
	case 1:
		return "Non-IDR Slice (P-frame)"
	case 5:
		return "IDR Slice (Keyframe)"
	case 6:
		return "SEI"
	case 7:
		return "SPS"
	case 8:
		return "PPS"
	case 9:
		return "Access Unit Delimiter"
	case 28:
		return "FU-A (Fragmented)"
	default:
		return "Other"
	}
}

// Parsed Opus TOC (Table of Contents) byte
type OpusTOC struct {
	Config     uint8
	Stereo     bool
	FrameCount uint8
}

func (h *Header) GetOpusTOC() *OpusTOC {
	if len(h.Payload) == 0 {
		return nil
	}
	tocByte := h.Payload[0]
	return &OpusTOC{
		Config:     (tocByte >> 3) & 0x1F,
		Stereo:     ((tocByte >> 2) & 0x01) != 0,
		FrameCount: tocByte & 0x03,
	}
}
