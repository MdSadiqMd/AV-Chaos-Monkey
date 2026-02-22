package rtp

import "encoding/binary"

type ParsedPacket struct {
	PayloadType   uint8
	Sequence      uint16
	Timestamp     uint32
	SSRC          uint32
	Marker        bool
	ParticipantID uint32
	Payload       []byte
}

func ParsePacket(data []byte) (*ParsedPacket, error) {
	pkt, err := Unmarshal(data)
	if err != nil {
		return nil, err
	}

	parsed := &ParsedPacket{
		PayloadType: pkt.Header.PayloadType,
		Sequence:    pkt.Header.SequenceNumber,
		Timestamp:   pkt.Header.Timestamp,
		SSRC:        pkt.Header.SSRC,
		Marker:      pkt.Header.Marker,
		Payload:     pkt.Payload,
	}

	if len(pkt.Header.Extensions) > 0 && pkt.Header.Extensions[0].ID == 1 {
		if len(pkt.Header.Extensions[0].Data) >= 4 {
			parsed.ParticipantID = binary.LittleEndian.Uint32(pkt.Header.Extensions[0].Data)
		}
	}

	return parsed, nil
}
