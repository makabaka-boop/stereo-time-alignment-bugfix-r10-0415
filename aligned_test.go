package rtpaudio_test

import (
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"rtpaudio"
)

const (
	alignSSRCA uint32 = 0x0A0A0A0A
	alignSSRCB uint32 = 0x0B0B0B0B
)

var (
	alignKeyA = rtpaudio.SourceKey("192.0.2.10:5000")
	alignKeyB = rtpaudio.SourceKey("192.0.2.11:5000")
)

// feedPackets submits count consecutive packets starting at firstSeq, all
// stamped with the same arrival time. Only the first arrival of a generation
// matters for playout scheduling.
func feedPackets(t *testing.T, r *rtpaudio.Receiver, key rtpaudio.SourceKey, ssrc uint32, firstSeq uint16, firstTS uint32, count int, arrival time.Time) {
	t.Helper()
	for i := 0; i < count; i++ {
		seq := firstSeq + uint16(i)
		ts := firstTS + uint32(i)*rtpaudio.SamplesPerPacket
		status := r.HandlePacket(key, makeRTP(seq, ts, ssrc), arrival)
		if !status.Accepted {
			t.Fatalf("packet seq %d rejected: %s: %v", seq, status.Reason, status.Err)
		}
	}
}

func newAlignReceiver(t0 time.Time) (*rtpaudio.Receiver, *rtpaudio.VirtualClock) {
	clock := rtpaudio.NewVirtualClock(t0)
	return rtpaudio.NewReceiver(clock, rtpaudio.Config{QueueCapacity: 64, ReorderWindow: 64}), clock
}

func checkStereoHeader(t *testing.T, wav []byte, samples int) {
	t.Helper()
	if len(wav) != 44+samples*4 {
		t.Fatalf("WAV length = %d, want %d", len(wav), 44+samples*4)
	}
	if string(wav[0:4]) != "RIFF" || string(wav[8:12]) != "WAVE" {
		t.Fatal("missing RIFF/WAVE magic")
	}
	if got := binary.LittleEndian.Uint32(wav[4:8]); got != uint32(36+samples*4) {
		t.Fatalf("RIFF size = %d, want %d", got, 36+samples*4)
	}
	if got := binary.LittleEndian.Uint16(wav[20:22]); got != 1 {
		t.Fatalf("format = %d, want PCM 1", got)
	}
	if got := binary.LittleEndian.Uint16(wav[22:24]); got != 2 {
		t.Fatalf("channels = %d, want 2", got)
	}
	if got := binary.LittleEndian.Uint32(wav[24:28]); got != rtpaudio.ClockRate {
		t.Fatalf("sample rate = %d, want %d", got, rtpaudio.ClockRate)
	}
	if got := binary.LittleEndian.Uint32(wav[28:32]); got != rtpaudio.ClockRate*4 {
		t.Fatalf("byte rate = %d, want %d", got, rtpaudio.ClockRate*4)
	}
	if got := binary.LittleEndian.Uint16(wav[32:34]); got != 4 {
		t.Fatalf("block align = %d, want 4", got)
	}
	if got := binary.LittleEndian.Uint16(wav[34:36]); got != 16 {
		t.Fatalf("bits per sample = %d, want 16", got)
	}
	if string(wav[36:40]) != "data" {
		t.Fatal("missing data chunk")
	}
	if got := binary.LittleEndian.Uint32(wav[40:44]); got != uint32(samples*4) {
		t.Fatalf("data size = %d, want %d", got, samples*4)
	}
}

func stereoSample(wav []byte, channel, p int) int16 {
	return int16(binary.LittleEndian.Uint16(wav[44+p*4+channel*2:]))
}

