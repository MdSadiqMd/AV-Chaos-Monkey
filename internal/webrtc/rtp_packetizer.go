package webrtc

import (
	"github.com/MdSadiqMd/AV-Chaos-Monkey/pkg/constants"
	"github.com/pion/rtp"
)

// converts H.264 NALU to RTP packets with proper fragmentation
func packetizeH264(nalu []byte, seq uint16, timestamp uint32, ssrc uint32) []*rtp.Packet {
	packets := []*rtp.Packet{}
	mtu := 1200

	if len(nalu) <= mtu {
		pkt := &rtp.Packet{
			Header: rtp.Header{
				Version:        2,
				Padding:        false,
				Extension:      false,
				Marker:         true,
				PayloadType:    constants.RTPPayloadType,
				SequenceNumber: seq,
				Timestamp:      timestamp,
				SSRC:           ssrc * 1000,
			},
			Payload: nalu,
		}
		packets = append(packets, pkt)
	} else {
		naluType := nalu[0] & 0x1F
		nri := nalu[0] & 0x60
		data := nalu[1:]

		for i := 0; i < len(data); i += mtu - 2 {
			end := i + mtu - 2
			end = min(end, len(data))

			fuIndicator := byte(28) | nri
			fuHeader := naluType
			if i == 0 {
				fuHeader |= 0x80
			}
			if end >= len(data) {
				fuHeader |= 0x40
			}

			payload := make([]byte, 2+end-i)
			payload[0] = fuIndicator
			payload[1] = fuHeader
			copy(payload[2:], data[i:end])

			pkt := &rtp.Packet{
				Header: rtp.Header{
					Version:        2,
					Padding:        false,
					Extension:      false,
					Marker:         end >= len(data),
					PayloadType:    constants.RTPPayloadType,
					SequenceNumber: seq,
					Timestamp:      timestamp,
					SSRC:           ssrc * 1000,
				},
				Payload: payload,
			}

			packets = append(packets, pkt)
			seq++
		}
	}

	return packets
}
