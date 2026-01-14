package media

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"sync"
)

type MediaConfig struct {
	VideoPath    string
	AudioPath    string
	VideoEnabled bool
	AudioEnabled bool
}

// H.264 NAL unit
type NALUnit struct {
	Data []byte
}

// Reads H.264 NAL units from an Annex B stream
type H264Reader struct {
	reader    *bufio.Reader
	file      *os.File
	mu        sync.Mutex
	closed    bool
	nalBuffer []byte
}

func NewH264Reader(path string) (*H264Reader, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("failed to open H.264 file: %w", err)
	}

	return &H264Reader{
		reader:    bufio.NewReaderSize(file, 1024*1024), // 1MB buffer
		file:      file,
		nalBuffer: make([]byte, 0, 1024*1024),
	}, nil
}

// NextNAL reads the next NAL unit from the H.264 stream
// H.264 Annex B format uses start codes: 0x00 0x00 0x01 or 0x00 0x00 0x00 0x01
func (r *H264Reader) NextNAL() (*NALUnit, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.closed {
		return nil, io.EOF
	}

	// Reset buffer
	r.nalBuffer = r.nalBuffer[:0]

	// Find start code
	if err := r.skipToStartCode(); err != nil {
		return nil, err
	}

	// Read until next start code or EOF
	for {
		b, err := r.reader.ReadByte()
		if err != nil {
			if err == io.EOF && len(r.nalBuffer) > 0 {
				// Return last NAL
				nal := &NALUnit{Data: make([]byte, len(r.nalBuffer))}
				copy(nal.Data, r.nalBuffer)
				return nal, nil
			}
			return nil, err
		}

		r.nalBuffer = append(r.nalBuffer, b)

		// Check for start code at end of buffer
		bufLen := len(r.nalBuffer)
		if bufLen >= 4 {
			// Check for 4-byte start code
			if r.nalBuffer[bufLen-4] == 0 && r.nalBuffer[bufLen-3] == 0 &&
				r.nalBuffer[bufLen-2] == 0 && r.nalBuffer[bufLen-1] == 1 {
				// Found start code, return NAL without it
				nal := &NALUnit{Data: make([]byte, bufLen-4)}
				copy(nal.Data, r.nalBuffer[:bufLen-4])
				r.nalBuffer = r.nalBuffer[:0]
				return nal, nil
			}
		}
		if bufLen >= 3 {
			// Check for 3-byte start code
			if r.nalBuffer[bufLen-3] == 0 && r.nalBuffer[bufLen-2] == 0 &&
				r.nalBuffer[bufLen-1] == 1 {
				// Found start code, return NAL without it
				nal := &NALUnit{Data: make([]byte, bufLen-3)}
				copy(nal.Data, r.nalBuffer[:bufLen-3])
				r.nalBuffer = r.nalBuffer[:0]
				return nal, nil
			}
		}
	}
}

func (r *H264Reader) skipToStartCode() error {
	zeroCount := 0
	for {
		b, err := r.reader.ReadByte()
		if err != nil {
			return err
		}

		if b == 0 {
			zeroCount++
		} else if b == 1 && zeroCount >= 2 {
			// Found start code
			return nil
		} else {
			zeroCount = 0
		}
	}
}

// Resets the reader to the beginning of the file
func (r *H264Reader) Reset() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, err := r.file.Seek(0, 0); err != nil {
		return err
	}
	r.reader.Reset(r.file)
	r.nalBuffer = r.nalBuffer[:0]
	return nil
}

// Closes the H.264 reader
func (r *H264Reader) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.closed = true
	if r.file != nil {
		return r.file.Close()
	}
	return nil
}