func TestAlignedExportPlacesFramesByEmissionTime(t *testing.T) {
	t0 := time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)
	r, clock := newAlignReceiver(t0)

	// Source B's first packet arrives 30 ms after source A's, so B's playout
	// grid starts 30 ms later. Audio must keep that offset in the export.
	feedPackets(t, r, alignKeyA, alignSSRCA, 0, 1000, 10, t0)
	feedPackets(t, r, alignKeyB, alignSSRCB, 100, 5000, 10, t0.Add(30*time.Millisecond))
	clock.Advance(240 * time.Millisecond)
	r.Pump()

	start := t0.Add(60 * time.Millisecond) // source A's first emitted frame
	plan := rtpaudio.AlignmentPlan{
		Channels: []rtpaudio.ChannelRef{
			{Source: alignKeyA, Generation: 1},
			{Source: alignKeyB, Generation: 1},
		},
		Start: start,
		End:   start.Add(160 * time.Millisecond),
	}
	out, err := r.Aligned(plan)
	if err != nil {
		t.Fatal(err)
	}
	const wantSamples = 1280 // 160 ms at 8 kHz
	if out.Samples != wantSamples {
		t.Fatalf("samples = %d, want %d", out.Samples, wantSamples)
	}
	checkStereoHeader(t, out.WAV, wantSamples)

	for p := 0; p < wantSamples; p++ {
		// Left: source A frames 0..7 back to back from output sample 0.
		wantA := expectedSample(uint64(p/rtpaudio.SamplesPerPacket), p%rtpaudio.SamplesPerPacket)
		if got := stereoSample(out.WAV, 0, p); got != wantA {
			t.Fatalf("left sample %d = %d, want %d", p, got, wantA)
		}
		// Right: 30 ms of silence, then source B frames 0..6; frame 6 is
		// clipped by the range end after 80 samples.
		var wantB int16
		if p >= 240 {
			rel := p - 240
			wantB = expectedSample(uint64(100+rel/rtpaudio.SamplesPerPacket), rel%rtpaudio.SamplesPerPacket)
		}
		if got := stereoSample(out.WAV, 1, p); got != wantB {
			t.Fatalf("right sample %d = %d, want %d", p, got, wantB)
		}
	}

	if len(out.Evidence) != 15 {
		t.Fatalf("evidence entries = %d, want 15", len(out.Evidence))
	}
	for k := 0; k < 8; k++ {
		want := rtpaudio.AlignedEvidence{
			Source: alignKeyA, Generation: 1, FrameIndex: uint64(k),
			SourceOffset: 0, OutputOffset: k * rtpaudio.SamplesPerPacket, Count: rtpaudio.SamplesPerPacket,
		}
		if out.Evidence[k] != want {
			t.Fatalf("evidence[%d] = %+v, want %+v", k, out.Evidence[k], want)
		}
	}
	for k := 0; k < 7; k++ {
		count := rtpaudio.SamplesPerPacket
		if k == 6 {
			count = 80 // clipped by the range end
		}
		want := rtpaudio.AlignedEvidence{
			Source: alignKeyB, Generation: 1, FrameIndex: uint64(k),
			SourceOffset: 0, OutputOffset: 240 + k*rtpaudio.SamplesPerPacket, Count: count,
		}
		if got := out.Evidence[8+k]; got != want {
			t.Fatalf("evidence[%d] = %+v, want %+v", 8+k, got, want)
		}
	}
}

