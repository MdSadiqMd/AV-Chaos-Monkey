package media

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
)

// OGG page header constants
const (
	oggPageHeaderSize = 27
	oggMagic          = "OggS"
)

// Opus audio packet
type OpusPacket struct {
	Data []byte
}

// Reads Opus packets from an OGG container
type OggReader struct {
	file       *os.File
	mu         sync.Mutex
	closed     bool
	pageBuffer []byte
}

func NewOggReader(path string) (*OggReader, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("failed to open OGG file: %w", err)
	}

	reader := &OggReader{
		file:       file,
		pageBuffer: make([]byte, 65536),
	}

	// Verify OGG magic
	magic := make([]byte, 4)
	if _, err := io.ReadFull(file, magic); err != nil {
		file.Close()
		return nil, fmt.Errorf("failed to read OGG magic: %w", err)
	}
	if string(magic) != oggMagic {
		file.Close()
		return nil, errors.New("not a valid OGG file")
	}

	// Reset to beginning
	if _, err := file.Seek(0, 0); err != nil {
		file.Close()
		return nil, err
	}

	return reader, nil
}

func (r *OggReader) NextPacket() (*OpusPacket, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.nextPacketLocked()
}

// Reads the next packet (must be called with mutex held)
func (r *OggReader) nextPacketLocked() (*OpusPacket, error) {
	if r.closed {
		return nil, io.EOF
	}

	// Read OGG page header
	header := make([]byte, oggPageHeaderSize)
	if _, err := io.ReadFull(r.file, header); err != nil {
		return nil, err
	}

	// Verify magic
	if string(header[0:4]) != oggMagic {
		return nil, errors.New("invalid OGG page header")
	}

	// Get number of segments
	numSegments := int(header[26])
	if numSegments == 0 {
		return nil, errors.New("empty OGG page")
	}

	// Read segment table
	segmentTable := make([]byte, numSegments)
	if _, err := io.ReadFull(r.file, segmentTable); err != nil {
		return nil, err
	}

	// Calculate total page size
	pageSize := 0
	for _, seg := range segmentTable {
		pageSize += int(seg)
	}

	// Read page data
	if pageSize > len(r.pageBuffer) {
		r.pageBuffer = make([]byte, pageSize)
	}
	pageData := r.pageBuffer[:pageSize]
	if _, err := io.ReadFull(r.file, pageData); err != nil {
		return nil, err
	}

	// Skip header packets (OpusHead and OpusTags)
	// These have granule position of 0
	granulePos := binary.LittleEndian.Uint64(header[6:14])
	if granulePos == 0 {
		// This is a header packet, skip and read next (recursive call without lock)
		return r.nextPacketLocked()
	}

	packet := &OpusPacket{
		Data: make([]byte, pageSize),
	}
	copy(packet.Data, pageData)

	return packet, nil
}

func (r *OggReader) Reset() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, err := r.file.Seek(0, 0); err != nil {
		return err
	}
	return nil
}

func (r *OggReader) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.closed = true
	if r.file != nil {
		return r.file.Close()
	}
	return nil
}

// Reades raw Opus packets (20ms frames)
type SimpleOpusReader struct {
	file   *os.File
	mu     sync.Mutex
	closed bool
}

func NewSimpleOpusReader(path string) (*SimpleOpusReader, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("failed to open Opus file: %w", err)
	}

	return &SimpleOpusReader{
		file: file,
	}, nil
}

// Reads the next Opus packet (assumes 20ms frames at 48kHz)
// For raw Opus, we read fixed-size chunks
func (r *SimpleOpusReader) NextPacket() (*OpusPacket, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.closed {
		return nil, io.EOF
	}

	// Opus 20ms frame at 128kbps is approximately 320 bytes
	// But actual size varies, so we read a reasonable chunk
	buf := make([]byte, 960) // Max Opus frame size
	n, err := r.file.Read(buf)
	if err != nil {
		return nil, err
	}

	return &OpusPacket{
		Data: buf[:n],
	}, nil
}

func (r *SimpleOpusReader) Reset() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	_, err := r.file.Seek(0, 0)
	return err
}

func (r *SimpleOpusReader) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.closed = true
	if r.file != nil {
		return r.file.Close()
	}
	return nil
}
