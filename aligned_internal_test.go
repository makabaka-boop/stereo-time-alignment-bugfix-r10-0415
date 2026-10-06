package rtpaudio

import (
	"testing"
	"time"
)

func TestAlignSegmentsRejectsOverlappingRecords(t *testing.T) {
	start := time.Date(2026, 10, 6, 14, 0, 0, 0, time.UTC)
	frame := func(index uint64, at time.Time) OutputFrame {
		return OutputFrame{Generation: 1, Index: index, At: at}
	}
	overlaps := [][]OutputFrame{
		// Second frame starts inside the first frame's 20 ms slot.
		{frame(0, start), frame(1, start.Add(10*time.Millisecond))},
		// Two records for the same output time.
		{frame(0, start), frame(1, start)},
		// Overlap outside the export range still corrupts the record.
		{frame(0, start.Add(time.Hour)), frame(1, start.Add(time.Hour).Add(10*time.Millisecond))},
	}
	for i, frames := range overlaps {
		if _, err := alignSegments(frames, start, 1600); err == nil {
			t.Fatalf("case %d: overlapping records were accepted", i)
		}
	}
}

func TestAlignSegmentsGridAlignment(t *testing.T) {
	start := time.Date(2026, 10, 6, 14, 30, 0, 0, time.UTC)

	// An intersecting frame 40 µs off the 125 µs sample grid fails.
	offGrid := []OutputFrame{{Generation: 1, Index: 0, At: start.Add(40 * time.Microsecond)}}
	if _, err := alignSegments(offGrid, start, 1600); err == nil {
		t.Fatal("intersecting off-grid frame was accepted")
	}

	// The same off-grid frame fully outside the range contributes nothing
	// and does not fail the export.
	outside := []OutputFrame{
		{Generation: 1, Index: 0, At: start.Add(-time.Hour).Add(40 * time.Microsecond)},
		{Generation: 1, Index: 1, At: start.Add(time.Hour).Add(40 * time.Microsecond)},
	}
	segments, err := alignSegments(outside, start, 1600)
	if err != nil {
		t.Fatalf("frames outside the range rejected the export: %v", err)
	}
	if len(segments) != 0 {
		t.Fatalf("segments = %+v, want none outside the range", segments)
	}
}

func TestAlignSegmentsClipsToRange(t *testing.T) {
	start := time.Date(2026, 10, 6, 15, 0, 0, 0, time.UTC)
	frames := []OutputFrame{
		{Generation: 1, Index: 0, At: start.Add(-10 * time.Millisecond)}, // tail visible
		{Generation: 1, Index: 1, At: start.Add(10 * time.Millisecond)},  // fully visible
		{Generation: 1, Index: 2, At: start.Add(30 * time.Millisecond)},  // head visible
		{Generation: 1, Index: 3, At: start.Add(time.Hour)},              // outside
	}
	segments, err := alignSegments(frames, start, 320) // 40 ms
	if err != nil {
		t.Fatal(err)
	}
	want := []frameSegment{
		{frame: frames[0], sourceOffset: 80, outputOffset: 0, count: 80},
		{frame: frames[1], sourceOffset: 0, outputOffset: 80, count: 160},
		{frame: frames[2], sourceOffset: 0, outputOffset: 240, count: 80},
	}
	if len(segments) != len(want) {
		t.Fatalf("segments = %+v, want %+v", segments, want)
	}
	for i, seg := range segments {
		if seg.sourceOffset != want[i].sourceOffset ||
			seg.outputOffset != want[i].outputOffset ||
			seg.count != want[i].count ||
			seg.frame.Index != want[i].frame.Index {
			t.Fatalf("segment %d = %+v, want offsets %+v", i, seg, want[i])
		}
	}
}
