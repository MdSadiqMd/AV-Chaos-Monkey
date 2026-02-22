# AV-Chaos-Monkey: Architecture Decisions

> A chaos testing tool for video/audio conferencing. Simulates virtual participants sending real RTP packets (H.264 video, Opus audio) and injects network chaos.

---

## Quick Reference

| Decision | Choice | Why |
|----------|--------|-----|
| Language | Go 1.24 | Goroutines for 1500+ participants, Pion WebRTC, cross-compilation |
| WebRTC | Pion | Pure Go, no CGO, production-grade ICE/DTLS/SRTP |
| Build System | Nix Flakes | Reproducible builds, cross-compilation to any architecture |
| Scaling | StatefulSet + Partitioning | `participant_id % total_partitions == partition_id` |
| UDP Transport | TCP relay chain | UDP can't cross Docker/Kind boundaries reliably |
| NAT Traversal | Coturn TURN (HPA 1-10) | Pod IPs not routable externally, auto-scales with load |
| Video Codec | H.264 (Annex B) | Universal support, FU-A fragmentation for RTP |
| Audio Codec | Opus (OGG container) | Low latency, 20ms frames, self-contained packets |
| Chaos Injection | Application-level | OS-level tc/netem requires NET_ADMIN capability |
| Client Package | pkg/client | WebRTC connection management, K8s discovery, auto-scaling |
| Spike Distribution | 5 strategies | even, random, front_loaded, back_loaded, legacy |
| UDP Receiver | 3-stage relay chain | Aggregates all pods, works in K8s |
| WebRTC Receiver | Multi-receiver with port-forwards | Per-participant stats, connects to all pods |
| Partition Discovery | Modulo-based | Each pod only handles its partition |
| Example Code | Self-contained | Simple over DRY for examples |
| Media Source | Real video file | rick-roll.mp4 with FFmpeg extraction |
| Example Languages | Go, JavaScript, Python, Rust | Go recommended, others UDP-only |
| TURN Server | Coturn in-cluster + public fallback | Essential for WebRTC NAT traversal |
| Integration Guide | Quick start with examples | Step-by-step for all languages |
| Example Status | Go fully working | Other languages UDP-only due to WebRTC library limitations |

---

## 1. Why Go + Pion WebRTC

**Problem:** Need to simulate 1500+ concurrent video participants with real RTP packets.

**Approaches tried:**
1. ❌ Shell scripts → Too slow, no concurrency
2. ❌ Python (aiortc) → ICE implementation incomplete, TURN relay issues
3. ❌ Node.js (werift) → Same ICE/TURN problems
4. ❌ Rust (webrtc-rs) → Immature, missing features
5. ✅ Go (Pion) → Pure Go, no CGO, works in containers, active community

**Final:** Go with Pion WebRTC. Handles 150 participants per pod × 10 pods = 1500 total.

---

## 2. Why Kubernetes StatefulSet with Partitioning

**Problem:** Single pod can't handle 1500 participants (CPU/memory limits).

**Approaches tried:**
1. ❌ Single pod → OOM at ~200 participants
2. ❌ Deployment with random distribution → No predictable assignment
3. ✅ StatefulSet with modulo partitioning → Stable names, predictable assignment

**How it works:**
```
participant_id % total_partitions == partition_id

Example with 10 pods:
- orchestrator-0 gets: 1010, 1020, 1030... (IDs where ID % 10 == 0)
- orchestrator-1 gets: 1001, 1011, 1021... (IDs where ID % 10 == 1)
```

**Why StatefulSet over Deployment:**
- Stable pod names (orchestrator-0, orchestrator-1, etc.)
- Ordered startup/shutdown
- Predictable partition assignment
- Metrics correlation by pod name

---

## 3. Why UDP-to-TCP Relay Chain

**Problem:** UDP packets from Kubernetes pods can't reach local machine.

**Approaches tried:**
1. ❌ Direct UDP to host.docker.internal → Packets silently dropped
2. ❌ UDP to node IP → Blocked by Docker networking
3. ❌ hostNetwork pods → Port conflicts (multiple pods bind to :8080)
4. ❌ NodePort service → Complex, still NAT issues
5. ✅ UDP→TCP relay inside cluster + kubectl port-forward + local TCP→UDP relay

**Final architecture:**
```
Orchestrator pods → UDP → udp-relay pod → TCP → kubectl port-forward → local relay → UDP receiver
                    ↓                      ↓                           ↓
              (inside cluster)      (crosses boundary)           (localhost:5002)
```

**Why this works:**
- TCP has connection tracking, can be port-forwarded
- UDP is stateless, Docker can't route it reliably
- 2-byte length prefix for TCP framing (stream → packets)

---

## 4. Why TURN Server (Coturn)

**Problem:** WebRTC connections fail because pod IPs (10.244.x.x) aren't routable from outside cluster.

**Approaches tried:**
1. ❌ STUN only → Works for public IPs, not for pod IPs
2. ❌ Direct connection → Pod IPs not reachable
3. ✅ TURN relay → Relays all traffic through accessible server

**Configuration:**
- Primary: In-cluster coturn service
- Fallback: Public TURN (openrelay.metered.ca)

---

## 5. Audio/Video Processing Pipeline

### Overview: From Media File to RTP Packets

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                        MEDIA PROCESSING PIPELINE                             │
├─────────────────────────────────────────────────────────────────────────────┤
│                                                                              │
│  ┌──────────────┐    FFmpeg     ┌──────────────┐                            │
│  │ rick-roll.mp4│ ────────────► │ video.h264   │  (Annex B format)          │
│  │              │               │ audio.opus   │  (OGG container)           │
│  └──────────────┘               └──────────────┘                            │
│                                        │                                     │
│                                        ▼                                     │
│  ┌──────────────────────────────────────────────────────────────────────┐   │
│  │                    MediaSource (Singleton)                           │   │
│  │  ┌─────────────────────┐    ┌─────────────────────┐                 │   │
│  │  │ videoNALs []NALUnit │    │ audioFrames []Opus  │                 │   │
│  │  │ (cached in memory)  │    │ (cached in memory)  │                 │   │
│  │  └─────────────────────┘    └─────────────────────┘                 │   │
│  └──────────────────────────────────────────────────────────────────────┘   │
│                                        │                                     │
│                    ┌───────────────────┴───────────────────┐                │
│                    ▼                                       ▼                │
│  ┌─────────────────────────────────┐    ┌─────────────────────────────────┐│
│  │     H264Packetizer              │    │     OpusPacketizer              ││
│  │  - FU-A fragmentation           │    │  - Single packet per frame      ││
│  │  - MTU: 1200 bytes              │    │  - 20ms frame duration          ││
│  │  - Payload Type: 96             │    │  - Payload Type: 111            ││
│  │  - Clock Rate: 90000 Hz         │    │  - Clock Rate: 48000 Hz         ││
│  └─────────────────────────────────┘    └─────────────────────────────────┘│
│                    │                                       │                │
│                    └───────────────────┬───────────────────┘                │
│                                        ▼                                     │
│  ┌──────────────────────────────────────────────────────────────────────┐   │
│  │                         RTP Packets                                  │   │
│  │  - Custom extension with participant ID                              │   │
│  │  - Sent via UDP (raw) or WebRTC (SRTP encrypted)                    │   │
│  └──────────────────────────────────────────────────────────────────────┘   │
│                                                                              │
└─────────────────────────────────────────────────────────────────────────────┘
```

---

## 6. RTP Packet Structure (RFC 3550)

### RTP Header Format

```
 0                   1                   2                   3
 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
|V=2|P|X|  CC   |M|     PT      |       Sequence Number         |
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
|                           Timestamp                           |
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
|                             SSRC                              |
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
|      Extension ID = 1         |       Extension Length        |
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
|                    Participant ID (4 bytes)                   |
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
|                         Payload Data                          |
|                             ...                               |
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
```

### Field Descriptions

| Field | Bits | Our Value | Purpose |
|-------|------|-----------|---------|
| V (Version) | 2 | 2 | Always 2 for RTP |
| P (Padding) | 1 | 0 | No padding used |
| X (Extension) | 1 | 1 | We use extension for participant ID |
| CC (CSRC Count) | 4 | 0 | No contributing sources |
| M (Marker) | 1 | varies | 1 = last packet of frame |
| PT (Payload Type) | 7 | 96/111 | 96=H.264, 111=Opus |
| Sequence Number | 16 | increments | Detect packet loss |
| Timestamp | 32 | increments | Sync audio/video |
| SSRC | 32 | participant_id × 1000 | Unique stream identifier |

### Why Custom Extension for Participant ID

**Problem:** Need to identify which participant sent each packet.

**Options considered:**
1. ❌ Use SSRC directly → SSRC is 32-bit, but we want clean participant IDs
2. ❌ Embed in payload → Would require modifying codec data
3. ✅ RTP header extension → Standard mechanism, doesn't touch payload

**Implementation:**
```go
// Extension ID: 1, Data: 4 bytes (little-endian uint32)
func createParticipantIDExtension(participantID uint32) []Extension {
    data := make([]byte, 4)
    binary.LittleEndian.PutUint32(data, participantID)
    return []Extension{{ID: 1, Data: data}}
}
```

---

## 7. H.264 Video Processing

### H.264 Annex B Format

H.264 video is stored in "Annex B" format, where NAL units are separated by start codes.

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                        H.264 ANNEX B STREAM                                  │
├─────────────────────────────────────────────────────────────────────────────┤
│                                                                              │
│  ┌─────────┐ ┌─────────────────┐ ┌─────────┐ ┌─────────────────┐           │
│  │Start    │ │    NAL Unit 1   │ │Start    │ │    NAL Unit 2   │  ...      │
│  │Code     │ │    (SPS/PPS/    │ │Code     │ │    (IDR/Non-IDR │           │
│  │00 00 01 │ │     Slice)      │ │00 00 01 │ │     Slice)      │           │
│  └─────────┘ └─────────────────┘ └─────────┘ └─────────────────┘           │
│                                                                              │
│  Start Code: 0x000001 (3 bytes) or 0x00000001 (4 bytes)                     │
│                                                                              │
└─────────────────────────────────────────────────────────────────────────────┘
```

### NAL Unit Types We Handle

