package media

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"

	"github.com/pion/opus"
)

const (
	oggOpusClockRate      = 48000
	oggOpusMaxFrameMillis = 120
	oggOpusMaxChannels    = 2
	oggOpusMaxPacketBytes = 1024 * 1024
)

var (
	oggCapturePattern = []byte("OggS")
	oggCRCValues      = makeOggCRCTable()
)

// openAudioFile routes Ogg containers through the pure-Go Opus decoder and
// leaves WAV, FLAC, and MP3 decoding to miniaudio. Detection is based on the
// container signature rather than an untrusted filename extension.
func openAudioFile(path string) (audioDecoder, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	var signature [4]byte
	_, readErr := io.ReadFull(file, signature[:])
	closeErr := file.Close()
	if readErr != nil {
		return nil, fmt.Errorf("inspect audio container: %w", readErr)
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if bytes.Equal(signature[:], oggCapturePattern) {
		return openOggOpus(path)
	}
	return openMiniaudio(path)
}

type oggOpusHead struct {
	channels     int
	preSkip      uint16
	outputGainQ8 int32
}

type oggOpusPacket struct {
	data         []byte
	granule      uint64
	granuleValid bool
	eos          bool
}

type oggOpusStream struct {
	packets *oggPacketReader
	head    oggOpusHead
}

func newOggOpusStream(source io.Reader) (*oggOpusStream, error) {
	packets := newOggPacketReader(source)
	headPacket, err := packets.readPacket()
	if err != nil {
		return nil, err
	}
	if !headPacket.bos {
		return nil, errors.New("Ogg/Opus identification packet is not at the beginning of the stream")
	}
	head, err := parseOggOpusHead(headPacket.data)
	if err != nil {
		return nil, err
	}
	tags, err := packets.readPacket()
	if err != nil {
		return nil, err
	}
	if err := validateOggOpusTags(tags.data); err != nil {
		return nil, err
	}
	return &oggOpusStream{packets: packets, head: head}, nil
}

func (s *oggOpusStream) readAudioPacket() (oggOpusPacket, error) {
	packet, err := s.packets.readPacket()
	if err != nil {
		return oggOpusPacket{}, err
	}
	if packet.bos || bytes.HasPrefix(packet.data, []byte("OpusHead")) || bytes.HasPrefix(packet.data, []byte("OpusTags")) {
		return oggOpusPacket{}, errors.New("unexpected Ogg/Opus header in audio stream")
	}
	return oggOpusPacket{data: packet.data, granule: packet.granule, granuleValid: packet.granuleValid, eos: packet.eos}, nil
}

type oggPacket struct {
	data         []byte
	granule      uint64
	granuleValid bool
	bos          bool
	eos          bool
}

type oggPacketReader struct {
	reader          *bufio.Reader
	serial          uint32
	hasSerial       bool
	nextSequence    uint32
	pending         []byte
	pendingBOS      bool
	queue           []oggPacket
	streamCompleted bool
}

func newOggPacketReader(source io.Reader) *oggPacketReader {
	return &oggPacketReader{reader: bufio.NewReaderSize(source, 64*1024)}
}

func (r *oggPacketReader) readPacket() (oggPacket, error) {
	for len(r.queue) == 0 {
		if r.streamCompleted {
			return oggPacket{}, io.EOF
		}
		if err := r.readPage(); err != nil {
			return oggPacket{}, err
		}
	}
	packet := r.queue[0]
	r.queue[0] = oggPacket{}
	r.queue = r.queue[1:]
	return packet, nil
}

func (r *oggPacketReader) readPage() error {
	var header [27]byte
	if _, err := io.ReadFull(r.reader, header[:]); err != nil {
		return err
	}
	if !bytes.Equal(header[:4], oggCapturePattern) {
		return errors.New("invalid Ogg capture pattern")
	}
	if header[4] != 0 {
		return fmt.Errorf("unsupported Ogg version %d", header[4])
	}
	segments := make([]byte, int(header[26]))
	if _, err := io.ReadFull(r.reader, segments); err != nil {
		return err
	}
	bodyBytes := 0
	for _, length := range segments {
		bodyBytes += int(length)
	}
	body := make([]byte, bodyBytes)
	if _, err := io.ReadFull(r.reader, body); err != nil {
		return err
	}
	if expected := binary.LittleEndian.Uint32(header[22:26]); oggCRC(header, segments, body) != expected {
		return errors.New("Ogg page checksum mismatch")
	}
	serial := binary.LittleEndian.Uint32(header[14:18])
	sequence := binary.LittleEndian.Uint32(header[18:22])
	if !r.hasSerial {
		r.serial = serial
		r.hasSerial = true
		r.nextSequence = sequence
	}
	if serial != r.serial {
		return errors.New("multiple logical streams in one Ogg voice attachment are unsupported")
	}
	if sequence != r.nextSequence {
		return fmt.Errorf("unexpected Ogg page sequence %d, want %d", sequence, r.nextSequence)
	}
	r.nextSequence++
	continued := header[5]&0x01 != 0
	if continued != (len(r.pending) > 0) {
		return errors.New("invalid Ogg continued-packet sequence")
	}
	if header[5]&0x02 != 0 {
		if r.pendingBOS || len(r.pending) > 0 || len(r.queue) > 0 {
			return errors.New("unexpected Ogg beginning-of-stream page")
		}
		r.pendingBOS = true
	}
	offset := 0
	pagePackets := make([]oggPacket, 0, 4)
	for _, rawLength := range segments {
		length := int(rawLength)
		if offset+length > len(body) {
			return errors.New("Ogg page segment exceeds its body")
		}
		if len(r.pending)+length > oggOpusMaxPacketBytes {
			return fmt.Errorf("Ogg packet exceeds the %d byte safety limit", oggOpusMaxPacketBytes)
		}
		r.pending = append(r.pending, body[offset:offset+length]...)
		offset += length
		if rawLength < 255 {
			pagePackets = append(pagePackets, oggPacket{data: r.pending, bos: r.pendingBOS})
			r.pending = nil
			r.pendingBOS = false
		}
	}
	if offset != len(body) {
		return errors.New("Ogg page contains unreferenced body data")
	}
	if len(pagePackets) > 0 {
		last := &pagePackets[len(pagePackets)-1]
		last.granule = binary.LittleEndian.Uint64(header[6:14])
		last.granuleValid = last.granule != math.MaxUint64
		last.eos = header[5]&0x04 != 0
	}
	if header[5]&0x04 != 0 {
		if len(r.pending) != 0 || len(pagePackets) == 0 {
			return errors.New("invalid Ogg end-of-stream page")
		}
		r.streamCompleted = true
	}
	r.queue = append(r.queue, pagePackets...)
	return nil
}

func parseOggOpusHead(data []byte) (oggOpusHead, error) {
	if len(data) < 19 || !bytes.Equal(data[:8], []byte("OpusHead")) {
		return oggOpusHead{}, errors.New("Ogg stream does not contain an Opus identification header")
	}
	if data[8] != 1 {
		return oggOpusHead{}, fmt.Errorf("unsupported Ogg/Opus header version %d", data[8])
	}
	channels := int(data[9])
	mappingFamily := data[18]
	if channels < 1 || channels > oggOpusMaxChannels || mappingFamily != 0 {
		return oggOpusHead{}, fmt.Errorf("unsupported Ogg/Opus channel layout: channels=%d mapping_family=%d", channels, mappingFamily)
	}
	outputGainQ8 := int32(binary.LittleEndian.Uint16(data[16:18]))
	if outputGainQ8 > math.MaxInt16 {
		outputGainQ8 -= 1 << 16
	}
	return oggOpusHead{
		channels: channels, preSkip: binary.LittleEndian.Uint16(data[10:12]),
		outputGainQ8: outputGainQ8,
	}, nil
}

func validateOggOpusTags(data []byte) error {
	if len(data) < 16 || !bytes.Equal(data[:8], []byte("OpusTags")) {
		return errors.New("Ogg/Opus comment header is missing or invalid")
	}
	offset := 8
	vendorBytes := int(binary.LittleEndian.Uint32(data[offset : offset+4]))
	offset += 4
	if vendorBytes > len(data)-offset {
		return errors.New("Ogg/Opus vendor field is truncated")
	}
	offset += vendorBytes
	if len(data)-offset < 4 {
		return errors.New("Ogg/Opus comment count is missing")
	}
	comments := binary.LittleEndian.Uint32(data[offset : offset+4])
	offset += 4
	if int64(comments) > int64(len(data)-offset)/4 {
		return errors.New("Ogg/Opus comment count exceeds the header size")
	}
	for range comments {
		commentBytes := int(binary.LittleEndian.Uint32(data[offset : offset+4]))
		offset += 4
		if commentBytes > len(data)-offset {
			return errors.New("Ogg/Opus comment is truncated")
		}
		offset += commentBytes
	}
	return nil
}

func makeOggCRCTable() [256]uint32 {
	var table [256]uint32
	const polynomial uint32 = 0x04C11DB7
	for index := range table {
		value := uint32(index) << 24
		for range 8 {
			if value&0x80000000 != 0 {
				value = value<<1 ^ polynomial
			} else {
				value <<= 1
			}
		}
		table[index] = value
	}
	return table
}

func oggCRC(header [27]byte, segments, body []byte) uint32 {
	header[22], header[23], header[24], header[25] = 0, 0, 0, 0
	value := uint32(0)
	for _, block := range [][]byte{header[:], segments, body} {
		for _, current := range block {
			value = value<<8 ^ oggCRCValues[byte(value>>24)^current]
		}
	}
	return value
}

type oggOpusDecoder struct {
	file          *os.File
	stream        *oggOpusStream
	decoder       *opus.Decoder
	channels      int
	gain          float32
	preSkipFrames int
	produced      int64
	decoded       []float32
	pending       []float32
	position      int
	eof           bool
}

func openOggOpus(path string) (*oggOpusDecoder, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*oggOpusDecoder, error) {
		_ = file.Close()
		return nil, err
	}
	stream, err := newOggOpusStream(file)
	if err != nil {
		return fail(fmt.Errorf("read Ogg/Opus headers: %w", err))
	}
	decoder, err := opus.NewDecoderWithOutput(parakeetSampleRate, stream.head.channels)
	if err != nil {
		return fail(fmt.Errorf("initialize Opus decoder: %w", err))
	}
	gainDB := float64(stream.head.outputGainQ8) / 256
	maxFrames := parakeetSampleRate * oggOpusMaxFrameMillis / 1000
	return &oggOpusDecoder{
		file:          file,
		stream:        stream,
		decoder:       &decoder,
		channels:      stream.head.channels,
		gain:          float32(math.Pow(10, gainDB/20)),
		preSkipFrames: scaleOpusFramesCeil(stream.head.preSkip),
		decoded:       make([]float32, maxFrames*stream.head.channels),
		pending:       make([]float32, 0, maxFrames),
	}, nil
}

