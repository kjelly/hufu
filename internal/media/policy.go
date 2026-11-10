package media

const (
	MaxCount                = 8
	MaxBytesPerImage  int64 = 16 << 20
	MaxBytesPerBundle int64 = 32 << 20
	MaxPixels         int64 = 36_000_000
	MaxSide                 = 8192
	MaxBranchBytes    int64 = 128 << 20
	MaxBranchBlobs          = 64
)

// Limits are operator ceilings; zero selects the hard ceiling, never unlimited.
type Limits struct {
	MaxCount          int   `json:"max_count" yaml:"max-count"`
	MaxBytesPerImage  int64 `json:"max_bytes_per_image" yaml:"max-bytes-per-image"`
	MaxBytesPerBundle int64 `json:"max_bytes_per_bundle" yaml:"max-bytes-per-bundle"`
	MaxPixels         int64 `json:"max_pixels" yaml:"max-pixels"`
	MaxSide           int   `json:"max_side" yaml:"max-side"`
	MaxBranchBytes    int64 `json:"max_branch_bytes" yaml:"max-branch-bytes"`
	MaxBranchBlobs    int   `json:"max_branch_blobs" yaml:"max-branch-blobs"`
}

func (l Limits) Resolve() (Limits, error) {
	ceilings := []struct {
		value   *int64
		ceiling int64
	}{
		{&l.MaxBytesPerImage, MaxBytesPerImage}, {&l.MaxBytesPerBundle, MaxBytesPerBundle},
		{&l.MaxPixels, MaxPixels}, {&l.MaxBranchBytes, MaxBranchBytes},
	}
	for _, c := range ceilings {
		if *c.value < 0 || *c.value > c.ceiling {
			return Limits{}, Reject(LimitExceeded, "configured limit exceeds hard ceiling")
		}
		if *c.value == 0 {
			*c.value = c.ceiling
		}
	}
	counts := []struct {
		value   *int
		ceiling int
	}{{&l.MaxCount, MaxCount}, {&l.MaxSide, MaxSide}, {&l.MaxBranchBlobs, MaxBranchBlobs}}
	for _, c := range counts {
		if *c.value < 0 || *c.value > c.ceiling {
			return Limits{}, Reject(LimitExceeded, "configured count exceeds hard ceiling")
		}
		if *c.value == 0 {
			*c.value = c.ceiling
		}
	}
	return l, nil
}

// CheckBundle counts occurrences, including repeated references. Branch storage
// accounting instead deduplicates immutable blobs in the admitted lineage.
func CheckBundle(refs []Ref, limits Limits) error {
	l, err := limits.Resolve()
	if err != nil {
		return err
	}
	if len(refs) == 0 || len(refs) > l.MaxCount {
		return Reject(LimitExceeded, "invalid image count")
	}
	var total int64
	for _, r := range refs {
		if err := r.Validate(); err != nil {
			return err
		}
		if r.ByteSize > l.MaxBytesPerImage || r.Width > l.MaxSide || r.Height > l.MaxSide || int64(r.Width)*int64(r.Height) > l.MaxPixels {
			return Reject(LimitExceeded, "image exceeds policy")
		}
		total += r.ByteSize // At most eight validated 16 MiB refs; cannot overflow.
	}
	if total > l.MaxBytesPerBundle {
		return Reject(LimitExceeded, "bundle exceeds byte budget")
	}
	return nil
}