| NAL Type | Value | Description | How We Handle |
|----------|-------|-------------|---------------|
| Non-IDR Slice | 1 (0x01) | P-frame data | Packetize normally |
| IDR Slice | 5 (0x05) | Keyframe (I-frame) | Packetize normally |
| SEI | 6 (0x06) | Supplemental info | Pass through |
| SPS | 7 (0x07) | Sequence Parameter Set | Required for decoding |
| PPS | 8 (0x08) | Picture Parameter Set | Required for decoding |
| FU-A | 28 (0x1C) | Fragmentation Unit | We CREATE these |

### NAL Unit Header Structure

```
+---------------+
|0|1|2|3|4|5|6|7|
+-+-+-+-+-+-+-+-+
|F|NRI|  Type   |
+---------------+

F (Forbidden): Always 0
NRI (Nal Ref Idc): Priority (0-3), higher = more important
Type: NAL unit type (1-31)
```

### FU-A Fragmentation (RFC 6184)

**Problem:** NAL units can be larger than MTU (1200 bytes). Need to fragment.

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                        FU-A FRAGMENTATION                                    │
├─────────────────────────────────────────────────────────────────────────────┤
│                                                                              │
│  Original NAL Unit (5000 bytes):                                            │
│  ┌────────────────────────────────────────────────────────────────────────┐ │
│  │ NAL Header │                    NAL Payload                            │ │
│  │  (1 byte)  │                   (4999 bytes)                            │ │
│  └────────────────────────────────────────────────────────────────────────┘ │
│                                                                              │
│  Fragmented into FU-A packets:                                              │
│                                                                              │
│  Packet 1 (Start):                                                          │
│  ┌──────────────┬──────────────┬─────────────────────────────────────────┐ │
│  │ FU Indicator │  FU Header   │           Fragment 1 (~1198 bytes)      │ │
│  │ 0x7C (28|NRI)│ 0x85 (S|Type)│                                         │ │
│  └──────────────┴──────────────┴─────────────────────────────────────────┘ │
│                                                                              │
│  Packet 2 (Middle):                                                         │
│  ┌──────────────┬──────────────┬─────────────────────────────────────────┐ │
│  │ FU Indicator │  FU Header   │           Fragment 2 (~1198 bytes)      │ │
│  │ 0x7C (28|NRI)│ 0x05 (Type)  │                                         │ │
│  └──────────────┴──────────────┴─────────────────────────────────────────┘ │
│                                                                              │
│  Packet N (End):                                                            │
│  ┌──────────────┬──────────────┬─────────────────────────────────────────┐ │
│  │ FU Indicator │  FU Header   │           Fragment N (remaining)        │ │
│  │ 0x7C (28|NRI)│ 0x45 (E|Type)│                                         │ │
│  └──────────────┴──────────────┴─────────────────────────────────────────┘ │
│                                                                              │
│  FU Indicator: Type=28 (FU-A) | NRI from original NAL                       │
│  FU Header: S=Start bit | E=End bit | R=Reserved | Type=Original NAL type   │
│                                                                              │
└─────────────────────────────────────────────────────────────────────────────┘
```

### FU Header Bits

```
+---------------+
|0|1|2|3|4|5|6|7|
+-+-+-+-+-+-+-+-+
|S|E|R|  Type   |
+---------------+

S (Start): 1 = first fragment
E (End): 1 = last fragment
R (Reserved): Always 0
Type: Original NAL unit type
```

### Our H.264 Packetizer Logic

```go
func (p *H264Packetizer) Packetize(nalu []byte, seq uint16, ts uint32) []*RTPPacket {
    if len(nalu) <= MTU {
        // Single NAL unit packet - send as-is
        return []*RTPPacket{createSingleNALPacket(nalu, seq, ts)}
    }
    
    // FU-A fragmentation needed
    naluType := nalu[0] & 0x1F  // Extract type from first byte
    nri := nalu[0] & 0x60       // Extract NRI bits
    data := nalu[1:]            // Skip NAL header (will be in FU header)
    
    var packets []*RTPPacket
    for i := 0; i < len(data); i += (MTU - 2) {
        fuIndicator := byte(28) | nri  // Type 28 = FU-A
        fuHeader := naluType
        if i == 0 { fuHeader |= 0x80 }           // Start bit
        if i + MTU - 2 >= len(data) { fuHeader |= 0x40 }  // End bit
        
        packets = append(packets, createFUAPacket(fuIndicator, fuHeader, data[i:end], seq))
        seq++
    }
    return packets
}
```

---

## 8. Opus Audio Processing

### Opus in OGG Container

Opus audio is stored in OGG container format. We parse OGG pages to extract raw Opus packets.

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                          OGG CONTAINER FORMAT                                │
├─────────────────────────────────────────────────────────────────────────────┤
│                                                                              │
│  ┌─────────────────────────────────────────────────────────────────────────┐│
│  │                         OGG Page Header (27 bytes)                      ││
│  ├─────────────────────────────────────────────────────────────────────────┤│
│  │ Bytes 0-3:   "OggS" (magic signature)                                   ││
│  │ Byte 4:      Version (always 0)                                         ││
│  │ Byte 5:      Header type flags                                          ││
│  │ Bytes 6-13:  Granule position (64-bit, audio sample count)              ││
│  │ Bytes 14-17: Serial number (stream ID)                                  ││
│  │ Bytes 18-21: Page sequence number                                       ││
│  │ Bytes 22-25: CRC checksum                                               ││
│  │ Byte 26:     Number of segments                                         ││
│  └─────────────────────────────────────────────────────────────────────────┘│
│                                                                              │
│  ┌─────────────────────────────────────────────────────────────────────────┐│
│  │                    Segment Table (N bytes)                              ││
│  │  Each byte = segment size (0-255). 255 means "continues in next"       ││
│  └─────────────────────────────────────────────────────────────────────────┘│
│                                                                              │
│  ┌─────────────────────────────────────────────────────────────────────────┐│
│  │                    Page Data (sum of segment sizes)                     ││
│  │  Contains one or more Opus packets                                      ││
│  └─────────────────────────────────────────────────────────────────────────┘│
│                                                                              │
└─────────────────────────────────────────────────────────────────────────────┘
```

### OGG Stream Structure

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                          OGG OPUS STREAM                                     │
├─────────────────────────────────────────────────────────────────────────────┤
│                                                                              │
│  Page 1: OpusHead (ID Header)                                               │
│  ┌─────────────────────────────────────────────────────────────────────────┐│
│  │ "OpusHead" │ Version │ Channels │ Pre-skip │ Sample Rate │ Gain │ Map  ││
│  │  (8 bytes) │ (1)     │ (1)      │ (2)      │ (4)         │ (2)  │ (1)  ││
│  └─────────────────────────────────────────────────────────────────────────┘│
│  granule_position = 0 (header packet)                                       │
│                                                                              │
│  Page 2: OpusTags (Comment Header)                                          │
│  ┌─────────────────────────────────────────────────────────────────────────┐│
│  │ "OpusTags" │ Vendor String │ User Comments                              ││
│  └─────────────────────────────────────────────────────────────────────────┘│
│  granule_position = 0 (header packet)                                       │
│                                                                              │
│  Pages 3+: Audio Data                                                       │
│  ┌─────────────────────────────────────────────────────────────────────────┐│
│  │ Opus Packet 1 │ Opus Packet 2 │ Opus Packet 3 │ ...                     ││
│  │ (20ms audio)  │ (20ms audio)  │ (20ms audio)  │                         ││
│  └─────────────────────────────────────────────────────────────────────────┘│
│  granule_position > 0 (audio sample count)                                  │
│                                                                              │
└─────────────────────────────────────────────────────────────────────────────┘
```

### Why We Skip Header Packets

```go
// In OggReader.nextPacketLocked():
granulePos := binary.LittleEndian.Uint64(header[6:14])
if granulePos == 0 {
    // This is a header packet (OpusHead or OpusTags), skip it
    return r.nextPacketLocked()  // Recursively get next
}
// granulePos > 0 means actual audio data
```

**Why:** OpusHead and OpusTags are metadata, not audio. We only want the raw Opus frames for RTP.

### Opus Frame Timing

| Parameter | Value | Why |
|-----------|-------|-----|
| Frame Duration | 20ms | Standard Opus frame size |
| Sample Rate | 48000 Hz | Opus native rate |
| Samples per Frame | 960 | 48000 × 0.020 = 960 |
| RTP Timestamp Increment | 960 | One frame = 960 samples |

### Opus RTP Packetization

Unlike H.264, Opus packets are small enough to fit in a single RTP packet (no fragmentation needed).

```go
func (p *OpusPacketizer) Packetize(opusData []byte, seq uint16, ts uint32) *RTPPacket {
    return &RTPPacket{
        Header: RTPHeader{
            Version:        2,
            Marker:         true,  // Opus packets are self-contained
            PayloadType:    111,   // Opus
            SequenceNumber: seq,
            Timestamp:      ts,
            SSRC:           p.SSRC,
            Extensions:     p.createParticipantIDExtension(),
        },
        Payload: opusData,  // Raw Opus frame, no modification
    }
}
```

---

## 9. WebRTC vs UDP: Two Delivery Paths

### Comparison

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                    WEBRTC vs UDP DELIVERY                                    │
├─────────────────────────────────────────────────────────────────────────────┤
│                                                                              │
│  ┌─────────────────────────────────┐  ┌─────────────────────────────────┐   │
│  │         WebRTC Path             │  │          UDP Path               │   │
│  ├─────────────────────────────────┤  ├─────────────────────────────────┤   │
│  │                                 │  │                                 │   │
│  │  RTP Packet                     │  │  RTP Packet                     │   │
│  │       │                         │  │       │                         │   │
│  │       ▼                         │  │       ▼                         │   │
│  │  SRTP Encryption                │  │  (no encryption)                │   │
│  │       │                         │  │       │                         │   │
│  │       ▼                         │  │       ▼                         │   │
│  │  DTLS Handshake                 │  │  (no handshake)                 │   │
│  │       │                         │  │       │                         │   │
│  │       ▼                         │  │       ▼                         │   │
│  │  ICE Connectivity               │  │  Direct UDP socket              │   │
│  │  (STUN/TURN)                    │  │       │                         │   │
│  │       │                         │  │       ▼                         │   │
│  │       ▼                         │  │  UDP-to-TCP Relay               │   │
│  │  TURN Relay                     │  │       │                         │   │
│  │  (if needed)                    │  │       ▼                         │   │
│  │       │                         │  │  kubectl port-forward           │   │
│  │       ▼                         │  │       │                         │   │
│  │  Receiver                       │  │       ▼                         │   │
│  │                                 │  │  TCP-to-UDP Relay               │   │
│  │                                 │  │       │                         │   │
│  │                                 │  │       ▼                         │   │
│  │                                 │  │  Receiver                       │   │
│  └─────────────────────────────────┘  └─────────────────────────────────┘   │
│                                                                              │
│  Pros:                              │  Pros:                                │
│  - Works through NAT                │  - Lower latency                      │
│  - Encrypted                        │  - Simpler debugging                  │
│  - Standard browser compatible      │  - All participants on one socket     │
│                                     │                                       │
│  Cons:                              │  Cons:                                │
│  - Higher latency (TURN relay)      │  - Needs relay chain in K8s           │
│  - Complex setup                    │  - No encryption                      │
│  - One connection per participant   │  - Can't cross NAT directly           │
│                                                                              │
└─────────────────────────────────────────────────────────────────────────────┘
```