func TestAlignedExportMissingFrameStaysSilentInPlace(t *testing.T) {
	t0 := time.Date(2026, 10, 6, 9, 30, 0, 0, time.UTC)
	r, clock := newAlignReceiver(t0)

	// Source A loses sequence 3; source B is complete.
	feedPackets(t, r, alignKeyA, alignSSRCA, 0, 1000, 3, t0)
	feedPackets(t, r, alignKeyA, alignSSRCA, 4, 1000+4*rtpaudio.SamplesPerPacket, 6, t0)
	feedPackets(t, r, alignKeyB, alignSSRCB, 100, 5000, 10, t0)
	clock.Advance(240 * time.Millisecond)
	r.Pump()

	start := t0.Add(60 * time.Millisecond)
	out, err := r.Aligned(rtpaudio.AlignmentPlan{
		Channels: []rtpaudio.ChannelRef{
			{Source: alignKeyA, Generation: 1},
			{Source: alignKeyB, Generation: 1},
		},
		Start: start,
		End:   start.Add(160 * time.Millisecond),
	})
	if err != nil {
		t.Fatal(err)
	}
	// The missing frame must occupy its own 20 ms slot: samples 480..639.
	for p := 480; p < 640; p++ {
		if got := stereoSample(out.WAV, 0, p); got != 0 {
			t.Fatalf("left sample %d in missing slot = %d, want silence", p, got)
		}
	}
	// Frames after the gap must not shift: frame 4 starts at sample 640.
	if got := stereoSample(out.WAV, 0, 640); got != expectedSample(4, 0) {
		t.Fatalf("left sample 640 = %d, want %d (frame 4 must not shift)", got, expectedSample(4, 0))
	}
	if got := stereoSample(out.WAV, 0, 479); got != expectedSample(2, 159) {
		t.Fatalf("left sample 479 = %d, want %d", got, expectedSample(2, 159))
	}
	var missingEntry *rtpaudio.AlignedEvidence
	for i := range out.Evidence {
		if out.Evidence[i].Source == alignKeyA && out.Evidence[i].FrameIndex == 3 {
			missingEntry = &out.Evidence[i]
		}
	}
	if missingEntry == nil {
		t.Fatal("no evidence entry for the missing frame")
	}
	want := rtpaudio.AlignedEvidence{
		Source: alignKeyA, Generation: 1, FrameIndex: 3, Missing: true,
		SourceOffset: 0, OutputOffset: 480, Count: rtpaudio.SamplesPerPacket,
	}
	if *missingEntry != want {
		t.Fatalf("missing evidence = %+v, want %+v", *missingEntry, want)
	}
}

func TestAlignedExportClipsFramesAtRangeBoundaries(t *testing.T) {
	t0 := time.Date(2026, 10, 6, 10, 0, 0, 0, time.UTC)
	r, clock := newAlignReceiver(t0)

	feedPackets(t, r, alignKeyA, alignSSRCA, 0, 1000, 10, t0)
	feedPackets(t, r, alignKeyB, alignSSRCB, 100, 5000, 10, t0.Add(30*time.Millisecond))
	clock.Advance(240 * time.Millisecond)
	r.Pump()

	// The range cuts 10 ms into A's first frame and ends 5 ms into a frame.
	start := t0.Add(70 * time.Millisecond)
	out, err := r.Aligned(rtpaudio.AlignmentPlan{
		Channels: []rtpaudio.ChannelRef{
			{Source: alignKeyA, Generation: 1},
			{Source: alignKeyB, Generation: 1},
		},
		Start: start,
		End:   start.Add(25 * time.Millisecond),
	})
	if err != nil {
		t.Fatal(err)
	}
	const wantSamples = 200 // 25 ms at 8 kHz
	if out.Samples != wantSamples {
		t.Fatalf("samples = %d, want %d", out.Samples, wantSamples)
	}
	checkStereoHeader(t, out.WAV, wantSamples)
	for p := 0; p < wantSamples; p++ {
		var wantA, wantB int16
		switch {
		case p < 80: // tail of A frame 0, source samples 80..159
			wantA = expectedSample(0, 80+p)
		default: // head of A frame 1
			wantA = expectedSample(1, p-80)
		}
		if p >= 160 { // head of B frame 0, clipped after 40 samples
			wantB = expectedSample(100, p-160)
		}
		if got := stereoSample(out.WAV, 0, p); got != wantA {
			t.Fatalf("left sample %d = %d, want %d", p, got, wantA)
		}
		if got := stereoSample(out.WAV, 1, p); got != wantB {
			t.Fatalf("right sample %d = %d, want %d", p, got, wantB)
		}
	}
	wantEvidence := []rtpaudio.AlignedEvidence{
		{Source: alignKeyA, Generation: 1, FrameIndex: 0, SourceOffset: 80, OutputOffset: 0, Count: 80},
		{Source: alignKeyA, Generation: 1, FrameIndex: 1, SourceOffset: 0, OutputOffset: 80, Count: 120},
		{Source: alignKeyB, Generation: 1, FrameIndex: 0, SourceOffset: 0, OutputOffset: 160, Count: 40},
	}
	if !reflect.DeepEqual(out.Evidence, wantEvidence) {
		t.Fatalf("evidence = %+v, want %+v", out.Evidence, wantEvidence)
	}
}

