// Package wire is a Roborock device's frame as Roborock's cloud carries
// it, and the encryption of a frame's payload under the device's key, in
// the version the devices with no protocol on the LAN speak: B01.
//
// Roborock documents none of it. It follows python-roborock, and is held
// to the frames of that project's own tests.
package wire

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5" // #nosec G501 -- how the protocol derives a vector, no choice of this package
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/crc32"
)

// What a frame is, by its protocol number.
const (
	// Request is what is asked of a device, and Response its answer.
	Request  uint16 = 101
	Response uint16 = 102
	// MapPart is a part of a map that is not its last, and Map a map or
	// its last part.
	MapPart uint16 = 300
	Map     uint16 = 301
)

const (
	// version is the first three bytes of a frame.
	version = "B01"
	// header is a frame's fields before its payload: the version, three
	// numbers of four bytes and the protocol.
	header = 17
	// counted is the size of a payload's length, and trailer of the
	// checksum that ends a frame.
	counted = 2
	trailer = 4
	// LargestPayload is the longest payload a frame carries: its length
	// is written in two bytes.
	LargestPayload = 1<<16 - 1
	// salt is what a vector is derived with, the same for every device.
	salt = "5wwh9ikChRjASpMU8cxg7o1d2E"
)

var (
	// ErrFrame is bytes that are no frame: too short, of another
	// version, or cut.
	ErrFrame = errors.New("wire: malformed frame")
	// ErrChecksum is a frame whose checksum is not its content's.
	ErrChecksum = errors.New("wire: the frame's checksum does not match")
	// ErrKey is a key that is not one a device has: sixteen bytes.
	ErrKey = errors.New("wire: a device's key is sixteen bytes")
	// ErrSealed is a payload that does not open: not whole blocks, or
	// not ended as one sealed under this key is. It is what a wrong key
	// gives, most times.
	ErrSealed = errors.New("wire: the payload does not open under the key")
)

// Frame is one frame. Its payload is as it is on the wire: sealed.
type Frame struct {
	// Seq numbers the frame for whoever sent it.
	Seq uint32
	// Random is a number of the frame's own, which its payload is sealed
	// with.
	Random uint32
	// Timestamp is when the frame was made, in seconds.
	Timestamp uint32
	// Protocol is what the frame is.
	Protocol uint16
	// Payload is its sealed content; nil for a frame with none.
	Payload []byte
}

// Encode is the frame as the cloud carries it: its fields, its payload
// and the checksum of both.
func (f Frame) Encode() ([]byte, error) {
	if len(f.Payload) == 0 || len(f.Payload) > LargestPayload {
		return nil, fmt.Errorf("%w: a payload of %d bytes", ErrFrame, len(f.Payload))
	}

	body := make([]byte, 0, header+counted+len(f.Payload)+trailer)
	body = append(body, version...)
	body = binary.BigEndian.AppendUint32(body, f.Seq)
	body = binary.BigEndian.AppendUint32(body, f.Random)
	body = binary.BigEndian.AppendUint32(body, f.Timestamp)
	body = binary.BigEndian.AppendUint16(body, f.Protocol)
	body = binary.BigEndian.AppendUint16(body, uint16(len(f.Payload))) // #nosec G115 -- bounded above
	body = append(body, f.Payload...)

	return binary.BigEndian.AppendUint32(body, crc32.ChecksumIEEE(body)), nil
}