### WebRTC Connection Flow

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                    WEBRTC SIGNALING FLOW                                     │
├─────────────────────────────────────────────────────────────────────────────┤
│                                                                              │
│  Orchestrator (Server)                    Receiver (Client)                 │
│         │                                        │                          │
│         │  1. GET /sdp/{participant_id}          │                          │
│         │◄───────────────────────────────────────│                          │
│         │                                        │                          │
│         │  2. SDP Offer (video + audio tracks)   │                          │
│         │────────────────────────────────────────►                          │
│         │                                        │                          │
│         │  3. POST /answer/{participant_id}      │                          │
│         │◄───────────────────────────────────────│                          │
│         │     SDP Answer                         │                          │
│         │                                        │                          │
│         │  4. ICE Candidates (trickle)           │                          │
│         │◄──────────────────────────────────────►│                          │
│         │                                        │                          │
│         │  5. DTLS Handshake                     │                          │
│         │◄──────────────────────────────────────►│                          │
│         │                                        │                          │
│         │  6. SRTP Media Flow                    │                          │
│         │────────────────────────────────────────►                          │
│         │                                        │                          │
└─────────────────────────────────────────────────────────────────────────────┘
```

### ICE Server Configuration

```go
iceServers := []webrtc.ICEServer{
    // STUN servers (discover public IP)
    {URLs: []string{"stun:stun.l.google.com:19302"}},
    {URLs: []string{"stun:stun1.l.google.com:19302"}},
    
    // In-cluster TURN (primary)
    {URLs: []string{"turn:coturn-lb:3478"},
     Username: "webrtc", Credential: "webrtc123"},
    
    // Public TURN (fallback)
    {URLs: []string{"turn:openrelay.metered.ca:443"},
     Username: "openrelayproject", Credential: "openrelayproject"},
}
```

---

## 10. Timing and Synchronization

### Video Timing

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                        VIDEO TIMING                                          │
├─────────────────────────────────────────────────────────────────────────────┤
│                                                                              │
│  FPS = 60 (StreamingFPS constant)                                           │
│  Clock Rate = 90000 Hz (RTP standard for video)                             │
│                                                                              │
│  Frame Interval = 1000ms / 60 = 16.67ms                                     │
│  Timestamp Increment = 90000 / 60 = 1500 per frame                          │
│                                                                              │
│  Timeline:                                                                   │
│  ┌────────┬────────┬────────┬────────┬────────┬────────┐                   │
│  │Frame 0 │Frame 1 │Frame 2 │Frame 3 │Frame 4 │Frame 5 │                   │
│  │TS=0    │TS=1500 │TS=3000 │TS=4500 │TS=6000 │TS=7500 │                   │
│  └────────┴────────┴────────┴────────┴────────┴────────┘                   │
│  │◄─16.67ms─►│                                                              │
│                                                                              │
└─────────────────────────────────────────────────────────────────────────────┘
```

### Audio Timing

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                        AUDIO TIMING                                          │
├─────────────────────────────────────────────────────────────────────────────┤
│                                                                              │
│  Frame Duration = 20ms (Opus standard)                                      │
│  Clock Rate = 48000 Hz (Opus native)                                        │
│  Samples per Frame = 48000 × 0.020 = 960                                    │
│                                                                              │
│  Timeline:                                                                   │
│  ┌────────┬────────┬────────┬────────┬────────┬────────┐                   │
│  │Frame 0 │Frame 1 │Frame 2 │Frame 3 │Frame 4 │Frame 5 │                   │
│  │TS=0    │TS=960  │TS=1920 │TS=2880 │TS=3840 │TS=4800 │                   │
│  └────────┴────────┴────────┴────────┴────────┴────────┘                   │
│  │◄──20ms──►│                                                               │
│                                                                              │
└─────────────────────────────────────────────────────────────────────────────┘
```

### SSRC Assignment

```
Participant ID: 1001
├── Video SSRC: 1001 × 1000 = 1001000
└── Audio SSRC: 1001 × 1000 + 1 = 1001001

Participant ID: 1002
├── Video SSRC: 1002000
└── Audio SSRC: 1002001
```

**Why separate SSRCs:** Each media stream needs a unique SSRC. Video and audio are separate streams.

---

## 11. Chaos Injection Points

### Where Chaos is Injected

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                    CHAOS INJECTION POINTS                                    │
├─────────────────────────────────────────────────────────────────────────────┤
│                                                                              │
│  Media Source                                                                │
│       │                                                                      │
│       ▼                                                                      │
│  ┌─────────────────┐                                                        │
│  │ shouldDropFrame │ ◄── frame_drop spike                                   │
│  │ (before encode) │     Drops entire video frames                          │
│  └─────────────────┘                                                        │
│       │                                                                      │
│       ▼                                                                      │
│  Packetizer                                                                  │
│       │                                                                      │
│       ▼                                                                      │
│  ┌──────────────────┐                                                       │
│  │ shouldDropPacket │ ◄── rtp_packet_loss spike                             │
│  │ (after packetize)│     Drops individual RTP packets                      │
│  └──────────────────┘                                                       │
│       │                                                                      │
│       ▼                                                                      │
│  ┌─────────────────┐                                                        │
│  │ shouldMuteAudio │ ◄── audio_silence spike                                │
│  │ (audio only)    │     Sends silence frames (0xFC)                        │
│  └─────────────────┘                                                        │
│       │                                                                      │
│       ▼                                                                      │
│  UDP/WebRTC Send                                                             │
│                                                                              │
└─────────────────────────────────────────────────────────────────────────────┘
```

### Spike Implementation

```go
func (p *VirtualParticipant) shouldDropPacket() bool {
    p.activeSpikesMu.RLock()
    defer p.activeSpikesMu.RUnlock()
    
    for _, spike := range p.activeSpikes {
        if spike.Type == "rtp_packet_loss" {
            if lossPct, ok := spike.Params["loss_percentage"]; ok {
                var pct int
                fmt.Sscanf(lossPct, "%d", &pct)
                return rand.Intn(100) < pct  // Random drop based on percentage
            }
        }
    }
    return false
}
```

---

## 12. Receiver Packet Parsing

### How Receivers Extract Participant ID

```go
func parseRTPPacket(data []byte) (*RTPHeader, error) {
    // 1. Check minimum size (12 bytes for fixed header)
    if len(data) < 12 { return nil, fmt.Errorf("packet too short") }
    
    // 2. Parse fixed header
    header := &RTPHeader{}
    header.Version = (data[0] >> 6) & 0x03      // Bits 0-1
    extension := (data[0] >> 4) & 0x01          // Bit 3 (X flag)
    csrcCount := data[0] & 0x0F                 // Bits 4-7
    header.PayloadType = data[1] & 0x7F         // Bits 9-15
    header.Sequence = binary.BigEndian.Uint16(data[2:4])
    header.Timestamp = binary.BigEndian.Uint32(data[4:8])
    header.SSRC = binary.BigEndian.Uint32(data[8:12])
    
    // 3. Skip CSRC list (4 bytes each)
    offset := 12 + int(csrcCount)*4
    
    // 4. Parse extension header (if present)
    if extension != 0 {
        extID := binary.BigEndian.Uint16(data[offset : offset+2])
        extLen := binary.BigEndian.Uint16(data[offset+2:offset+4]) * 4
        
        // Our custom extension: ID=1, Length=4 bytes
        if extID == 1 && extLen == 4 {
            header.ParticipantID = binary.LittleEndian.Uint32(data[offset+4 : offset+8])
        }
        offset += 4 + int(extLen)
    }
    
    // 5. Rest is payload
    header.Payload = data[offset:]
    return header, nil
}
```

### Identifying Media Type

```go
switch header.PayloadType {
case 96:  // H.264 video
    stats.VideoPackets++
    // First byte of payload indicates NAL type
    if len(header.Payload) > 0 {
        nalType := header.Payload[0] & 0x1F
        stats.NALTypes[nalType]++
    }
case 111: // Opus audio
    stats.AudioPackets++
}
```

---

## 13. Why Real Media File (rick-roll.mp4)

**Problem:** Synthetic frames don't test real codec behavior.

**Approaches tried:**
1. ❌ Generated color frames → Unrealistic NAL unit sizes
2. ❌ Static image loop → No GOP structure
3. ✅ Real video file → Proper H.264 NAL units, keyframes, Opus audio

**How:** FFmpeg extracts H.264 and Opus at startup, cached in memory.

```bash
# Video extraction (Annex B format)
ffmpeg -i rick-roll.mp4 -c:v copy -bsf:v h264_mp4toannexb video.h264

# Audio extraction (OGG/Opus)
ffmpeg -i rick-roll.mp4 -c:a libopus -b:a 128k audio.opus
```

---

## 14. Application-Level Chaos (Not tc/netem)

**Problem:** Need portable chaos injection that works in any container.

**Approaches tried:**
1. ❌ tc/netem → Requires NET_ADMIN capability, fails with "Operation not permitted"
2. ❌ iptables rules → Same privilege issues
3. ✅ Application-level injection → Works everywhere, precise control

