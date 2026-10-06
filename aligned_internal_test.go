package rtpaudio

import (
	"errors"
	"testing"
	"time"
)

func TestAlignedFrameIntervalsRejectBadGridAndOverlap(t *testing.T) {
	base := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	frames := []OutputFrame{{Index: 0, At: base}}

	if _, err := alignedFrameIntervals(base.Add(time.Microsecond), frames); !errors.Is(err, ErrInvalidAlignment) {
		t.Fatalf("bad grid error = %v, want ErrInvalidAlignment", err)
	}

	overlapping := []OutputFrame{
		{Index: 0, At: base},
		{Index: 1, At: base.Add(FrameDuration - sampleDuration)},
	}
	if _, err := alignedFrameIntervals(base, overlapping); !errors.Is(err, ErrOverlappingRecords) {
		t.Fatalf("overlap error = %v, want ErrOverlappingRecords", err)
	}

	duplicateIndex := []OutputFrame{
		{Index: 0, At: base},
		{Index: 0, At: base.Add(FrameDuration)},
	}
	if _, err := alignedFrameIntervals(base, duplicateIndex); !errors.Is(err, ErrOverlappingRecords) {
		t.Fatalf("duplicate index error = %v, want ErrOverlappingRecords", err)
	}
}