// Decode reads the frames of one message of the cloud's: one after the
// other, each whole. A frame whose length is zero has no payload, and no
// checksum after it. What is not frames end to end is ErrFrame, with the
// frames before it.
func Decode(message []byte) ([]Frame, error) {
	var frames []Frame

	for len(message) > 0 {
		if len(message) < header+counted || string(message[:len(version)]) != version {
			return frames, fmt.Errorf("%w: %d bytes that begin no frame", ErrFrame, len(message))
		}

		frame := Frame{
			Seq:       binary.BigEndian.Uint32(message[3:]),
			Random:    binary.BigEndian.Uint32(message[7:]),
			Timestamp: binary.BigEndian.Uint32(message[11:]),
			Protocol:  binary.BigEndian.Uint16(message[15:]),
		}

		length := int(binary.BigEndian.Uint16(message[header:]))
		if length == 0 {
			frames = append(frames, frame)
			message = message[header+counted:]

			continue
		}

		end := header + counted + length
		if len(message) < end+trailer {
			return frames, fmt.Errorf("%w: a payload of %d bytes in %d", ErrFrame, length, len(message))
		}

		if binary.BigEndian.Uint32(message[end:]) != crc32.ChecksumIEEE(message[:end]) {
			return frames, ErrChecksum
		}

		frame.Payload = bytes.Clone(message[header+counted : end])
		frames = append(frames, frame)
		message = message[end+trailer:]
	}

	return frames, nil
}

// Seal encrypts a payload under a device's key, for a frame whose random
// number is the one given: AES-128 in CBC mode, the content padded to
// whole blocks.
func Seal(key string, random uint32, plain []byte) ([]byte, error) {
	mode, err := blocks(key, random, cipher.NewCBCEncrypter)
	if err != nil {
		return nil, err
	}

	sealed := Pad(plain)
	mode.CryptBlocks(sealed, sealed)

	return sealed, nil
}

// Open decrypts a frame's payload under a device's key and the frame's
// random number. What it decrypted to is in no error: under a wrong key
// it is noise, and under the right one a device's words.
//
// Nothing authenticates a payload. Most of what a wrong key decrypts to
// does not end as a sealed content does, and is ErrSealed; some does, and
// is returned as noise.
func Open(key string, random uint32, sealed []byte) ([]byte, error) {
	mode, err := blocks(key, random, cipher.NewCBCDecrypter)
	if err != nil {
		return nil, err
	}

	if len(sealed) == 0 || len(sealed)%aes.BlockSize != 0 {
		return nil, fmt.Errorf("%w: %d bytes are not whole blocks", ErrSealed, len(sealed))
	}

	plain := make([]byte, len(sealed))
	mode.CryptBlocks(plain, sealed)

	plain, padded := Unpad(plain)
	if !padded {
		return nil, ErrSealed
	}

	return plain, nil
}

// blocks is the cipher for one frame: the key as it is, and a vector that
// is sixteen characters of a digest of the frame's random number, as text.
func blocks(key string, random uint32, mode func(cipher.Block, []byte) cipher.BlockMode) (cipher.BlockMode, error) {
	if len(key) != aes.BlockSize {
		return nil, ErrKey
	}

	block, err := aes.NewCipher([]byte(key))
	if err != nil {
		return nil, ErrKey
	}

	// #nosec G401 -- how the protocol derives a vector, no choice of this package
	digest := md5.Sum(fmt.Appendf(nil, "%08x%s", random, salt))

	const from, to = 9, 25

	return mode(block, []byte(hex.EncodeToString(digest[:])[from:to])), nil
}

// Pad is a content padded to whole blocks, each added byte the count of
// them: a whole block of them when it was whole blocks already.
func Pad(plain []byte) []byte {
	count := aes.BlockSize - len(plain)%aes.BlockSize

	return append(bytes.Clone(plain), bytes.Repeat([]byte{byte(count)}, count)...)
}

// Unpad is a content without its padding, and whether it ended in one.
func Unpad(padded []byte) ([]byte, bool) {
	if len(padded) == 0 || len(padded)%aes.BlockSize != 0 {
		return padded, false
	}

	count := int(padded[len(padded)-1])
	if count == 0 || count > aes.BlockSize ||
		!bytes.Equal(padded[len(padded)-count:], bytes.Repeat([]byte{byte(count)}, count)) {
		return padded, false
	}

	return padded[:len(padded)-count], true
}