**Why OS-level failed:**
```bash
# Kubernetes pods lack NET_ADMIN capability by default
[⚠] Failed to apply real jitter: failed to add root qdisc: exit status 2
RTNETLINK answers: Operation not permitted
```

**Solution:** Implement chaos at application level before packet send.

### Spike Types and Implementation

#### 1. RTP Packet Loss
```go
func (p *VirtualParticipant) shouldDropPacket() bool {
    for _, spike := range p.activeSpikes {
        if spike.Type == "rtp_packet_loss" {
            lossPct := spike.Params["loss_percentage"]
            return rand.Intn(100) < lossPct  // Random drop
        }
    }
    return false
}
```

#### 2. Network Jitter (Application-Level Delay)
```go
func (p *VirtualParticipant) applyJitterDelay() {
    for _, spike := range p.activeSpikes {
        if spike.Type == "network_jitter" {
            baseLatency := spike.Params["base_latency_ms"]
            jitterStdDev := spike.Params["jitter_std_dev_ms"]
            
            // Box-Muller transform for normal distribution
            u1, u2 := rand.Float64(), rand.Float64()
            z := sqrt(-2*ln(u1)) * cos(2*π*u2)
            delay := baseLatency + int(z * jitterStdDev)
            
            // Clamp to 0-2000ms
            delay = max(0, min(delay, 2000))
            
            // Record for metrics and apply delay
            p.metrics.RecordSimulatedJitter(float64(delay))
            time.Sleep(time.Duration(delay) * time.Millisecond)
        }
    }
}
```

**Why Box-Muller:** Generates normally distributed random numbers, simulating real network jitter patterns.

#### 3. Bitrate Reduction
```go
func (p *VirtualParticipant) AddSpike(spike *pb.SpikeEvent) {
    if spike.Type == "bitrate_reduce" {
        newBitrate := spike.Params["new_bitrate_kbps"]
        p.VideoConfig.BitrateKbps = newBitrate  // Affects encoder
    }
}
```

#### 4. Frame Drop
```go
func (p *VirtualParticipant) shouldDropFrame() bool {
    for _, spike := range p.activeSpikes {
        if spike.Type == "frame_drop" {
            dropPct := spike.Params["drop_percentage"]
            return rand.Intn(100) < dropPct
        }
    }
    return false
}
```

### Jitter Metrics Tracking

**Challenge:** Jitter is calculated on receiver side (RFC 3550), but orchestrator only sends packets.

**Solution:** Simulate receiver-side jitter calculation using applied delays:

```go
func (m *RTPMetrics) RecordSimulatedJitter(jitterMs float64) {
    m.mu.Lock()
    defer m.mu.Unlock()
    
    // RFC 3550 jitter formula: J(i) = J(i-1) + (|D(i-1,i)| - J(i-1))/16
    m.jitter = m.jitter + (jitterMs-m.jitter)/16.0
    m.jitterSamples = append(m.jitterSamples, m.jitter)
    
    // Keep last 1000 samples for P99 calculation
    if len(m.jitterSamples) > 1000 {
        m.jitterSamples = m.jitterSamples[len(m.jitterSamples)-500:]
    }
}
```

### Packet Send Flow with Chaos

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                    PACKET SEND WITH CHAOS INJECTION                          │
├─────────────────────────────────────────────────────────────────────────────┤
│                                                                              │
│  Media Source                                                                │
│       │                                                                      │
│       ▼                                                                      │
│  ┌─────────────────┐                                                        │
│  │ shouldDropFrame │ ◄── frame_drop spike                                   │
│  │ (before encode) │     Drops entire video frames                          │
│  └─────────────────┘                                                        │
│       │                                                                      │
│       ▼                                                                      │
│  Packetizer (H264/Opus)                                                     │
│       │                                                                      │
│       ▼                                                                      │
│  ┌──────────────────┐                                                       │
│  │ shouldDropPacket │ ◄── rtp_packet_loss spike                             │
│  │ (after packetize)│     Drops individual RTP packets                      │
│  └──────────────────┘                                                       │
│       │                                                                      │
│       ▼                                                                      │
│  ┌─────────────────┐                                                        │
│  │ applyJitterDelay│ ◄── network_jitter spike                               │
│  │ (before send)   │     Sleeps 0-2000ms (normal distribution)              │
│  └─────────────────┘                                                        │
│       │                                                                      │
│       ▼                                                                      │
│  UDP/WebRTC Send                                                             │
│                                                                              │
└─────────────────────────────────────────────────────────────────────────────┘
```

### Distribution Strategies

Spikes are distributed across test duration using configurable strategies:

- `even` - Uniform distribution across test duration
- `random` - Random timing with minimum spacing
- `front_loaded` - Cluster in first 40%
- `back_loaded` - Cluster in last 40%
- `legacy` - Fixed interval ticker

### Configuration Example

```json
{
  "spikes": {
    "count": 70,
    "interval_seconds": 5,
    "types": {
      "network_jitter": {
        "weight": 50,
        "params": {
          "base_latency_ms_min": 50,
          "base_latency_ms_max": 150,
          "jitter_std_dev_ms_min": 100,
          "jitter_std_dev_ms_max": 300
        }
      },
      "rtp_packet_loss": {
        "weight": 20,
        "params": {
          "loss_percentage_min": 1,
          "loss_percentage_max": 25
        }
      }
    }
  }
}
```

### Why Application-Level Works Better

| Aspect | OS-Level (tc/netem) | Application-Level |
|--------|---------------------|-------------------|
| Privileges | Requires NET_ADMIN | No special privileges |
| Portability | Linux-specific | Works everywhere |
| Precision | Per-interface | Per-packet |
| Debugging | dmesg, tc show | Application logs |
| Metrics | External tools | Built-in tracking |
| Container Support | Needs privileged mode | Standard containers |

---

## 15. Why Keep Receivers Simple (Rejected Shared Metrics)

**Problem:** Wanted to DRY up duplicate code in UDP/WebRTC receivers.

**What happened:**
1. Created shared `internal/rtp/helpers.go` with `ParsePacket()` function
2. Refactored UDP receiver to use shared `pkg/metrics/stats.go`
3. Discovered 85-90% packet loss on UDP receiver

**Root cause:** The packet loss was REAL. Single UDP socket can't handle 3000+ concurrent streams (1500 participants × 2 tracks). Kernel buffer overflows. The old code just didn't measure it.

**Decision:** Keep receivers simple and self-contained. Use shared RTP parsing (`internal/rtp/helpers.go`) but keep example code minimal.

**What we share:**
- `internal/rtp/helpers.go` - `ParsePacket()` extracts participant ID from RTP extension
- `pkg/metrics/stats.go` - Metrics calculation (jitter, packet loss, MOS)

**What stays separate:**
- Example receivers remain simple and focused
- No complex dependencies in example code
- Easy to understand for users

**Lesson:** Measuring changes behavior. Adding metrics revealed a fundamental limitation, not a bug. Keep example code simple even if it means some duplication.

---

## 19. Key Learnings

1. **UDP doesn't cross container boundaries** - Convert to TCP for transport
2. **Partition-based scaling works** - Modulo assignment is simple and effective
3. **TURN servers are essential in Kubernetes** - STUN alone won't work
4. **Measuring reveals hidden problems** - Adding metrics can expose issues that were always there
5. **Go WebRTC (Pion) is production-ready** - Other languages' libraries are not (yet)
6. **Keep examples simple** - Self-contained is better than DRY for example code
7. **H.264 FU-A fragmentation is essential** - NAL units often exceed MTU
8. **Opus frames are self-contained** - No fragmentation needed, 20ms per frame
9. **Application-level chaos is more portable** - OS-level tc/netem requires NET_ADMIN capability
10. **Jitter is receiver-side metric** - Orchestrator can simulate but not measure true jitter without RTCP feedback
11. **Single UDP socket has limits** - Cannot reliably receive 3000+ concurrent streams without packet loss
12. **WebRTC in Kubernetes needs TURN** - Pod IPs not routable from outside cluster
13. **Nix enables true reproducibility** - Same build on any machine, any architecture
14. **Speed over resilience at scale** - Short timeouts and minimal retries prevent cascading failures
15. **Spike distribution strategies matter** - Different patterns test different failure scenarios
16. **hostNetwork causes port conflicts** - Each pod needs its own network namespace
17. **Partition-aware discovery is critical** - Must query correct pod for each participant ID
18. **Comments are documentation** - Removing "obvious" comments makes debugging harder later

---

## 17. Mathematical Foundations

### RFC 3550 Jitter Calculation

**Formula**:
```
J(i) = J(i-1) + (|D(i-1,i)| - J(i-1))/16

Where:
D(i-1,i) = (R(i) - R(i-1)) - (S(i) - S(i-1))
R(i) = arrival time of packet i (in RTP timestamp units)
S(i) = RTP timestamp of packet i
```

**Implementation**:
```go
func (m *RTPMetrics) RecordPacketReceived(rtpTimestamp uint32, seqNum uint16, size int) {
    arrivalTime := time.Now()
    
    if m.lastTimestamp != 0 {
        // Convert arrival time to milliseconds
        arrivalMs := int32(arrivalTime.UnixNano() / 1000000)
        
        // Convert RTP timestamp to milliseconds (90kHz clock for video)
        rtpMs := int32(rtpTimestamp / 90)
        
        // Calculate transit time
        transit := arrivalMs - rtpMs
        
        // Calculate difference from last transit time
        d := transit - m.transit
        if d < 0 {
            d = -d  // Absolute value
        }
        
        // Exponential moving average with α = 1/16
        m.jitter = m.jitter + (float64(d)-m.jitter)/16.0
        
        m.transit = transit
    } else {
        // First packet - initialize transit
        m.transit = int32(arrivalTime.UnixNano()/1000000) - int32(rtpTimestamp/90)
    }
    
    m.lastTimestamp = rtpTimestamp
    m.lastArrivalTime = arrivalTime
}
```

**Why α = 1/16?**
- RFC 3550 specifies this smoothing factor
- Balances responsiveness vs stability
- Gives ~94% weight to history, 6% to new sample
- Smooths out short-term variations

### Box-Muller Transform for Normal Distribution

**Problem**: Need normally distributed random numbers for realistic jitter simulation.

**Formula**:
```
Given two independent uniform random variables U₁, U₂ ~ Uniform(0,1):

