package vecfmt

import (
	"errors"
	"math"
	"math/rand/v2"
	"slices"
	"testing"
)

func randVec(r *rand.Rand, n int) []float32 {
	v := make([]float32, n)
	for i := range v {
		v[i] = float32(r.NormFloat64())
	}
	return v
}

func cosine(a, b []float32) float64 {
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	return dot / math.Sqrt(na*nb)
}

func TestTruncateNormalizes(t *testing.T) {
	v := randVec(rand.New(rand.NewPCG(1, 2)), 768)
	got, err := Truncate(v)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != Dim {
		t.Fatalf("len = %d", len(got))
	}
	var sum float64
	for _, x := range got {
		sum += float64(x) * float64(x)
	}
	if math.Abs(sum-1) > 1e-5 {
		t.Fatalf("norm² = %v, want 1", sum)
	}
	// Direction of the first 256 dims is unchanged.
	if c := cosine(got, v[:Dim]); c < 0.99999 {
		t.Fatalf("cosine to original prefix = %v", c)
	}
}

func TestTruncateRejectsBadVectors(t *testing.T) {
	nan := make([]float32, 768)
	nan[3] = float32(math.NaN())
	inf := make([]float32, 768)
	inf[0] = float32(math.Inf(1))
	for name, v := range map[string][]float32{
		"short": make([]float32, 100), "zero": make([]float32, 768), "nan": nan, "inf": inf,
	} {
		if _, err := Truncate(v); !errors.Is(err, ErrBadVector) {
			t.Errorf("%s: got %v, want ErrBadVector", name, err)
		}
	}
}

func TestBinaryAndHamming(t *testing.T) {
	v := make([]float32, Dim)
	for i := range v {
		v[i] = -1
	}
	v[0], v[9], v[255] = 1, 1, 1
	b := ToBinary(v)
	if b[0] != 1 || b[1] != 2 || b[31] != 0x80 {
		t.Fatalf("bits = %v", b)
	}
	var zero Bin
	if h := Hamming(&b, &zero); h != 3 {
		t.Fatalf("Hamming = %d, want 3", h)
	}
	all := Bin{}
	for i := range all {
		all[i] = 0xFF
	}
	if h := Hamming(&zero, &all); h != Dim {
		t.Fatalf("Hamming = %d, want %d", h, Dim)
	}
}

// Int8 dot product must track the real cosine closely, and Hamming must rank roughly
// the same way, otherwise search quality drops.
func TestQuantizedSimilarityTracksCosine(t *testing.T) {
	r := rand.New(rand.NewPCG(3, 4))
	q, _ := Truncate(randVec(r, 768))
	qb, qi := ToBinary(q), ToInt8(q)
	var maxErr float64
	for range 500 {
		d := randVec(r, 768)
		// Mix in some of q so similarities spread over a useful range.
		mix := r.Float32()
		for i := 0; i < Dim; i++ {
			d[i] = d[i]*(1-mix) + q[i]*mix*20
		}
		dt, err := Truncate(d)
		if err != nil {
			t.Fatal(err)
		}
		di := ToInt8(dt)
		real := cosine(q, dt)
		maxErr = max(maxErr, math.Abs(float64(Dot(&qi, &di))-real))
		db := ToBinary(dt)
		// Hamming approximates angle: cos(pi * h / Dim).
		approx := math.Cos(math.Pi * float64(Hamming(&qb, &db)) / Dim)
		if math.Abs(approx-real) > 0.25 {
			t.Errorf("binary estimate %.3f vs cosine %.3f", approx, real)
		}
	}
	if maxErr > 0.02 {
		t.Fatalf("int8 dot max error %.4f > 0.02", maxErr)
	}
}

func TestEncode(t *testing.T) {
	_, q, err := Encode(randVec(rand.New(rand.NewPCG(5, 6)), 768))
	if err != nil {
		t.Fatal(err)
	}
	if d := Dot(&q, &q); math.Abs(float64(d)-1) > 0.02 {
		t.Fatalf("self dot = %v, want ≈1", d)
	}
}

// Plan 9.9: a binary scan of 100k vectors must take < 5 ms on a mid laptop.
func BenchmarkHamming100k(b *testing.B) {
	r := rand.New(rand.NewPCG(7, 8))
	vecs := make([]Bin, 100_000)
	for i := range vecs {
		vecs[i] = ToBinary(randVec(r, Dim))
	}
	q := ToBinary(randVec(r, Dim))
	b.ResetTimer()
	for b.Loop() {
		best := Dim
		for i := range vecs {
			best = min(best, Hamming(&q, &vecs[i]))
		}
		_ = best
	}
}

func BenchmarkDot200(b *testing.B) {
	r := rand.New(rand.NewPCG(9, 10))
	vecs := make([]Int8, 200)
	for i := range vecs {
		t, _ := Truncate(randVec(r, 768))
		vecs[i] = ToInt8(t)
	}
	q := vecs[0]
	for b.Loop() {
		var s float32
		for i := range vecs {
			s += Dot(&q, &vecs[i])
		}
		_ = s
	}
}

func TestToInt8TinyValues(t *testing.T) {
	v := make([]float32, Dim)
	v[0] = 1e-44
	if q := ToInt8(v); q.Scale != 0 || q.I8[0] != 0 {
		t.Fatalf("got scale=%v i8[0]=%d", q.Scale, q.I8[0])
	}
}

// Search keeps the top 200 by Hamming distance, then re-ranks with int8. That only works
// if the true top-10 (by real cosine) are almost always inside that top 200.
func TestBinaryPrefilterRecall(t *testing.T) {
	r := rand.New(rand.NewPCG(11, 12))
	const nDocs, nClusters, nQueries = 20000, 200, 50
	centers := make([][]float32, nClusters)
	for i := range centers {
		centers[i] = randVec(r, Dim)
	}
	noisy := func(c []float32, noise float32) []float32 {
		v := make([]float32, Dim)
		for i := range v {
			v[i] = c[i] + noise*float32(r.NormFloat64())
		}
		out, _ := Truncate(v)
		return out
	}
	docs := make([][]float32, nDocs)
	bins := make([]Bin, nDocs)
	for i := range docs {
		docs[i] = noisy(centers[i%nClusters], 0.8)
		bins[i] = ToBinary(docs[i])
	}
	var hits, total int
	for qi := range nQueries {
		q := noisy(centers[qi*3%nClusters], 0.8)
		qb := ToBinary(q)
		// True top 10 by cosine.
		type scored struct {
			i int
			s float64
		}
		all := make([]scored, nDocs)
		for i := range docs {
			all[i] = scored{i, cosine(q, docs[i])}
		}
		sortDesc(all, func(a, b scored) bool { return a.s > b.s })
		// Top 200 by Hamming.
		byHam := make([]scored, nDocs)
		for i := range bins {
			byHam[i] = scored{i, -float64(Hamming(&qb, &bins[i]))}
		}
		sortDesc(byHam, func(a, b scored) bool { return a.s > b.s })
		in200 := map[int]bool{}
		for _, s := range byHam[:200] {
			in200[s.i] = true
		}
		for _, s := range all[:10] {
			total++
			if in200[s.i] {
				hits++
			}
		}
	}
	recall := float64(hits) / float64(total)
	t.Logf("binary top-200 recall@10 = %.3f", recall)
	if recall < 0.9 {
		t.Fatalf("recall %.3f < 0.9", recall)
	}
}

func sortDesc[T any](s []T, less func(a, b T) bool) {
	slices.SortFunc(s, func(a, b T) int {
		if less(a, b) {
			return -1
		}
		if less(b, a) {
			return 1
		}
		return 0
	})
}
