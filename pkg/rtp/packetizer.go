package rtp

import (
	"encoding/binary"
)

// Represents an RTP packet header
type RTPHeader struct {
	Version        uint8
	Padding        bool
	Extension      bool
	CSRCCount      uint8
	Marker         bool
	PayloadType    uint8
	SequenceNumber uint16
	Timestamp      uint32
	SSRC           uint32
	CSRC           []uint32
	ExtensionID    uint16
	Extensions     []Extension
}

// Extension represents an RTP header extension
type Extension struct {
	ID   uint8
	Data []byte
}

// Represents a complete RTP packet
type RTPPacket struct {
	Header  RTPHeader
	Payload []byte
}

// H264Packetizer packetizes H.264 NALUs into RTP packets
type H264Packetizer struct {
	SSRC          uint32
	PayloadType   uint8
	ClockRate     uint32
	MTU           int
	ParticipantID uint32
}

func NewH264Packetizer(participantID uint32, payloadType uint8, clockRate uint32) *H264Packetizer {
	return &H264Packetizer{
		SSRC:          participantID * 1000, // Simple SSRC derivation
		PayloadType:   payloadType,
		ClockRate:     clockRate,
		MTU:           1200, // Conservative for NAT traversal
		ParticipantID: participantID,
	}
}

// Packetize converts a frame into RTP packets with FU-A fragmentation
func (p *H264Packetizer) Packetize(nalu []byte, sequenceNumber uint16, timestamp uint32) []*RTPPacket {
	packets := []*RTPPacket{}
	maxPayload := p.MTU - 12 // RTP header size

	if len(nalu) <= maxPayload {
		// Single NAL unit packet
		pkt := &RTPPacket{
			Header: RTPHeader{
				Version:        2,
				Padding:        false,
				Extension:      true,
				Marker:         true,
				PayloadType:    p.PayloadType,
				SequenceNumber: sequenceNumber,
				Timestamp:      timestamp,
				SSRC:           p.SSRC,
				Extensions:     p.createParticipantIDExtension(),
			},
			Payload: nalu,
		}
		packets = append(packets, pkt)
	} else {
		// FU-A fragmentation
		fragments := p.fragmentNALU(nalu, maxPayload-2) // -2 for FU indicator + FU header
		naluType := nalu[0] & 0x1F
		nri := nalu[0] & 0x60

		for i, frag := range fragments {
			fuIndicator := byte(28) | nri // FU-A type
			fuHeader := naluType

			if i == 0 {
				fuHeader |= 0x80 // Start bit
			}
			if i == len(fragments)-1 {
				fuHeader |= 0x40 // End bit
			}

			payload := make([]byte, 2+len(frag))
			payload[0] = fuIndicator
			payload[1] = fuHeader
			copy(payload[2:], frag)

			pkt := &RTPPacket{
				Header: RTPHeader{
					Version:        2,
					Padding:        false,
					Extension:      true,
					Marker:         i == len(fragments)-1,
					PayloadType:    p.PayloadType,
					SequenceNumber: sequenceNumber + uint16(i),
					Timestamp:      timestamp,
					SSRC:           p.SSRC,
					Extensions:     p.createParticipantIDExtension(),
				},
				Payload: payload,
			}
			packets = append(packets, pkt)
		}
	}

	return packets
}

// fragmentNALU splits a NALU into fragments
func (p *H264Packetizer) fragmentNALU(nalu []byte, maxSize int) [][]byte {
	fragments := [][]byte{}

	// Skip first byte (NALU header) for fragments
	data := nalu[1:]

	for i := 0; i < len(data); i += maxSize {
		end := min(i+maxSize, len(data))

		fragment := make([]byte, end-i)
		copy(fragment, data[i:end])
		fragments = append(fragments, fragment)
	}

	return fragments
}