Z₀ = √(-2 ln(U₁)) × cos(2πU₂)
Z₁ = √(-2 ln(U₁)) × sin(2πU₂)

Where Z₀, Z₁ ~ Normal(0,1)
```

**Implementation**:
```go
func (p *VirtualParticipant) applyJitterDelay() {
    // Get uniform random numbers [0,1)
    u1 := rand.Float64()
    u2 := rand.Float64()
    
    // Box-Muller transform
    z := math.Sqrt(-2*math.Log(u1)) * math.Cos(2*math.Pi*u2)
    
    // Scale to desired mean and standard deviation
    jitterMs := baseLatency + int(z * jitterStdDev)
    
    // Clamp to reasonable range
    jitterMs = max(0, min(jitterMs, 2000))
    
    time.Sleep(time.Duration(jitterMs) * time.Millisecond)
}
```

**Why Box-Muller?**
- Exact transformation (not approximation)
- Computationally efficient
- Produces true normal distribution
- Network jitter follows Gaussian distribution in reality

**Visual Representation**:
```
Uniform Distribution (input):
│
│ ████████████████████████████
│ ████████████████████████████
└─────────────────────────────► value
  0                          1

Normal Distribution (output):
│        ████
│      ████████
│    ████████████
│  ████████████████
│████████████████████
└─────────────────────────────► value
  μ-3σ    μ    μ+3σ
```

### ITU-T G.107 E-Model (MOS Calculation)

**Purpose**: Calculate Mean Opinion Score (1.0-4.5) from network metrics.

**Formula**:
```
R = R₀ - Id - Ie

Where:
R₀ = 93.2 (base quality)
Id = delay impairment
Ie = equipment impairment (packet loss + jitter)

MOS = 1 + 0.035R + R(R-60)(100-R) × 7×10⁻⁶
```

**Delay Impairment (Id)**:
```go
delay := rttMs / 2  // One-way delay

if delay <= 177.3 {
    id = 0.024 * delay
} else {
    // Non-linear penalty for high delay
    id = 0.024*delay + 0.11*(delay-177.3)*(1.0-math.Exp(-(delay-177.3)/100.0))
}
```

**Equipment Impairment (Ie)**:
```go
// Packet loss component (logarithmic)
ie := 0.0
if packetLoss > 0 {
    ie = 30.0 * math.Log(1.0 + 15.0*packetLoss/100.0)
}

// Jitter component (linear above threshold)
if jitterMs > 10 {
    ie += (jitterMs - 10) * 0.5
}
```

**R-Factor to MOS Conversion**:
```go
// Clamp R-factor to valid range
r = max(0, min(r, 100))

// Polynomial conversion
mos = 1.0 + 0.035*r + r*(r-60)*(100-r)*7e-6

// Clamp MOS to valid range
mos = max(1.0, min(mos, 4.5))
```

**MOS Scale**:
```
4.5 - 4.0: Excellent
4.0 - 3.5: Good
3.5 - 3.0: Fair
3.0 - 2.5: Poor
2.5 - 1.0: Bad
```

**Why This Model?**
- ITU-T standard for VoIP quality
- Accounts for human perception
- Non-linear (humans more sensitive to bad quality)
- Validated against subjective tests

### Packet Loss from Sequence Numbers

**Problem**: Detect packet loss without explicit loss reports.

**Formula**:
```
Expected packets = (max_seq - min_seq + 1) + (wraps × 65536)
Lost packets = Expected - Received
Loss % = (Lost / Expected) × 100
```

**Implementation**:
```go
func (m *RTPMetrics) GetPacketLossFromSequence() float64 {
    m.mu.RLock()
    defer m.mu.RUnlock()
    
    if len(m.receivedSeqNums) == 0 {
        return 0
    }
    
    // Find min and max sequence numbers
    var minSeq, maxSeq uint16
    first := true
    for seq := range m.receivedSeqNums {
        if first {
            minSeq, maxSeq = seq, seq
            first = false
        } else {
            minSeq = min(minSeq, seq)
            maxSeq = max(maxSeq, seq)
        }
    }
    
    // Calculate expected packets (accounting for wraps)
    expected := int(maxSeq) - int(minSeq) + 1 + m.seqWrap*65536
    received := len(m.receivedSeqNums)
    
    if expected <= 0 {
        return 0
    }
    
    return float64(expected-received) / float64(expected) * 100.0
}
```

**Sequence Number Wrap Handling**:
```
Sequence numbers are 16-bit: 0-65535

Example wrap:
65534, 65535, 0, 1, 2  ← wrap occurred

Detection:
if currentSeq < 1000 && lastSeq > 65000 {
    seqWrap++  // Increment wrap counter
}
```

### Percentile Calculation (P99 Jitter)

**Purpose**: Find 99th percentile of jitter samples (worst 1% threshold).

**Algorithm**:
```go
func (m *RTPMetrics) GetJitterP99() float64 {
    m.mu.RLock()
    defer m.mu.RUnlock()
    
    if len(m.jitterSamples) == 0 {
        return 0
    }
    
    // Copy and sort samples
    sorted := make([]float64, len(m.jitterSamples))
    copy(sorted, m.jitterSamples)
    sort.Float64s(sorted)
    
    // Calculate 99th percentile index
    idx := int(float64(len(sorted)) * 0.99)
    if idx >= len(sorted) {
        idx = len(sorted) - 1
    }
    
    return sorted[idx]
}
```

**Why P99?**
- Shows worst-case performance (excluding outliers)
- More meaningful than max (which can be a single spike)
- Industry standard for latency metrics
- Represents user experience for 99% of packets

### Bitrate Calculation (Sliding Window)

**Formula**:
```
Bitrate (kbps) = (Total bytes in window × 8) / Window duration (seconds) / 1000
```

**Implementation**:
```go
func (m *RTPMetrics) GetBitrateKbps() int {
    m.mu.Lock()
    defer m.mu.Unlock()
    
    now := time.Now()
    cutoff := now.Add(-m.bitrateWindow)  // 1 second window
    
    // Filter samples within window
    validSamples := make([]bitrateSample, 0, len(m.bitrateSamples))
    for _, s := range m.bitrateSamples {
        if s.timestamp.After(cutoff) {
            validSamples = append(validSamples, s)
        }
    }
    m.bitrateSamples = validSamples
    
    // Sum bytes in window
    var totalBytes int64
    for _, s := range validSamples {
        totalBytes += s.bytes
    }
    
    // Convert to kbps
    return int(totalBytes * 8 / 1000)
}
```

**Why Sliding Window?**
- Smooths out burst traffic
- Reflects current throughput
- Matches how network equipment measures bandwidth
- 1-second window balances responsiveness vs stability

### Exponential Moving Average (EMA)

**General Formula**:
```
EMA(t) = α × Value(t) + (1-α) × EMA(t-1)

Where:
α = smoothing factor (0 < α < 1)
Higher α = more weight to recent values
Lower α = more weight to history
```

**Used in**:
- Jitter calculation (α = 1/16 = 0.0625)
- RTT averaging
- Bitrate smoothing

**Why EMA?**
- Simple to compute (no need to store all history)
- Gives more weight to recent data
- Smooths out noise
- Computationally efficient (O(1) per update)

---

## 18. Jitter Implementation Deep Dive

### The Challenge

**Problem**: Jitter metrics showed 0 in Grafana despite network_jitter spikes being configured.

**Root Cause**: Jitter is calculated on the receiver side (RFC 3550), but orchestrator only sends packets. Additionally, OS-level jitter (tc/netem) failed due to missing NET_ADMIN capability in Kubernetes pods.

### Why OS-Level Failed

```bash
# Kubernetes pods lack NET_ADMIN capability by default
[⚠] Failed to apply real jitter: failed to add root qdisc: exit status 2
RTNETLINK answers: Operation not permitted
```

Attempting to use `tc netem` or `dnctl` requires privileged containers, which is:
- A security risk
- Not portable across environments
- Blocked by default in Kubernetes

### Application-Level Solution

Implemented jitter at the application level by adding delays before packet transmission:

```go
func (p *VirtualParticipant) applyJitterDelay() {
    for _, spike := range p.activeSpikes {
        if spike.Type == "network_jitter" {
            baseLatency := spike.Params["base_latency_ms"]
            jitterStdDev := spike.Params["jitter_std_dev_ms"]
            
            // Box-Muller transform for normal distribution
            u1, u2 := rand.Float64(), rand.Float64()
            z := sqrt(-2*ln(u1)) * cos(2*π*u2)
            delay := baseLatency + int(z * jitterStdDev)
            
            // Clamp to 0-2000ms
            delay = max(0, min(delay, 2000))
            
            // Record for metrics and apply delay
            p.metrics.RecordSimulatedJitter(float64(delay))
            time.Sleep(time.Duration(delay) * time.Millisecond)
        }
    }
}
```

### Why Box-Muller Transform?

Network jitter follows a normal (Gaussian) distribution in real networks. Box-Muller generates normally distributed random numbers from uniform random numbers, providing realistic jitter patterns.

### Metrics Simulation

Since orchestrator only sends (never receives), we simulate receiver-side jitter calculation:

```go
func (m *RTPMetrics) RecordSimulatedJitter(jitterMs float64) {
    m.mu.Lock()
    defer m.mu.Unlock()
    
    // RFC 3550 jitter formula: J(i) = J(i-1) + (|D(i-1,i)| - J(i-1))/16
    m.jitter = m.jitter + (jitterMs-m.jitter)/16.0
    m.jitterSamples = append(m.jitterSamples, m.jitter)
    
    // Keep last 1000 samples for P99 calculation
    if len(m.jitterSamples) > 1000 {
        m.jitterSamples = m.jitterSamples[len(m.jitterSamples)-500:]
    }
}
```

### True Jitter Measurement (Receiver Side)

The UDP receiver can calculate actual jitter using RFC 3550:

```go
// In examples/go/udp_receiver.go
func (m *RTPMetrics) RecordPacketReceived(rtpTimestamp uint32, seqNum uint16, size int) {
    arrivalTime := time.Now()
    
    if m.lastTimestamp != 0 {
        arrivalMs := int32(arrivalTime.UnixNano() / 1000000)
        rtpMs := int32(rtpTimestamp / 90)  // 90kHz clock for video
        transit := arrivalMs - rtpMs
        d := transit - m.transit
        if d < 0 {
            d = -d
        }
        // RFC 3550 formula
        m.jitter = m.jitter + (float64(d)-m.jitter)/16.0
        m.transit = transit
    } else {
        m.transit = int32(arrivalTime.UnixNano()/1000000) - int32(rtpTimestamp/90)
    }
    
    m.lastTimestamp = rtpTimestamp
    m.lastArrivalTime = arrivalTime
}
```

### Architecture: Sender vs Receiver Metrics

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                    JITTER MEASUREMENT ARCHITECTURE                           │
├─────────────────────────────────────────────────────────────────────────────┤
│                                                                              │
│  Orchestrator (Sender)                    UDP Receiver                      │
│         │                                        │                          │
│         │  1. Apply jitter delay                 │                          │
│         │     (50-150ms + 100-300ms variance)    │                          │
│         │                                        │                          │
│         │  2. Record simulated jitter            │                          │
│         │     (for Grafana dashboard)            │                          │
│         │                                        │                          │
│         │  3. Send RTP packet ──────────────────►│                          │
│         │                                        │                          │
│         │                                        │  4. Record arrival time  │
│         │                                        │                          │
│         │                                        │  5. Calculate true jitter│
│         │                                        │     (RFC 3550 formula)   │
│         │                                        │                          │
│         │  ❌ No RTCP feedback                   │  ✅ True jitter value    │
│         │     (future enhancement)               │                          │
│                                                                              │
└─────────────────────────────────────────────────────────────────────────────┘
```

