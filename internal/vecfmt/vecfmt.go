// Package vecfmt turns model embeddings into the compact forms Findly stores and searches
// (plan 9.9):
//
//   - Truncate: keep the first 256 of 768 dimensions (Matryoshka) and L2-normalize again.
//   - Binary: one sign bit per dimension, 32 bytes. Hamming distance between two of these
//     is a very fast first filter over every vector in the index.
//   - Int8: each dimension scaled to -127..127 plus one float scale, 256 bytes. The dot
//     product of two of these closely matches the real cosine similarity, used to
//     re-rank the best candidates from the binary scan.
package vecfmt

import (
	"errors"
	"math"
	"math/bits"
)

// Dim is the stored vector size.
const Dim = 256

// Bin is a binary vector: bit i is 1 when dimension i is positive.
type Bin [Dim / 8]byte

// Int8 is a quantized vector; the real value of dimension i is I8[i] * Scale.
type Int8 struct {
	I8    [Dim]int8
	Scale float32
}

// ErrBadVector is returned for vectors that are too short, all zero, or contain NaN/Inf.
// A GPU backend computing in float16 can silently produce these (plan section 7).
var ErrBadVector = errors.New("vecfmt: invalid embedding vector")

// Truncate keeps the first Dim dimensions of v and scales them to length 1.
func Truncate(v []float32) ([]float32, error) {
	if len(v) < Dim {
		return nil, ErrBadVector
	}
	out := make([]float32, Dim)
	var sum float64
	for i := range out {
		x := float64(v[i])
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return nil, ErrBadVector
		}
		out[i] = v[i]
		sum += x * x
	}
	if sum == 0 {
		return nil, ErrBadVector
	}
	inv := 1 / math.Sqrt(sum)
	for i := range out {
		out[i] = float32(float64(out[i]) * inv)
	}
	return out, nil
}

// ToBinary sets bit i when v[i] > 0. Bit order: dimension i is bit (i % 8) of byte i/8.
func ToBinary(v []float32) Bin {
	var b Bin
	for i := 0; i < Dim && i < len(v); i++ {
		if v[i] > 0 {
			b[i/8] |= 1 << (i % 8)
		}
	}
	return b
}

// ToInt8 quantizes v with scale = max|v| / 127, so the largest value maps to ±127.
func ToInt8(v []float32) Int8 {
	var q Int8
	var maxAbs float32
	for i := 0; i < Dim && i < len(v); i++ {
		maxAbs = max(maxAbs, float32(math.Abs(float64(v[i]))))
	}
	if maxAbs == 0 {
		return q
	}
	q.Scale = maxAbs / 127
	for i := 0; i < Dim && i < len(v); i++ {
		q.I8[i] = int8(math.Round(float64(v[i] / q.Scale)))
	}
	return q
}

// Encode runs Truncate, then returns both compact forms.
func Encode(v []float32) (Bin, Int8, error) {
	t, err := Truncate(v)
	if err != nil {
		return Bin{}, Int8{}, err
	}
	return ToBinary(t), ToInt8(t), nil
}

// Hamming counts the bits where a and b differ (0 = same direction in every dimension,
// 256 = opposite). Smaller means more similar.
func Hamming(a, b *Bin) int {
	n := 0
	for i := 0; i < len(a); i += 8 {
		x := uint64(a[i]) | uint64(a[i+1])<<8 | uint64(a[i+2])<<16 | uint64(a[i+3])<<24 |
			uint64(a[i+4])<<32 | uint64(a[i+5])<<40 | uint64(a[i+6])<<48 | uint64(a[i+7])<<56
		y := uint64(b[i]) | uint64(b[i+1])<<8 | uint64(b[i+2])<<16 | uint64(b[i+3])<<24 |
			uint64(b[i+4])<<32 | uint64(b[i+5])<<40 | uint64(b[i+6])<<48 | uint64(b[i+7])<<56
		n += bits.OnesCount64(x ^ y)
	}
	return n
}

// Dot approximates the cosine similarity of the two original (normalized) vectors.
func Dot(a, b *Int8) float32 {
	var sum int32
	for i := range a.I8 {
		sum += int32(a.I8[i]) * int32(b.I8[i])
	}
	return float32(sum) * a.Scale * b.Scale
}