func (d *oggOpusDecoder) ReadPCMFrames(output []float32) (uint64, error) {
	if d == nil || d.decoder == nil || d.file == nil {
		return 0, errors.New("Ogg/Opus decoder is closed")
	}
	written := 0
	for written < len(output) {
		if d.position < len(d.pending) {
			copied := copy(output[written:], d.pending[d.position:])
			d.position += copied
			written += copied
			continue
		}
		d.pending = d.pending[:0]
		d.position = 0
		if d.eof {
			return uint64(written), io.EOF
		}
		if err := d.decodePacket(); err != nil {
			if errors.Is(err, io.EOF) {
				d.eof = true
				return uint64(written), io.EOF
			}
			return uint64(written), err
		}
	}
	return uint64(written), nil
}

func (d *oggOpusDecoder) decodePacket() error {
	packet, err := d.stream.readAudioPacket()
	if err != nil {
		return err
	}
	frames, err := d.decoder.DecodeToFloat32(packet.data, d.decoded)
	if err != nil {
		return fmt.Errorf("decode Opus packet: %w", err)
	}
	start := min(d.preSkipFrames, frames)
	d.preSkipFrames -= start
	frames -= start
	if packet.granuleValid {
		maximum := playableOpusFrames(packet.granule, d.stream.head.preSkip)
		if remaining := maximum - d.produced; remaining < int64(frames) {
			frames = max(0, int(remaining))
		}
	}
	if frames > 0 {
		d.pending = d.pending[:frames]
		for frame := range frames {
			base := (start + frame) * d.channels
			sample := float32(0)
			for channel := range d.channels {
				sample += d.decoded[base+channel]
			}
			sample = sample / float32(d.channels) * d.gain
			d.pending[frame] = max(-1, min(1, sample))
		}
		d.produced += int64(frames)
	}
	if packet.eos {
		d.eof = true
	}
	return nil
}

func (d *oggOpusDecoder) Close() {
	if d == nil {
		return
	}
	if d.decoder != nil {
		d.decoder = nil
	}
	if d.file != nil {
		_ = d.file.Close()
		d.file = nil
	}
}

func scaleOpusFramesCeil(frames uint16) int {
	return (int(frames) + oggOpusClockRate/parakeetSampleRate - 1) / (oggOpusClockRate / parakeetSampleRate)
}

func playableOpusFrames(granule uint64, preSkip uint16) int64 {
	if granule <= uint64(preSkip) {
		return 0
	}
	frames := (granule - uint64(preSkip)) / (oggOpusClockRate / parakeetSampleRate)
	if frames > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(frames)
}