### Verification

**Check orchestrator logs**:
```bash
kubectl logs orchestrator-0 | grep -i "jitter\|spike"
```

Look for:
```
[CHAOS] Injected spike chaos_spike_24 type=network_jitter to 150 participants
```

**Run UDP receiver**:
```bash
go run ./examples/go/udp_receiver.go 5002
```

Output:
```
[Jitter] Current: 145.23ms | P99: 287.12ms
```

### Future Enhancement: RTCP Feedback

For true end-to-end jitter measurement in Grafana, implement RTCP Receiver Reports:

1. UDP receiver calculates jitter
2. Sends RTCP RR packets back to orchestrator
3. Orchestrator updates metrics from RTCP feedback
4. Grafana shows actual measured jitter

This is how production WebRTC systems work, but requires significant implementation effort.

---

## 20. Evolution Timeline

| Phase | What Changed |
|-------|--------------|
| Initial | Shell scripts, single pod, synthetic frames |
| v1 | Go rewrite, Prometheus metrics, Docker support |
| v2 | Kubernetes StatefulSet, partitioning, real media |
| v3 | UDP relay chain, WebRTC multi-receiver, Nix builds |
| v4 | Application-level chaos, jitter simulation, spike distribution strategies |
| Current | 1500 participants, 10 pods, full chaos injection with metrics |

---

## 21. Final Architecture

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                         KUBERNETES CLUSTER                                   │
├─────────────────────────────────────────────────────────────────────────────┤
│                                                                              │
│  ┌───────────────────────────────────────────────────────────────────────┐  │
│  │              Orchestrator StatefulSet (10 pods)                       │  │
│  │                                                                       │  │
│  │  ┌─────────────────────────────────────────────────────────────────┐ │  │
│  │  │                    Per-Pod Processing                           │ │  │
│  │  │                                                                 │ │  │
│  │  │  MediaSource ──► H264Reader ──► H264Packetizer ──► RTP Packets │ │  │
│  │  │       │                                                │        │ │  │
│  │  │       └──────► OggReader ───► OpusPacketizer ──────────┘        │ │  │
│  │  │                                                                 │ │  │
│  │  │  150 participants × (video + audio) = 300 streams per pod      │ │  │
│  │  └─────────────────────────────────────────────────────────────────┘ │  │
│  │                                                                       │  │
│  │  Chaos Injection: frame_drop, rtp_packet_loss, audio_silence         │  │
│  │  Prometheus: /metrics endpoint on :8080                              │  │
│  └───────────────────────────────────────────────────────────────────────┘  │
│                              │                                               │
│              ┌───────────────┴───────────────┐                              │
│              ▼                               ▼                              │
│  ┌─────────────────────┐        ┌─────────────────────┐                    │
│  │    UDP Relay Pod    │        │   Coturn TURN Pod   │                    │
│  │  UDP:5000 → TCP:5001│        │   STUN/TURN:3478    │                    │
│  │  (length-prefixed)  │        │   (NAT traversal)   │                    │
│  └─────────────────────┘        └─────────────────────┘                    │
│                                                                              │
└─────────────────────────────────────────────────────────────────────────────┘
                │                               │
                │ kubectl port-forward          │ WebRTC via TURN
                ▼                               ▼
┌─────────────────────────────────────────────────────────────────────────────┐
│                           LOCAL MACHINE                                      │
├─────────────────────────────────────────────────────────────────────────────┤
│                                                                              │
│  ┌─────────────────────────────┐    ┌─────────────────────────────┐        │
│  │      UDP Receiver           │    │     WebRTC Receiver         │        │
│  │  localhost:5002             │    │  Per-participant SRTP       │        │
│  │  All 1500 participants      │    │  connections via TURN       │        │
│  │  on single socket           │    │                             │        │
│  └─────────────────────────────┘    └─────────────────────────────┘        │
│                                                                              │
└─────────────────────────────────────────────────────────────────────────────┘
```

---

## 22. Client Package Architecture

**Package:** `pkg/client` - Provides WebRTC client infrastructure for connecting to participants at scale.

**Structure:**
```
pkg/client/
├── client.go          # Main entry point, Config, ConnectionManager, KubernetesManager
├── discovery.go       # Participant discovery logic
├── stats.go           # Connection statistics collection
├── k8s/
│   └── manager.go     # Kubernetes pod discovery, port-forwarding, auto-scaling
├── webrtc/
│   └── connection.go  # WebRTC connection management, SDP handling, track handlers
├── rtp/               # RTP packet parsing utilities
└── udp/               # UDP receiver for RTP packets
```

**Key Configuration Defaults:**
- HTTPTimeout: 120s (overall timeout)
- MaxIdleConns: 200
- ICEDisconnectedTimeout: 30s
- ICEFailedTimeout: 60s
- ConcurrencyLimit: 50 (per-pod concurrency)
- MaxRetries: 0 (speed priority - fail fast)
- PortForwardBasePort: 18080

**HTTP Client (optimized for speed):**
- Timeout: 3 seconds (fail fast)
- MaxIdleConns: 500
- MaxConnsPerHost: 50
- ResponseHeaderTimeout: 2 seconds

**Connection Flow:**
1. Kubernetes Mode: Auto-scale orchestrator pods, set up concurrent port-forwards, distribute connections across pods (~150 per pod)
2. Local Mode: Connect directly to single endpoint with semaphore for concurrency control (50 concurrent)
3. Batch Connection: Wave 1 connects all participants with concurrency limit, Wave 2 retries failed participants (1s delay)

**Design Decisions:**
- Speed over resilience - Short timeouts, minimal retries
- No kubectl exec fallback - Too slow at scale (API server gets overwhelmed)
- Concurrent port-forwards - All pods set up simultaneously to minimize startup time
- Partition-aware discovery - Each pod only handles participants in its partition (participant_id % total_partitions)
- Auto-scaling - Orchestrators scale to optimal count based on participant count (150 per pod, max 10 pods)

---

## 23. Spike Distribution Strategies

**Problem:** Need flexible control over when chaos spikes are injected during tests.

**Available Strategies:**

| Strategy | Timing | Use Case |
|----------|--------|----------|
| `legacy` | Fixed interval ticker | Real-time chaos simulation (original behavior) |
| `even` | Pre-calculated, uniform | Consistent load testing |
| `random` | Pre-calculated, random | Realistic chaos scenarios |
| `front_loaded` | First 40% of test | Recovery testing |
| `back_loaded` | Last 40% of test | Baseline + chaos comparison |

**Configuration:**
```json
{
  "spike_distribution": {
    "strategy": "even|random|front_loaded|back_loaded|legacy",
    "min_spacing_seconds": 5,
    "jitter_percent": 15,
    "respect_min_offset": true
  }
}
```

**Visual Timeline:**
```
Test Duration: 0% -------- 50% -------- 100%

legacy:       |*  *  *  *  *  *  *  *  *  *|  (fixed intervals)
even:         |*  *  *  *  *  *  *  *  *  *|  (uniform distribution)
random:       |** *    * **  *   * *  *  * |  (random with clusters/gaps)
front_loaded: |*****  ***  **             |  (dense start, quiet end)
back_loaded:  |              ** *** *****|  (quiet start, dense end)
```

**Why Multiple Strategies:**
- `legacy`: Maintains backward compatibility with original runtime injection
- `even`: Predictable spike distribution for reproducible tests
- `random`: Simulates real-world unpredictability
- `front_loaded`: Tests system recovery after intense chaos
- `back_loaded`: Establishes baseline before introducing chaos

---

## 24. Receiver Refactoring Lessons

**Problem:** Attempted to consolidate duplicate metrics code from UDP and WebRTC receivers into shared package.

**What Happened:**
1. Created shared `pkg/client/metrics.go` with RFC 3550 compliant jitter calculation
2. Added per-SSRC packet loss tracking based on sequence number gaps
3. UDP receiver showed 85-90% packet loss with 1500 participants (3000 streams)

**Root Cause:** The packet loss was REAL, not a measurement bug. Single UDP socket cannot handle 3000+ concurrent streams without kernel buffer overflow. The original code didn't track per-stream metrics, so this loss was invisible.

**Decision:** Reverted to original implementations. Keep example receivers simple and self-contained.

**Lessons Learned:**
1. Measuring changes behavior - Adding metrics can reveal problems that were always there
2. UDP at scale - Single socket cannot reliably receive thousands of concurrent streams
3. Keep examples simple - Self-contained code is better than DRY for examples
4. Comments matter - Removing "obvious" comments during refactoring makes debugging harder

**Files Affected:**
- `examples/go/udp_receiver.go` - Reverted to original
- `examples/go/webrtc_receiver.go` - Reverted to original
- `pkg/client/metrics.go` - Deleted (unused)

---

## 25. WebRTC in Kubernetes Limitations and Solutions

**Problem:** WebRTC connections from outside cluster to pods fail with "connection timeout".

**Root Cause:**
- Pod IPs (10.244.x.x) are not routable from outside cluster
- TURN servers inside cluster not accessible from local machine
- ICE negotiation fails due to NAT traversal issues

**Solutions:**

### Option 1: UDP Receiver (Recommended for Monitoring)
Use UDP receiver for Kubernetes testing - works reliably via relay chain.

### Option 2: WebRTC Multi-Receiver (For Detailed Analysis)
The WebRTC multi-receiver tool now supports Kubernetes by:
1. Auto-discovering all orchestrator pods
2. Setting up kubectl port-forward for each pod (localhost:18080-18089)
3. Connecting to participants across all pods via localhost
4. Aggregating statistics

**Usage:**
```bash
# Connect to all 1500 participants
./start-webrtc-receiver.sh chaos_test_123 all