// Creates the custom participant_id extension
func (p *H264Packetizer) createParticipantIDExtension() []Extension {
	data := make([]byte, 4)
	binary.LittleEndian.PutUint32(data, p.ParticipantID)
	return []Extension{
		{
			ID:   1, // Custom extension ID
			Data: data,
		},
	}
}

// Marshal serializes an RTP packet to bytes
func (pkt *RTPPacket) Marshal() []byte {
	headerLen := 12 // Fixed RTP header
	if pkt.Header.Extension && len(pkt.Header.Extensions) > 0 {
		headerLen += 4 + len(pkt.Header.Extensions[0].Data) // Extension header
	}

	buf := make([]byte, headerLen+len(pkt.Payload))

	// First byte: V=2, P, X, CC
	buf[0] = 2 << 6 // Version = 2
	if pkt.Header.Padding {
		buf[0] |= 0x20
	}
	if pkt.Header.Extension {
		buf[0] |= 0x10
	}
	buf[0] |= pkt.Header.CSRCCount & 0x0F

	// Second byte: M, PT
	buf[1] = pkt.Header.PayloadType & 0x7F
	if pkt.Header.Marker {
		buf[1] |= 0x80
	}

	// Sequence number (16 bits)
	binary.BigEndian.PutUint16(buf[2:4], pkt.Header.SequenceNumber)

	// Timestamp (32 bits)
	binary.BigEndian.PutUint32(buf[4:8], pkt.Header.Timestamp)

	// SSRC (32 bits)
	binary.BigEndian.PutUint32(buf[8:12], pkt.Header.SSRC)

	offset := 12

	// Extensions
	if pkt.Header.Extension && len(pkt.Header.Extensions) > 0 {
		ext := pkt.Header.Extensions[0]
		binary.BigEndian.PutUint16(buf[offset:offset+2], uint16(ext.ID))
		binary.BigEndian.PutUint16(buf[offset+2:offset+4], uint16(len(ext.Data)/4))
		offset += 4
		copy(buf[offset:], ext.Data)
		offset += len(ext.Data)
	}

	// Payload
	copy(buf[offset:], pkt.Payload)

	return buf
}

// Unmarshal parses an RTP packet from bytes
func Unmarshal(data []byte) (*RTPPacket, error) {
	if len(data) < 12 {
		return nil, ErrPacketTooShort
	}

	pkt := &RTPPacket{}

	pkt.Header.Version = (data[0] >> 6) & 0x03
	pkt.Header.Padding = (data[0] & 0x20) != 0
	pkt.Header.Extension = (data[0] & 0x10) != 0
	pkt.Header.CSRCCount = data[0] & 0x0F

	pkt.Header.Marker = (data[1] & 0x80) != 0
	pkt.Header.PayloadType = data[1] & 0x7F

	pkt.Header.SequenceNumber = binary.BigEndian.Uint16(data[2:4])
	pkt.Header.Timestamp = binary.BigEndian.Uint32(data[4:8])
	pkt.Header.SSRC = binary.BigEndian.Uint32(data[8:12])

	offset := 12

	// Skip CSRCs
	offset += int(pkt.Header.CSRCCount) * 4

	// Parse extension
	if pkt.Header.Extension {
		if len(data) < offset+4 {
			return nil, ErrPacketTooShort
		}
		extID := binary.BigEndian.Uint16(data[offset : offset+2])
		extLen := binary.BigEndian.Uint16(data[offset+2:offset+4]) * 4
		offset += 4

		if len(data) < offset+int(extLen) {
			return nil, ErrPacketTooShort
		}

		pkt.Header.Extensions = []Extension{
			{
				ID:   uint8(extID),
				Data: data[offset : offset+int(extLen)],
			},
		}
		offset += int(extLen)
	}

	pkt.Payload = data[offset:]

	return pkt, nil
}

type rtpError string

func (e rtpError) Error() string { return string(e) }

const (
	ErrPacketTooShort rtpError = "rtp: packet too short"
)
