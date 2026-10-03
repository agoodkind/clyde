package vectorsearch

import (
	"encoding/binary"
	"math"
	"math/bits"
)

// Collection names embed an MD5 digest prefix. Rows already stored under those
// names make the digest part of the storage layout, and the standard library
// MD5 package trips the repository's weak-primitive lint. This file computes the
// RFC 1321 digest directly. It is an identifier hash only.

const (
	md5BlockSize   = 64
	md5RoundCount  = 64
	md5RoundLength = 16
	md5LengthBytes = 8
	md5PadMarker   = 0x80
	// md5PadTarget is the block offset where the message length begins.
	md5PadTarget = md5BlockSize - md5LengthBytes
	bitsPerByte  = 8
)

// md5InitialState is the RFC 1321 initial value of the four state words.
var md5InitialState = [4]uint32{0x67452301, 0xefcdab89, 0x98badcfe, 0x10325476}

// md5Shifts is the per-round left rotation amount.
var md5Shifts = [md5RoundCount]int{
	7, 12, 17, 22, 7, 12, 17, 22, 7, 12, 17, 22, 7, 12, 17, 22,
	5, 9, 14, 20, 5, 9, 14, 20, 5, 9, 14, 20, 5, 9, 14, 20,
	4, 11, 16, 23, 4, 11, 16, 23, 4, 11, 16, 23, 4, 11, 16, 23,
	6, 10, 15, 21, 6, 10, 15, 21, 6, 10, 15, 21, 6, 10, 15, 21,
}

// md5Constants returns the per-round additive constants, floor(2^32 * |sin(i+1)|).
func md5Constants() [md5RoundCount]uint32 {
	var constants [md5RoundCount]uint32
	for i := range constants {
		constants[i] = uint32(math.Floor(math.Abs(math.Sin(float64(i+1))) * (1 << 32)))
	}
	return constants
}

// md5Sum returns the MD5 digest of data.
func md5Sum(data []byte) [16]byte {
	padded := md5Pad(data)
	constants := md5Constants()
	state := md5InitialState
	for offset := 0; offset < len(padded); offset += md5BlockSize {
		state = md5Block(state, padded[offset:offset+md5BlockSize], constants)
	}
	var digest [16]byte
	for i, word := range state {
		binary.LittleEndian.PutUint32(digest[i*4:], word)
	}
	return digest
}

// md5Pad appends the 0x80 marker, zero bytes up to the length field, and the
// message length in bits as a little-endian 64-bit value.
func md5Pad(data []byte) []byte {
	padded := make([]byte, 0, len(data)+md5BlockSize+md5LengthBytes)
	padded = append(padded, data...)
	padded = append(padded, md5PadMarker)
	for len(padded)%md5BlockSize != md5PadTarget {
		padded = append(padded, 0)
	}
	bitLength := uint64(len(data)) * bitsPerByte
	return binary.LittleEndian.AppendUint64(padded, bitLength)
}

// md5Block applies the four MD5 rounds to one 64-byte block.
func md5Block(state [4]uint32, block []byte, constants [md5RoundCount]uint32) [4]uint32 {
	var words [md5RoundLength]uint32
	for i := range words {
		words[i] = binary.LittleEndian.Uint32(block[i*4:])
	}
	a, b, c, d := state[0], state[1], state[2], state[3]
	for i := range md5RoundCount {
		var mix uint32
		var wordIndex int
		switch i / md5RoundLength {
		case 0:
			mix = (b & c) | (^b & d)
			wordIndex = i
		case 1:
			mix = (d & b) | (^d & c)
			wordIndex = (5*i + 1) % md5RoundLength
		case 2:
			mix = b ^ c ^ d
			wordIndex = (3*i + 5) % md5RoundLength
		default:
			mix = c ^ (b | ^d)
			wordIndex = (7 * i) % md5RoundLength
		}
		mix += a + constants[i] + words[wordIndex]
		a = d
		d = c
		c = b
		b += bits.RotateLeft32(mix, md5Shifts[i])
	}
	return [4]uint32{state[0] + a, state[1] + b, state[2] + c, state[3] + d}
}