# Or manually
go run ./examples/go/webrtc_multi_receiver.go http://localhost:8080 chaos_test_123 all
```

**How it works:**
```
Your Machine
    ↓
kubectl port-forward orchestrator-0 → localhost:18080 ─┐
kubectl port-forward orchestrator-1 → localhost:18081 ─┤
kubectl port-forward orchestrator-2 → localhost:18082 ─┼→ WebRTC Connections
...                                                     ─┤
kubectl port-forward orchestrator-9 → localhost:18089 ─┘
```

**Performance:**
- Connection time: 1-2 minutes for 1500 participants
- Memory: ~3-4 GB for 1500 connections
- CPU: Moderate during connection, low during streaming

**Comparison:**

| Scenario | UDP Receiver | WebRTC Multi-Receiver |
|----------|-------------|----------------------|
| Local (no K8s) | ✅ Works | ✅ Works |
| Kubernetes | ✅ Works (via relay) | ✅ Works (via port-forwards) |
| All Participants | ✅ Automatic aggregation | ✅ Connects to all via port-forwards |
| Resource Usage | Low (~100 MB) | High (~3-4 GB for 1500) |
| Setup Complexity | Simple (3 terminals) | Automated (script handles it) |
| Connection Time | Instant | 1-2 minutes |
| Per-Participant Stats | ✅ Yes | ✅ Yes with pod breakdown |
| Use Case | Quick monitoring | Detailed analysis |

**Recommendation:** 
- For quick monitoring and high-scale testing: Use UDP receiver
- For detailed per-participant analysis: Use WebRTC multi-receiver
- For local development/debugging: Use single WebRTC receiver

**Key Fix:** The WebRTC multi-receiver was updated to register default codecs using `MediaEngine.RegisterDefaultCodecs()`, which fixed the "SetRemoteDescription called with no ice-ufrag" error.

---

## 26. WebRTC Client Fix - ICE Credentials Issue

**Problem:** WebRTC multi-receiver failing with "answer request failed: 500 - Failed to set SDP answer: SetRemoteDescription called with no ice-ufrag".

**Root Cause:** Client-side peer connection was not registering any media codecs, causing `CreateAnswer()` to generate invalid SDP answer without ICE credentials.

**Technical Details:**
1. Server's SDP offer contained proper ICE credentials and codec information (H264, VP8, Opus)
2. Client's peer connection created without MediaEngine configuration
3. Without registered codecs, client couldn't negotiate media and generated malformed answer
4. Answer had `m=video 0` and `m=audio 0` (rejected media) and no ICE credentials
5. Server rejected answer with "SetRemoteDescription called with no ice-ufrag" error

**Solution:** Register default codecs on client-side peer connection:

```go
// Create media engine and register codecs
mediaEngine := &webrtc.MediaEngine{}
if err := mediaEngine.RegisterDefaultCodecs(); err != nil {
    return nil, fmt.Errorf("failed to register codecs: %w", err)
}

// Create settings engine with relaxed timeouts
settingEngine := webrtc.SettingEngine{}
settingEngine.SetICETimeouts(10*time.Second, 30*time.Second, 5*time.Second)

// Create API with both media engine and settings engine
api := webrtc.NewAPI(
    webrtc.WithMediaEngine(mediaEngine),
    webrtc.WithSettingEngine(settingEngine),
)

pc, err := api.NewPeerConnection(config)
```

**Result:** Answer SDP now contains proper ICE credentials and codec negotiation, allowing successful WebRTC connections.

**Lessons Learned:**
1. Always register codecs when creating WebRTC peer connections
2. Without codec registration, SDP negotiation fails silently with malformed answers
3. Error "SetRemoteDescription called with no ice-ufrag" indicates missing ICE credentials in SDP
4. Both MediaEngine (for codecs) and SettingEngine (for ICE timeouts) should be configured

---

## 27. Partition-Aware Participant Discovery

**Problem:** WebRTC receiver failing with "answer request failed: 500" when trying to connect to participants.

**Root Cause:** Participants are distributed across pods using modulo allocation:
- Partition 0 (orchestrator-0): Participants where `ID % 10 == 0` → 1010, 1020, 1030...
- Partition 1 (orchestrator-1): Participants where `ID % 10 == 1` → 1001, 1011, 1021...
- Partition 2 (orchestrator-2): Participants where `ID % 10 == 2` → 1002, 1012, 1022...

The old code tried sequential IDs (1001, 1002, 1003...) on each pod, which meant:
- Pod 0 was asked for 1001, 1002, 1003... but only has 1010, 1020, 1030...
- Pod 1 was asked for 1001, 1002, 1003... but only has 1001, 1011, 1021...

**Solution:** Updated `findParticipantsInPod()` to:
1. Get `total_partitions` from pod's `/healthz` endpoint
2. Calculate which participant IDs belong to this partition using modulo
3. Only try to connect to participants that actually exist in this pod

**Example Allocation (1500 participants, 10 pods):**
```
Pod 0 (partition 0): 1010, 1020, 1030, ..., 2500  (150 participants)
Pod 1 (partition 1): 1001, 1011, 1021, ..., 2491  (150 participants)
Pod 2 (partition 2): 1002, 1012, 1022, ..., 2492  (150 participants)
...
Pod 9 (partition 9): 1009, 1019, 1029, ..., 2499  (150 participants)
```

**Result:** No more failed connections, faster discovery, correct distribution.

---

## 28. Nix Build System

**Why Nix:**
- Reproducible builds across all platforms
- Cross-compilation to any architecture without Docker
- Hermetic build environment (no system dependencies)
- Declarative configuration in `flake.nix`

**Supported Architectures:**
- x86_64-linux (Standard Linux Intel/AMD 64-bit)
- aarch64-linux (ARM64 Linux - Raspberry Pi, AWS Graviton)
- x86_64-darwin (macOS Intel)
- aarch64-darwin (macOS Apple Silicon M1/M2/M3)

**Build Commands:**
```bash
# Build for current system
nix build

# Build for specific architecture
nix build .#packages.x86_64-linux.av-chaos-monkey
nix build .#packages.aarch64-linux.av-chaos-monkey
nix build .#packages.x86_64-darwin.av-chaos-monkey
nix build .#packages.aarch64-darwin.av-chaos-monkey
```

**Development Environment:**
```bash
# Enter dev shell (includes Go, Docker, kubectl, protobuf)
nix develop

# Or use direnv for automatic environment loading
direnv allow
```

**Docker Integration:**
```bash
# Build with Nix
nix build .#packages.x86_64-linux.av-chaos-monkey

# Use in Dockerfile
FROM alpine:latest
COPY result/bin/main /usr/local/bin/av-chaos-monkey
CMD ["av-chaos-monkey", "-http", ":8080"]
```

**Why Not Traditional Go Build:**
- Nix ensures exact same build on any machine
- No "works on my machine" issues
- Cross-compilation without Docker or QEMU
- Reproducible down to compiler version

---

## 29. Pod Startup Fix (hostNetwork Removal)

**Problem:** With `hostNetwork: true`, all pods tried to bind to port 8080 on host, causing port conflicts. Only 1 out of 10 pods could start.

**Solution:** Removed `hostNetwork: true` from `k8s/orchestrator/orchestrator.yaml`. Now all pods use their own network namespace and can all bind to port 8080 within their pods.

**Impact on WebRTC:**
- Without `hostNetwork: true`, WebRTC has NAT traversal challenges
- Pods generate ICE candidates with pod IPs (10.244.0.x)
- Client cannot reach pod IPs directly
- TURN servers configured to handle relay

**Verification:**
```bash
# All 10 pods should be running
kubectl get pods -l app=orchestrator
```

---

## 30. UDP Receiver Deployment Architecture

**Problem:** UDP packets from Kubernetes pods cannot reach local machine directly due to Docker/Kind network isolation.

**Solution:** Three-stage relay chain:

```
┌─────────────────────────────────────────────────────────────────┐
│ Kubernetes Cluster                                              │
│                                                                 │
│  Orchestrator Pods (1500 participants)                         │
│         │                                                       │
│         ▼                                                       │
│  udp-relay:5000 (UDP) → TCP:5001                              │
│  - Aggregates UDP from all pods                                │
│  - Converts to TCP with length-prefix framing                  │
└─────────────────────────┼───────────────────────────────────────┘
                          │
                          │ kubectl port-forward 15001:5001
                          ▼
                 ┌────────────────┐
                 │ localhost:15001│ (TCP)
                 └────────┬───────┘
                          │
                          │ tools/udp-relay/main.go
                          │ (TCP→UDP converter)
                          ▼
                 ┌────────────────┐
                 │ localhost:5002 │ (UDP)
                 └────────┬───────┘
                          │
                          ▼
                    UDP Receiver
```

**Why this architecture:**
1. UDP cannot cross Docker/Kind boundaries reliably
2. TCP has connection tracking, works with kubectl port-forward
3. Length-prefix framing preserves packet boundaries over TCP stream
4. Single relay aggregates packets from all 1500 participants

**Deployment:**
```bash
# Terminal 1: Port-forward
kubectl port-forward udp-relay 15001:5001

# Terminal 2: Local relay
go run ./tools/udp-relay/main.go

