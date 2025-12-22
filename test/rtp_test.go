package main

import (
	"bytes"
	"testing"

	"github.com/MdSadiqMd/AV-Chaos-Monkey/pkg/rtp"
)

func TestPacketizeSingleNALU(t *testing.T) {
	p := rtp.NewH264Packetizer(42, 96, 90000)

	nalu := []byte{0x65, 0x01, 0x02, 0x03}
	seq := uint16(100)
	ts := uint32(12345)
	t.Logf("Input NALU size=%d seq=%d ts=%d", len(nalu), seq, ts)

	packets := p.Packetize(nalu, seq, ts)
	t.Logf("Generated packets=%d", len(packets))

	if len(packets) != 1 {
		t.Fatalf("expected 1 packet, got %d", len(packets))
	}

	pkt := packets[0]
	t.Logf(
		"RTP Header: seq=%d ts=%d marker=%v pt=%d ssrc=%d",
		pkt.Header.SequenceNumber,
		pkt.Header.Timestamp,
		pkt.Header.Marker,
		pkt.Header.PayloadType,
		pkt.Header.SSRC,
	)

	if pkt.Header.SequenceNumber != seq {
		t.Errorf("expected sequence %d, got %d", seq, pkt.Header.SequenceNumber)
	}

	if !pkt.Header.Marker {
		t.Errorf("expected marker bit to be set")
	}

	if !bytes.Equal(pkt.Payload, nalu) {
		t.Errorf("payload mismatch")
	}
	t.Logf("Payload bytes=%v", pkt.Payload)

	if len(pkt.Header.Extensions) != 1 {
		t.Fatalf("expected 1 extension, got %d", len(pkt.Header.Extensions))
	}
	t.Logf("Extension ID=%d Data=%v", pkt.Header.Extensions[0].ID, pkt.Header.Extensions[0].Data)
}

func TestPacketizeFragmentedNALU(t *testing.T) {
	p := rtp.NewH264Packetizer(7, 96, 90000)

	nalu := make([]byte, 2000)
	nalu[0] = 0x65
	for i := 1; i < len(nalu); i++ {
		nalu[i] = byte(i)
	}

	t.Logf("Large NALU size=%d", len(nalu))

	packets := p.Packetize(nalu, 10, 999)
	t.Logf("Fragmented into %d RTP packets", len(packets))

	if len(packets) < 2 {
		t.Fatalf("expected fragmented packets, got %d", len(packets))
	}

	for i, pkt := range packets {
		t.Logf(
			"Packet[%d]: seq=%d marker=%v payloadSize=%d FU-indicator=0x%x FU-header=0x%x",
			i,
			pkt.Header.SequenceNumber,
			pkt.Header.Marker,
			len(pkt.Payload),
			pkt.Payload[0],
			pkt.Payload[1],
		)

		if pkt.Header.SequenceNumber != 10+uint16(i) {
			t.Errorf("sequence mismatch at packet %d", i)
		}

		if i == len(packets)-1 {
			if !pkt.Header.Marker {
				t.Errorf("last packet should have marker bit set")
			}
		} else if pkt.Header.Marker {
			t.Errorf("only last packet should have marker bit set")
		}

		if pkt.Payload[0]&0x1F != 28 {
			t.Errorf("expected FU-A packet")
		}
	}
}

func TestMarshalUnmarshalRoundTrip(t *testing.T) {
	p := rtp.NewH264Packetizer(99, 97, 90000)

	nalu := []byte{0x61, 0xAA, 0xBB, 0xCC}
	packets := p.Packetize(nalu, 555, 777)

	raw := packets[0].Marshal()
	t.Logf("Marshalled packet size=%d bytes", len(raw))

	parsed, err := rtp.Unmarshal(raw)
	if err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}

	t.Logf(
		"Parsed RTP: seq=%d ts=%d pt=%d marker=%v payloadSize=%d",
		parsed.Header.SequenceNumber,
		parsed.Header.Timestamp,
		parsed.Header.PayloadType,
		parsed.Header.Marker,
		len(parsed.Payload),
	)

	if parsed.Header.SequenceNumber != packets[0].Header.SequenceNumber {
		t.Errorf("sequence mismatch after unmarshal")
	}

	if parsed.Header.Timestamp != packets[0].Header.Timestamp {
		t.Errorf("timestamp mismatch after unmarshal")
	}

	if !bytes.Equal(parsed.Payload, packets[0].Payload) {
		t.Errorf("payload mismatch after unmarshal")
	}

	if len(parsed.Header.Extensions) != 1 {
		t.Fatalf("expected extension after unmarshal")
	}
}

func TestUnmarshalTooShort(t *testing.T) {
	t.Log("Testing short packet unmarshal")

	_, err := rtp.Unmarshal([]byte{0x00, 0x01})
	if err == nil {
		t.Fatalf("expected error for short packet")
	}

	t.Logf("Received expected error: %v", err)

	if err != rtp.ErrPacketTooShort {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestParticipantIDExtensionContent(t *testing.T) {
	p := rtp.NewH264Packetizer(1234, 96, 90000)

	packets := p.Packetize([]byte{0x61, 0x01}, 1, 1)
	ext := packets[0].Header.Extensions[0]

	t.Logf("Participant extension: ID=%d Data=%v", ext.ID, ext.Data)

	if ext.ID != 1 {
		t.Errorf("expected extension ID 1, got %d", ext.ID)
	}

	if len(ext.Data) != 4 {
		t.Fatalf("expected 4 bytes extension data")
	}
}