func TestAlignedExportOutsideRecordedRangeIsSilence(t *testing.T) {
	t0 := time.Date(2026, 10, 6, 10, 30, 0, 0, time.UTC)
	r, clock := newAlignReceiver(t0)

	feedPackets(t, r, alignKeyA, alignSSRCA, 0, 1000, 5, t0)
	feedPackets(t, r, alignKeyB, alignSSRCB, 100, 5000, 5, t0)
	clock.Advance(160 * time.Millisecond)
	r.Pump()

	// The whole range starts an hour after anything was recorded.
	start := t0.Add(time.Hour)
	out, err := r.Aligned(rtpaudio.AlignmentPlan{
		Channels: []rtpaudio.ChannelRef{
			{Source: alignKeyA, Generation: 1},
			{Source: alignKeyB, Generation: 1},
		},
		Start: start,
		End:   start.Add(20 * time.Millisecond),
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Samples != 160 {
		t.Fatalf("samples = %d, want 160", out.Samples)
	}
	if len(out.Evidence) != 0 {
		t.Fatalf("evidence = %+v, want none outside recorded output", out.Evidence)
	}
	for i := 44; i < len(out.WAV); i++ {
		if out.WAV[i] != 0 {
			t.Fatalf("WAV byte %d = %d, want silence outside recorded output", i, out.WAV[i])
		}
	}
}

func TestAlignedExportUsesSelectedGenerationAfterRestart(t *testing.T) {
	t0 := time.Date(2026, 10, 6, 11, 0, 0, 0, time.UTC)
	r, clock := newAlignReceiver(t0)

	// Generation 1 of A: sequences 0..4 starting at t0.
	feedPackets(t, r, alignKeyA, alignSSRCA, 0, 1000, 5, t0)
	// Source B runs one long generation.
	feedPackets(t, r, alignKeyB, alignSSRCB, 100, 5000, 15, t0)
	clock.Advance(160 * time.Millisecond)
	r.Pump()
	gen, err := r.RestartSource(alignKeyA, clock.Now())
	if err != nil || gen != 2 {
		t.Fatalf("restart = generation %d, %v", gen, err)
	}
	// Generation 2 of A: sequences 200..4 starting at t0+160ms.
	feedPackets(t, r, alignKeyA, alignSSRCA, 200, 32000, 5, t0.Add(160*time.Millisecond))
	clock.Advance(160 * time.Millisecond)
	r.Pump()

	// Selecting the old generation must yield the old audio, not the new one.
	start1 := t0.Add(60 * time.Millisecond)
	out, err := r.Aligned(rtpaudio.AlignmentPlan{
		Channels: []rtpaudio.ChannelRef{
			{Source: alignKeyA, Generation: 1},
			{Source: alignKeyB, Generation: 1},
		},
		Start: start1,
		End:   start1.Add(100 * time.Millisecond),
	})
	if err != nil {
		t.Fatal(err)
	}
	for p := 0; p < out.Samples; p++ {
		frame := p / rtpaudio.SamplesPerPacket
		if got, want := stereoSample(out.WAV, 0, p), expectedSample(uint64(frame), p%rtpaudio.SamplesPerPacket); got != want {
			t.Fatalf("old-generation left sample %d = %d, want %d (new generation leaked into export)", p, got, want)
		}
		if got, want := stereoSample(out.WAV, 1, p), expectedSample(uint64(100+frame), p%rtpaudio.SamplesPerPacket); got != want {
			t.Fatalf("right sample %d = %d, want %d", p, got, want)
		}
	}
	for _, ev := range out.Evidence {
		if ev.Source == alignKeyA && ev.Generation != 1 {
			t.Fatalf("evidence labels old-generation export as generation %d", ev.Generation)
		}
	}

	// Selecting the new generation places its audio at its own output times.
	start2 := t0.Add(220 * time.Millisecond)
	out, err = r.Aligned(rtpaudio.AlignmentPlan{
		Channels: []rtpaudio.ChannelRef{
			{Source: alignKeyA, Generation: 2},
			{Source: alignKeyB, Generation: 1},
		},
		Start: start2,
		End:   start2.Add(100 * time.Millisecond),
	})
	if err != nil {
		t.Fatal(err)
	}
	for p := 0; p < out.Samples; p++ {
		frame := p / rtpaudio.SamplesPerPacket
		if got, want := stereoSample(out.WAV, 0, p), expectedSample(uint64(200+frame), p%rtpaudio.SamplesPerPacket); got != want {
			t.Fatalf("new-generation left sample %d = %d, want %d", p, got, want)
		}
		if got, want := stereoSample(out.WAV, 1, p), expectedSample(uint64(108+frame), p%rtpaudio.SamplesPerPacket); got != want {
			t.Fatalf("right sample %d = %d, want %d", p, got, want)
		}
	}

	// Both generations of one source may be compared directly.
	out, err = r.Aligned(rtpaudio.AlignmentPlan{
		Channels: []rtpaudio.ChannelRef{
			{Source: alignKeyA, Generation: 1},
			{Source: alignKeyA, Generation: 2},
		},
		Start: start2,
		End:   start2.Add(100 * time.Millisecond),
	})
	if err != nil {
		t.Fatal(err)
	}
	for p := 0; p < out.Samples; p++ {
		if got := stereoSample(out.WAV, 0, p); got != 0 {
			t.Fatalf("left sample %d = %d, want silence (generation 1 ended before the range)", p, got)
		}
		frame := p / rtpaudio.SamplesPerPacket
		if got, want := stereoSample(out.WAV, 1, p), expectedSample(uint64(200+frame), p%rtpaudio.SamplesPerPacket); got != want {
			t.Fatalf("right sample %d = %d, want %d", p, got, want)
		}
	}
}

func TestAlignedExportIgnoresLatePacketRewrites(t *testing.T) {
	t0 := time.Date(2026, 10, 6, 11, 30, 0, 0, time.UTC)
	r, clock := newAlignReceiver(t0)

	feedPackets(t, r, alignKeyA, alignSSRCA, 0, 1000, 5, t0)
	feedPackets(t, r, alignKeyB, alignSSRCB, 100, 5000, 5, t0)
	clock.Advance(160 * time.Millisecond)
	r.Pump()

	// A late copy of sequence 0 with corrupted payload arrives after its
	// frame was emitted. It is evidence only and must not rewrite output.
	late := makeRTP(0, 1000, alignSSRCA)
	for i := rtpaudio.FixedHeaderSize; i < len(late); i++ {
		late[i] = 0x7F
	}
	r.HandlePacket(alignKeyA, late, clock.Now())
	r.Pump()
	info, err := r.Info(alignKeyA, 1)
	if err != nil {
		t.Fatal(err)
	}
	if info.LatePackets != 1 {
		t.Fatalf("late packets = %d, want 1", info.LatePackets)
	}

	start := t0.Add(60 * time.Millisecond)
	out, err := r.Aligned(rtpaudio.AlignmentPlan{
		Channels: []rtpaudio.ChannelRef{
			{Source: alignKeyA, Generation: 1},
			{Source: alignKeyB, Generation: 1},
		},
		Start: start,
		End:   start.Add(100 * time.Millisecond),
	})
	if err != nil {
		t.Fatal(err)
	}
	for p := 0; p < out.Samples; p++ {
		frame := p / rtpaudio.SamplesPerPacket
		if got, want := stereoSample(out.WAV, 0, p), expectedSample(uint64(frame), p%rtpaudio.SamplesPerPacket); got != want {
			t.Fatalf("left sample %d = %d, want %d (late packet rewrote emitted audio)", p, got, want)
		}
	}
}

func TestAlignedExportRejectsInvalidPlans(t *testing.T) {
	t0 := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	r, clock := newAlignReceiver(t0)
	feedPackets(t, r, alignKeyA, alignSSRCA, 0, 1000, 4, t0)
	feedPackets(t, r, alignKeyB, alignSSRCB, 100, 5000, 4, t0)
	clock.Advance(120 * time.Millisecond)
	r.Pump()

	start := t0.Add(60 * time.Millisecond)
	refA := rtpaudio.ChannelRef{Source: alignKeyA, Generation: 1}
	cases := []struct {
		name string
		plan rtpaudio.AlignmentPlan
	}{
		{"one channel", rtpaudio.AlignmentPlan{
			Channels: []rtpaudio.ChannelRef{refA},
			Start:    start, End: start.Add(20 * time.Millisecond),
		}},
		{"three channels", rtpaudio.AlignmentPlan{
			Channels: []rtpaudio.ChannelRef{refA, refA, refA},
			Start:    start, End: start.Add(20 * time.Millisecond),
		}},
		{"zero generation", rtpaudio.AlignmentPlan{
			Channels: []rtpaudio.ChannelRef{{Source: alignKeyA}, refA},
			Start:    start, End: start.Add(20 * time.Millisecond),
		}},
		{"duplicate channels", rtpaudio.AlignmentPlan{
			Channels: []rtpaudio.ChannelRef{refA, refA},
			Start:    start, End: start.Add(20 * time.Millisecond),
		}},
		{"empty range", rtpaudio.AlignmentPlan{
			Channels: []rtpaudio.ChannelRef{refA, {Source: alignKeyB, Generation: 1}},
			Start:    start, End: start,
		}},
		{"inverted range", rtpaudio.AlignmentPlan{
			Channels: []rtpaudio.ChannelRef{refA, {Source: alignKeyB, Generation: 1}},
			Start:    start, End: start.Add(-20 * time.Millisecond),
		}},
		{"range too long", rtpaudio.AlignmentPlan{
			Channels: []rtpaudio.ChannelRef{refA, {Source: alignKeyB, Generation: 1}},
			Start:    start, End: start.Add(10*time.Second + 125*time.Microsecond),
		}},
		{"range off sample grid", rtpaudio.AlignmentPlan{
			Channels: []rtpaudio.ChannelRef{refA, {Source: alignKeyB, Generation: 1}},
			Start:    start, End: start.Add(20*time.Millisecond + time.Microsecond),
		}},
		{"sub-sample range", rtpaudio.AlignmentPlan{
			Channels: []rtpaudio.ChannelRef{refA, {Source: alignKeyB, Generation: 1}},
			Start:    start, End: start.Add(100 * time.Microsecond),
		}},
		{"unknown source", rtpaudio.AlignmentPlan{
			Channels: []rtpaudio.ChannelRef{{Source: "192.0.2.99:5000", Generation: 1}, refA},
			Start:    start, End: start.Add(20 * time.Millisecond),
		}},
		{"unknown generation", rtpaudio.AlignmentPlan{
			Channels: []rtpaudio.ChannelRef{{Source: alignKeyA, Generation: 7}, refA},
			Start:    start, End: start.Add(20 * time.Millisecond),
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := r.Aligned(tc.plan); err == nil {
				t.Fatal("invalid plan was accepted")
			}
		})
	}

	// Boundary spans of exactly one sample and exactly ten seconds are valid.
	refB := rtpaudio.ChannelRef{Source: alignKeyB, Generation: 1}
	for _, span := range []time.Duration{125 * time.Microsecond, 10 * time.Second} {
		plan := rtpaudio.AlignmentPlan{
			Channels: []rtpaudio.ChannelRef{refA, refB},
			Start:    start, End: start.Add(span),
		}
		out, err := r.Aligned(plan)
		if err != nil {
			t.Fatalf("span %v rejected: %v", span, err)
		}
		if out.Samples != int(span/(time.Second/rtpaudio.ClockRate)) {
			t.Fatalf("span %v: samples = %d", span, out.Samples)
		}
	}
}

func TestAlignedExportRejectsOffGridFrameTimes(t *testing.T) {
	t0 := time.Date(2026, 10, 6, 12, 30, 0, 0, time.UTC)
	r, clock := newAlignReceiver(t0)

	// Source A's playout grid is offset from whole 8 kHz sample boundaries.
	feedPackets(t, r, alignKeyA, alignSSRCA, 0, 1000, 5, t0.Add(40*time.Microsecond))
	feedPackets(t, r, alignKeyB, alignSSRCB, 100, 5000, 5, t0)
	clock.Advance(200 * time.Millisecond)
	r.Pump()

	start := t0.Add(60 * time.Millisecond)
	_, err := r.Aligned(rtpaudio.AlignmentPlan{
		Channels: []rtpaudio.ChannelRef{
			{Source: alignKeyA, Generation: 1},
			{Source: alignKeyB, Generation: 1},
		},
		Start: start,
		End:   start.Add(100 * time.Millisecond),
	})
	if err == nil {
		t.Fatal("export with frame times off the sample grid was accepted")
	}
}

func TestExportAlignedWritesWAVAndEvidence(t *testing.T) {
	t0 := time.Date(2026, 10, 6, 13, 0, 0, 0, time.UTC)
	r, clock := newAlignReceiver(t0)
	feedPackets(t, r, alignKeyA, alignSSRCA, 0, 1000, 5, t0)
	feedPackets(t, r, alignKeyB, alignSSRCB, 100, 5000, 5, t0)
	clock.Advance(160 * time.Millisecond)
	r.Pump()

	start := t0.Add(60 * time.Millisecond)
	plan := rtpaudio.AlignmentPlan{
		Channels: []rtpaudio.ChannelRef{
			{Source: alignKeyA, Generation: 1},
			{Source: alignKeyB, Generation: 1},
		},
		Start: start,
		End:   start.Add(100 * time.Millisecond),
	}
	dir := t.TempDir()
	wavPath, evidencePath, err := r.ExportAligned(dir, plan)
	if err != nil {
		t.Fatal(err)
	}
	if wavPath != filepath.Join(dir, "aligned.wav") || evidencePath != filepath.Join(dir, "aligned.json") {
		t.Fatalf("paths = %q, %q", wavPath, evidencePath)
	}
	want, err := r.Aligned(plan)
	if err != nil {
		t.Fatal(err)
	}
	wav, err := os.ReadFile(wavPath)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(wav, want.WAV) {
		t.Fatal("aligned.wav does not match the in-memory export")
	}
	data, err := os.ReadFile(evidencePath)
	if err != nil {
		t.Fatal(err)
	}
	var evidence []rtpaudio.AlignedEvidence
	if err := json.Unmarshal(data, &evidence); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(evidence, want.Evidence) {
		t.Fatalf("aligned.json = %+v, want %+v", evidence, want.Evidence)
	}
	for _, ev := range evidence {
		if ev.Generation != 1 {
			t.Fatalf("evidence generation = %d, want 1", ev.Generation)
		}
	}
}