# Terminal 3: Receiver
go run ./examples/go/udp_receiver.go 5002
```

**Alternative:** In-cluster UDP receiver pod with `hostNetwork: true` (eliminates need for relay chain).

---

## 32. Example Implementations and Language Support

**Problem:** Need examples in multiple languages to demonstrate integration, but WebRTC libraries vary in maturity.

**Languages Implemented:**
- Go (fully working)
- JavaScript/Node.js (UDP only)
- Python (UDP only)
- Rust (UDP only)

**Status Summary:**

| Language | UDP Receiver | WebRTC Receiver | Recommended Use |
|----------|-------------|-----------------|-----------------|
| **Go** | ✅ Works | ✅ Works | Production, testing, development |
| JavaScript | ✅ Works | ❌ ICE issues | UDP integration only |
| Python | ✅ Works | ⚠️ Partial | UDP integration only |
| Rust | ✅ Works | ⚠️ Partial | UDP integration only |

### Why Go is the Only Fully Working Implementation

**Go (Pion WebRTC):**
- Mature, production-grade WebRTC library
- Full ICE/DTLS/SRTP support with proper trickle ICE
- Native integration with orchestrator (same library)
- Handles 500+ concurrent connections efficiently
- Proper TURN relay support for NAT traversal
- Active development and community support

**JavaScript/Node.js Issues:**
- `node-datachannel` and `werift` have incomplete ICE implementations
- ICE candidate gathering often fails or times out
- TURN relay candidates not properly negotiated
- No reliable trickle ICE support in Node.js WebRTC libraries
- Error: "ICE connection state: failed"

**Python (aiortc) Issues:**
- Limited ICE candidate handling
- Incomplete TURN relay support
- ICE gathering timeout issues in containerized environments
- SDP parsing differences cause compatibility issues
- Error: "ICE Connection State: failed"

**Rust (webrtc-rs) Issues:**
- Library still maturing
- ICE candidate exchange timing issues
- DTLS handshake failures in some network configurations
- Async runtime conflicts with ICE gathering
- Error: "Peer Connection State: failed"

### Root Causes of Non-Go Failures

**1. ICE Connectivity:**
WebRTC requires ICE (Interactive Connectivity Establishment) to negotiate network paths. Non-Go libraries often have incomplete ICE implementations that fail in:
- NAT traversal scenarios
- Containerized environments (Docker, Kubernetes)
- Networks with restrictive firewalls

**2. TURN Relay Support:**
When direct connectivity fails, WebRTC falls back to TURN relay servers. Many libraries:
- Don't properly request relay candidates
- Fail to authenticate with TURN servers
- Don't handle TURN over TCP fallback

**3. Trickle ICE:**
Modern WebRTC uses "trickle ICE" where candidates are sent incrementally. Libraries that don't support this properly will:
- Wait too long for all candidates
- Miss candidates that arrive after SDP exchange
- Fail to establish connections in time

**4. SDP Compatibility:**
Session Description Protocol (SDP) parsing varies between implementations:
- Different handling of media sections
- Incompatible codec negotiation
- Missing or malformed attributes

### UDP Receivers Work in All Languages

All language implementations have working UDP receivers because:
- UDP is a simple, well-supported protocol
- No complex negotiation required
- Standard socket APIs work consistently
- No dependency on WebRTC libraries

**Usage:**
```bash
# Go
go run ./examples/go/udp_receiver.go 5002

# JavaScript
node ./examples/javascript/udp_receiver.js 5002

# Python
python3 ./examples/python/udp_receiver.py 5002

# Rust
cargo run --bin udp_receiver 5002
```

### Integration Recommendations

**For Development/Testing:**
Use the Go WebRTC receiver - it's reliable and handles all edge cases:
```bash
go run ./examples/go/webrtc_receiver.go http://localhost:8080 <test_id> <count>
```

**For Non-Go Integration:**
1. **Use UDP forwarding**: Configure orchestrator to forward packets via UDP, receive with language's UDP receiver
2. **Use Go proxy**: Run Go WebRTC receiver and forward packets to your application
3. **Wait for library improvements**: WebRTC libraries in other languages are actively developed

**For Production:**
- Use Go implementation directly
- Or deploy Go receiver as sidecar/proxy service
- UDP receivers work reliably in all languages if WebRTC is not required

### Example Directory Structure

```
examples/
├── go/
│   ├── udp_receiver.go          ✅ Working
│   ├── webrtc_receiver.go       ✅ Working
│   └── webrtc_multi_receiver.go ✅ Working
├── javascript/
│   ├── udp_receiver.js          ✅ Working
│   └── webrtc_receiver.js       ❌ ICE issues
├── python/
│   ├── udp_receiver.py          ✅ Working
│   └── webrtc_receiver.py       ⚠️ Partial
└── rust/
    ├── udp_receiver.rs          ✅ Working
    └── webrtc_receiver.rs       ⚠️ Partial
```

### Quick Start Guide

The `examples/QUICK_START_GUIDE.md` provides step-by-step instructions for:
1. Starting AV Chaos Monkey
2. Creating and starting tests
3. Running receivers in each language
4. Understanding output
5. Troubleshooting common issues

### UDP-to-TCP Relay Chain for Kubernetes

For Kubernetes deployments, UDP packets use a 3-stage relay chain:

```
Orchestrator pods → UDP → udp-relay pod → TCP → kubectl port-forward → Local relay → UDP receiver
```

This architecture is necessary because:
- UDP cannot cross Docker/Kind boundaries reliably
- TCP has connection tracking, works with kubectl port-forward
- Length-prefix framing preserves packet boundaries over TCP stream
- Single relay aggregates packets from all 1500 participants

See `examples/UDP_WEBRTC_INTEGRATION_JOURNEY.md` for complete implementation details.

### Future Improvements

Potential enhancements for non-Go languages:
- WebSocket-based packet streaming (bypasses WebRTC entirely)
- gRPC streaming for packet delivery
- Improved SDP compatibility for non-Go clients
- Wait for WebRTC library maturity in other languages

**Lesson:** When building multi-language examples, choose libraries carefully. Go's Pion WebRTC is production-ready, while other languages' WebRTC libraries are still maturing. UDP provides a reliable fallback for all languages.

---

## 33. Files Reference

### Core Implementation

| Purpose | File |
|---------|------|
| Entry point | `cmd/main.go` |
| HTTP server | `internal/server/http.go` |
| H.264 reader | `internal/media/h264_reader.go` |
| Opus reader | `internal/media/opus_reader.go` |
| Media source | `internal/media/source.go` |
| H.264 packetizer | `internal/rtp/packetizer.go` |
| Opus packetizer | `internal/rtp/opus_packetizer.go` |
| RTP parser (shared) | `internal/rtp/helpers.go` |
| WebRTC participant | `internal/webrtc/participant.go` |
| WebRTC streaming | `internal/webrtc/streaming.go` |
| UDP streaming | `internal/pool/streaming.go` |
| Spike injection | `internal/spike/injector.go` |
| Spike generation | `internal/spike/generator.go` |
| Spike scheduling | `pkg/scheduler/spike_scheduler.go` |
| RTP metrics | `pkg/metrics/stats.go` |
| Client package | `pkg/client/client.go` |
| K8s manager | `pkg/client/k8s/manager.go` |
| WebRTC connection | `pkg/client/webrtc/connection.go` |

### Example Implementations

**Legend:** ✅ Fully working | ⚠️ Partially working (UDP only) | ❌ Not working

| Language | File | Status | Notes |
|----------|------|--------|-------|
| **Go** | `examples/go/udp_receiver.go` | ✅ | Production-ready UDP receiver |
| **Go** | `examples/go/webrtc_receiver.go` | ✅ | Single participant WebRTC |
| **Go** | `examples/go/webrtc_multi_receiver.go` | ✅ | Multi-participant WebRTC with K8s support |
| **JavaScript** | `examples/javascript/udp_receiver.js` | ✅ | UDP receiver works |
| **JavaScript** | `examples/javascript/webrtc_receiver.js` | ❌ | ICE connectivity issues |
| **Python** | `examples/python/udp_receiver.py` | ✅ | UDP receiver works |
| **Python** | `examples/python/webrtc_receiver.py` | ⚠️ | Partial - aiortc ICE issues |
| **Rust** | `examples/rust/udp_receiver.rs` | ✅ | UDP receiver works |
| **Rust** | `examples/rust/webrtc_receiver.rs` | ⚠️ | Partial - webrtc-rs maturity issues |

### Example Documentation

| Purpose | File |
|---------|------|
| Quick start guide | `examples/QUICK_START_GUIDE.md` |
| Main examples README | `examples/README.md` |
| WebRTC status | `examples/WEBRTC_STATUS.md` |
| WebRTC limitations | `examples/WEBRTC_LIMITATIONS.md` |
| UDP/WebRTC journey | `examples/UDP_WEBRTC_INTEGRATION_JOURNEY.md` |
| Packet forwarding | `examples/PACKET_FORWARDING.md` |
| TURN server setup | `examples/TURN_SERVER_SETUP.md` |
| Coturn setup | `examples/COTURN_SETUP.md` |
| K8s WebRTC NAT | `examples/KUBERNETES_WEBRTC_NAT.md` |
| Port conflict solutions | `examples/PORT_CONFLICT_SOLUTIONS.md` |

### Tools

| Purpose | File |
|---------|------|
| Chaos test CLI | `tools/chaos-test/main.go` |
| UDP relay tool | `tools/udp-relay/main.go` |
| K8s start tool | `tools/k8s-start/main.go` |
| Local start tool | `tools/start/main.go` |

### Kubernetes Manifests

| Purpose | File |
|---------|------|
| Orchestrator StatefulSet | `k8s/orchestrator/orchestrator.yaml` |
| UDP relay deployment | `k8s/udp-relay/udp-relay.yaml` |
| TURN server (Coturn) | `k8s/coturn/coturn.yaml` |
| Prometheus monitoring | `k8s/monitoring/prometheus.yaml` |
| Grafana dashboard | `k8s/monitoring/grafana.yaml` |
| Prometheus RBAC | `k8s/monitoring/prometheus-rbac.yaml` |
| Connector deployment | `k8s/connector/connector.yaml` |

---

*Last updated: 2026-02-22 - Comprehensive update with all architectural decisions, implementation details, and operational guides*
