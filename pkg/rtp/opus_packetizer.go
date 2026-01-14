package rtp

import "encoding/binary"

// Packetizes Opus audio into RTP packets
type OpusPacketizer struct {
	SSRC          uint32
	PayloadType   uint8
	ClockRate     uint32
	ParticipantID uint32
}

func NewOpusPacketizer(participantID uint32, payloadType uint8, clockRate uint32) *OpusPacketizer {
	return &OpusPacketizer{
		SSRC:          participantID*1000 + 1, // Different SSRC from video
		PayloadType:   payloadType,
		ClockRate:     clockRate,
		ParticipantID: participantID,
	}
}

// Creates an RTP packet from Opus audio data
func (p *OpusPacketizer) Packetize(opusData []byte, sequenceNumber uint16, timestamp uint32) *RTPPacket {
	return &RTPPacket{
		Header: RTPHeader{
			Version:        2,
			Padding:        false,
			Extension:      true,
			Marker:         true, // Opus packets are self-contained
			PayloadType:    p.PayloadType,
			SequenceNumber: sequenceNumber,
			Timestamp:      timestamp,
			SSRC:           p.SSRC,
			Extensions:     p.createParticipantIDExtension(),
		},
		Payload: opusData,
	}
}

func (p *OpusPacketizer) createParticipantIDExtension() []Extension {
	data := make([]byte, 4)
	binary.LittleEndian.PutUint32(data, p.ParticipantID)
	return []Extension{
		{
			ID:   1, // Custom RTP header extension ID
			Data: data,
		},
	}
}
