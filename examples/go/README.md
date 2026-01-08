# Go Examples for AV Chaos Monkey

This directory contains Go examples for receiving RTP packets from AV Chaos Monkey via UDP and WebRTC.

## Prerequisites

- Go 1.21 or later
- AV Chaos Monkey orchestrator running on `http://localhost:8080`

## Examples

### 1. UDP Receiver (`udp_receiver.go`)

Receives RTP packets directly via UDP. This is the simplest way to receive packets.

**Usage:**
```bash
go run udp_receiver.go [port]
```

**Example:**
```bash
# Listen on default port 5000
go run udp_receiver.go

# Listen on custom port
go run udp_receiver.go 5002
```

**What it does:**
- Listens for UDP packets on the specified port
- Parses RTP headers to extract:
  - Participant ID (from RTP extension)
  - Payload type (96 = H.264 video, 111 = Opus audio)
  - Sequence number
  - Timestamp
  - SSRC
- Prints packet information every 100 packets

**Output:**
```
Listening for RTP packets on UDP port :5000
Packet #100 from 127.0.0.1:xxxxx:
  Participant ID: 1001
  Payload Type: 96 (96=H.264 video, 111=Opus audio)
  Sequence: 100
  Timestamp: 3000000
  SSRC: 1001000
  Payload Size: 1200 bytes
```

### 2. WebRTC Receiver (`webrtc_receiver.go`)

Receives RTP packets via WebRTC connection. This provides a more realistic connection with ICE, DTLS, and SRTP.

**Usage:**
```bash
go get github.com/pion/webrtc/v3
go run webrtc_receiver.go [base_url]
```

**Example:**
```bash
# Connect to local orchestrator
go run webrtc_receiver.go

# Connect to remote orchestrator
go run webrtc_receiver.go http://192.168.1.100:8080
```

**What it does:**
1. Creates a test with 1 participant
2. Gets SDP offer from orchestrator
3. Creates WebRTC peer connection
4. Exchanges SDP offer/answer
5. Receives RTP packets via WebRTC tracks
6. Prints packet information

**Output:**
```
Connecting to AV Chaos Monkey at http://localhost:8080
Created test: test_1234567890, Participant ID: 1001
Received SDP offer
Created SDP answer
Sent SDP answer, waiting for media...
Test started, receiving media...
ICE Connection State: checking
ICE Connection State: connected
Received video track, SSRC: 1001000
Received audio track, SSRC: 1001001
Received video packet: Seq=1, TS=0, Payload=1200 bytes
Received audio packet: Seq=1, TS=0, Payload=60 bytes
```

**Alternative:** Set `UDP_TARGET_HOST` to your host IP (not 127.0.0.1):
```bash
UDP_TARGET_HOST=$(ipconfig getifaddr en0) UDP_TARGET_PORT=5000 ./scripts/start_everything.sh run
```

## Integration Steps

### For UDP:

1. **Start AV Chaos Monkey:**
   ```bash
   cd /path/to/AV-Chaos-Monkey
   go run cmd/main.go
   ```

2. **Create and start a test:**
   ```bash
   curl -X POST http://localhost:8080/api/v1/test/create \
     -H "Content-Type: application/json" \
     -d '{
       "num_participants": 1,
       "backend_rtp_base_port": "5000"
     }'
   
   curl -X POST http://localhost:8080/api/v1/test/{test_id}/start
   ```

3. **Run the UDP receiver:**
   ```bash
   cd examples/go
   go run udp_receiver.go
   ```

### For WebRTC:

1. **Start AV Chaos Monkey:**
   ```bash
   cd /path/to/AV-Chaos-Monkey
   go run cmd/main.go
   ```

2. **Run the WebRTC receiver:**
   ```bash
   cd examples/go
   go get github.com/pion/webrtc/v3
   go run webrtc_receiver.go
   ```

The WebRTC receiver automatically creates the test and starts receiving.

## RTP Packet Structure

The examples parse RTP packets with the following structure:

```
RTP Header (12 bytes):
  - Version (2 bits)
  - Padding (1 bit)
  - Extension (1 bit)
  - CSRC Count (4 bits)
  - Marker (1 bit)
  - Payload Type (7 bits)
  - Sequence Number (16 bits)
  - Timestamp (32 bits)
  - SSRC (32 bits)

Extension Header (if present):
  - Extension ID (16 bits) = 1 (Participant ID)
  - Extension Length (16 bits) = 1 (4 bytes)
  - Participant ID (32 bits, little-endian)

Payload:
  - H.264 NAL units (video)
  - Opus frames (audio)
```

## Extending the Examples

### Processing Video (H.264)

To decode H.264 video, you'll need an H.264 decoder:

```go
import "github.com/pion/mediadevices/pkg/codec/openh264"

// Create decoder
decoder, err := openh264.NewDecoder()
if err != nil {
    log.Fatal(err)
}

// Decode NAL units
for _, nalu := range h264NALUs {
    frame, err := decoder.Decode(nalu)
    if err != nil {
        continue
    }
    // Process frame
}
```

### Processing Audio (Opus)

To decode Opus audio, you'll need an Opus decoder:

```go
import "github.com/pion/opus"

// Create decoder
decoder, err := opus.NewDecoder(48000, 1)
if err != nil {
    log.Fatal(err)
}

// Decode Opus frames
pcm, err := decoder.Decode(opusFrame, pcmBuffer)
if err != nil {
    continue
}
// Process PCM audio
```

## Troubleshooting

### No packets received (UDP)

1. Check if AV Chaos Monkey is running
2. Verify the test is started
3. Check firewall settings
4. Ensure port is not in use by another application

### WebRTC connection fails

1. Check STUN server accessibility
2. Verify SDP exchange completed
3. Check ICE connection state logs
4. Ensure network allows UDP traffic

### Port conflicts

If port 5000 is in use, specify a different port:
```bash
go run udp_receiver.go 5002
```

And update the test creation to use port 5002:
```json
{
  "backend_rtp_base_port": "5002"
}
```
