package rtpaudio_test

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"rtpaudio"
)

func submitAlignedPacket(t *testing.T, r *rtpaudio.Receiver, key rtpaudio.SourceKey, seq int, at time.Time) {
	t.Helper()
	pkt := make([]byte, rtpaudio.FixedHeaderSize+rtpaudio.SamplesPerPacket*2)
	pkt[0] = 2 << 6
	pkt[1] = rtpaudio.PayloadType
	binary.BigEndian.PutUint16(pkt[2:4], uint16(seq))
	binary.BigEndian.PutUint32(pkt[4:8], uint32(seq*rtpaudio.SamplesPerPacket))
	binary.BigEndian.PutUint32(pkt[8:12], testSSRC)
	for i := 0; i < rtpaudio.SamplesPerPacket; i++ {
		binary.BigEndian.PutUint16(pkt[rtpaudio.FixedHeaderSize+i*2:], uint16(seq*1000+i))
	}
	status := r.HandlePacket(key, pkt, at)
	if !status.Accepted {
		t.Fatalf("packet %d rejected: %s: %v", seq, status.Reason, status.Err)
	}
}

func alignedSampleValue(seq, i int) int16 {
	return int16(seq*1000 + i)
}

func alignedWAVSample(wav []byte, sample, channel int) int16 {
	offset := 44 + sample*4 + channel*2
	return int16(binary.LittleEndian.Uint16(wav[offset:]))
}

func prepareAlignedReceiver(t *testing.T) (*rtpaudio.Receiver, time.Time, rtpaudio.SourceKey, rtpaudio.SourceKey) {
	t.Helper()
	start := time.Date(2026, 10, 6, 10, 0, 0, 0, time.UTC)
	clock := rtpaudio.NewVirtualClock(start)
	r := rtpaudio.NewReceiver(clock, rtpaudio.Config{QueueCapacity: 64, ReorderWindow: 64})
	left := rtpaudio.SourceKey("192.0.2.10:5000")
	right := rtpaudio.SourceKey("192.0.2.11:5000")

	for seq := 0; seq < 3; seq++ {
		submitAlignedPacket(t, r, left, seq, start)
	}
	rightStart := start.Add(40 * time.Millisecond)
	for seq := 0; seq < 3; seq++ {
		submitAlignedPacket(t, r, right, seq, rightStart)
	}
	clock.Advance(100 * time.Millisecond)
	r.Pump()

	if _, err := r.RestartSource(left, clock.Now()); err != nil {
		t.Fatal(err)
	}
	submitAlignedPacket(t, r, left, 100, clock.Now())
	clock.Advance(100 * time.Millisecond)
	r.Pump()

	return r, start, left, right
}

func TestAlignedUsesEmittedTimesAndSelectedGenerations(t *testing.T) {
	r, start, left, right := prepareAlignedReceiver(t)
	plan := rtpaudio.AlignmentPlan{
		Channels: []rtpaudio.ChannelRef{
			{Source: left, Generation: 1},
			{Source: right, Generation: 1},
		},
		Start: start,
		End:   start.Add(260 * time.Millisecond),
	}
	output, err := r.Aligned(plan)
	if err != nil {
		t.Fatal(err)
	}
	if output.Samples != 2080 || len(output.WAV) != 44+2080*4 {
		t.Fatalf("output size = samples %d, bytes %d", output.Samples, len(output.WAV))
	}
	if got := binary.LittleEndian.Uint16(output.WAV[22:24]); got != 2 {
		t.Fatalf("WAV channels = %d, want 2", got)
	}
	if got := binary.LittleEndian.Uint32(output.WAV[24:28]); got != rtpaudio.ClockRate {
		t.Fatalf("WAV rate = %d", got)
	}
	if got := binary.LittleEndian.Uint16(output.WAV[32:34]); got != 4 {
		t.Fatalf("WAV block align = %d, want 4", got)
	}

	if got := alignedWAVSample(output.WAV, 0, 0); got != 0 {
		t.Fatalf("left sample before first output = %d, want silence", got)
	}
	if got := alignedWAVSample(output.WAV, 480, 0); got != alignedSampleValue(0, 0) {
		t.Fatalf("left first frame sample = %d", got)
	}
	if got := alignedWAVSample(output.WAV, 960, 0); got != 0 {
		t.Fatalf("left sample after old generation = %d, want silence", got)
	}
	if got := alignedWAVSample(output.WAV, 1280, 0); got != 0 {
		t.Fatalf("selected old generation contained new-generation audio at sample 1280: %d", got)
	}
	if got := alignedWAVSample(output.WAV, 800, 1); got != alignedSampleValue(0, 0) {
		t.Fatalf("right first sample = %d", got)
	}
	if got := alignedWAVSample(output.WAV, 799, 1); got != 0 {
		t.Fatalf("right sample before first output = %d, want silence", got)
	}
	if got := alignedWAVSample(output.WAV, 1280, 1); got != 0 {
		t.Fatalf("right sample outside recorded output = %d, want silence", got)
	}

	want := []rtpaudio.AlignedEvidence{
		{Source: left, Generation: 1, FrameIndex: 0, SourceOffset: 0, OutputOffset: 480, Count: 160},
		{Source: left, Generation: 1, FrameIndex: 1, SourceOffset: 0, OutputOffset: 640, Count: 160},
		{Source: left, Generation: 1, FrameIndex: 2, SourceOffset: 0, OutputOffset: 800, Count: 160},
		{Source: right, Generation: 1, FrameIndex: 0, SourceOffset: 0, OutputOffset: 800, Count: 160},
		{Source: right, Generation: 1, FrameIndex: 1, SourceOffset: 0, OutputOffset: 960, Count: 160},
		{Source: right, Generation: 1, FrameIndex: 2, SourceOffset: 0, OutputOffset: 1120, Count: 160},
		{Source: right, Generation: 1, FrameIndex: 3, Missing: true, SourceOffset: 0, OutputOffset: 1280, Count: 160},
		{Source: right, Generation: 1, FrameIndex: 4, Missing: true, SourceOffset: 0, OutputOffset: 1440, Count: 160},
		{Source: right, Generation: 1, FrameIndex: 5, Missing: true, SourceOffset: 0, OutputOffset: 1600, Count: 160},
	}
	if len(output.Evidence) != len(want) {
		t.Fatalf("evidence = %+v, want %+v", output.Evidence, want)
	}
	for i, got := range output.Evidence {
		if got != want[i] {
			t.Fatalf("evidence %d = %+v, want %+v", i, got, want[i])
		}
	}
}

func TestAlignedClipsAtSampleBoundaries(t *testing.T) {
	r, start, left, right := prepareAlignedReceiver(t)
	plan := rtpaudio.AlignmentPlan{
		Channels: []rtpaudio.ChannelRef{
			{Source: left, Generation: 1},
			{Source: right, Generation: 1},
		},
		Start: start.Add(90 * time.Millisecond),
		End:   start.Add(130 * time.Millisecond),
	}
	output, err := r.Aligned(plan)
	if err != nil {
		t.Fatal(err)
	}
	if output.Samples != 320 {
		t.Fatalf("samples = %d, want 320", output.Samples)
	}

	if got := alignedWAVSample(output.WAV, 0, 0); got != alignedSampleValue(1, 80) {
		t.Fatalf("left start-clipped sample = %d", got)
	}
	if got := alignedWAVSample(output.WAV, 79, 0); got != alignedSampleValue(1, 159) {
		t.Fatalf("left clipped frame end = %d", got)
	}
	if got := alignedWAVSample(output.WAV, 80, 0); got != alignedSampleValue(2, 0) {
		t.Fatalf("left next frame sample = %d", got)
	}
	if got := alignedWAVSample(output.WAV, 319, 0); got != 0 {
		t.Fatalf("left unrecorded sample = %d, want silence", got)
	}
	if got := alignedWAVSample(output.WAV, 0, 1); got != 0 {
		t.Fatalf("right sample before first output = %d, want silence", got)
	}
	if got := alignedWAVSample(output.WAV, 80, 1); got != alignedSampleValue(0, 0) {
		t.Fatalf("right first frame sample = %d", got)
	}
	if got := alignedWAVSample(output.WAV, 319, 1); got != alignedSampleValue(1, 79) {
		t.Fatalf("right end-clipped sample = %d", got)
	}

	want := []rtpaudio.AlignedEvidence{
		{Source: left, Generation: 1, FrameIndex: 1, SourceOffset: 80, OutputOffset: 0, Count: 80},
		{Source: left, Generation: 1, FrameIndex: 2, SourceOffset: 0, OutputOffset: 80, Count: 160},
		{Source: right, Generation: 1, FrameIndex: 0, SourceOffset: 0, OutputOffset: 80, Count: 160},
		{Source: right, Generation: 1, FrameIndex: 1, SourceOffset: 0, OutputOffset: 240, Count: 80},
	}
	if len(output.Evidence) != len(want) {
		t.Fatalf("evidence = %+v, want %+v", output.Evidence, want)
	}
	for i, got := range output.Evidence {
		if got != want[i] {
			t.Fatalf("evidence %d = %+v, want %+v", i, got, want[i])
		}
	}
}

func TestAlignedEmittedMissingFramesAreSilent(t *testing.T) {
	start := time.Date(2026, 10, 6, 11, 0, 0, 0, time.UTC)
	clock := rtpaudio.NewVirtualClock(start)
	r := rtpaudio.NewReceiver(clock, rtpaudio.Config{QueueCapacity: 32, ReorderWindow: 32})
	left := rtpaudio.SourceKey("192.0.2.20:5000")
	right := rtpaudio.SourceKey("192.0.2.21:5000")

	submitAlignedPacket(t, r, left, 0, start)
	submitAlignedPacket(t, r, left, 1, start)
	submitAlignedPacket(t, r, right, 0, start)
	clock.Advance(160 * time.Millisecond)
	r.Pump()

	plan := rtpaudio.AlignmentPlan{
		Channels: []rtpaudio.ChannelRef{
			{Source: left, Generation: 1},
			{Source: right, Generation: 1},
		},
		Start: start.Add(90 * time.Millisecond),
		End:   start.Add(130 * time.Millisecond),
	}
	output, err := r.Aligned(plan)
	if err != nil {
		t.Fatal(err)
	}
	if got := alignedWAVSample(output.WAV, 0, 1); got != 0 {
		t.Fatalf("missing frame sample = %d, want silence", got)
	}
	want := rtpaudio.AlignedEvidence{
		Source: right, Generation: 1, FrameIndex: 1, Missing: true,
		SourceOffset: 80, OutputOffset: 0, Count: 80,
	}
	var found bool
	for _, evidence := range output.Evidence {
		if evidence == want {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing evidence not found in %+v", output.Evidence)
	}
}

func TestAlignedRejectsInvalidPlansAndReferences(t *testing.T) {
	r, start, left, right := prepareAlignedReceiver(t)
	valid := []rtpaudio.ChannelRef{
		{Source: left, Generation: 1},
		{Source: right, Generation: 1},
	}
	tests := []struct {
		name string
		plan rtpaudio.AlignmentPlan
	}{
		{"one channel", rtpaudio.AlignmentPlan{Channels: valid[:1], Start: start, End: start.Add(20 * time.Millisecond)}},
		{"three channels", rtpaudio.AlignmentPlan{Channels: append(append([]rtpaudio.ChannelRef(nil), valid...), valid[0]), Start: start, End: start.Add(20 * time.Millisecond)}},
		{"duplicate channel", rtpaudio.AlignmentPlan{Channels: []rtpaudio.ChannelRef{valid[0], valid[0]}, Start: start, End: start.Add(20 * time.Millisecond)}},
		{"same source generations", rtpaudio.AlignmentPlan{Channels: []rtpaudio.ChannelRef{valid[0], {Source: left, Generation: 2}}, Start: start, End: start.Add(20 * time.Millisecond)}},
		{"zero generation", rtpaudio.AlignmentPlan{Channels: []rtpaudio.ChannelRef{{Source: left}, valid[1]}, Start: start, End: start.Add(20 * time.Millisecond)}},
		{"unknown generation", rtpaudio.AlignmentPlan{Channels: []rtpaudio.ChannelRef{{Source: left, Generation: 99}, valid[1]}, Start: start, End: start.Add(20 * time.Millisecond)}},
		{"unknown source", rtpaudio.AlignmentPlan{Channels: []rtpaudio.ChannelRef{{Source: "missing", Generation: 1}, valid[1]}, Start: start, End: start.Add(20 * time.Millisecond)}},
		{"empty range", rtpaudio.AlignmentPlan{Channels: valid, Start: start, End: start}},
		{"negative range", rtpaudio.AlignmentPlan{Channels: valid, Start: start.Add(time.Millisecond), End: start}},
		{"non-sample range", rtpaudio.AlignmentPlan{Channels: valid, Start: start, End: start.Add(40*time.Millisecond + time.Microsecond)}},
		{"too long", rtpaudio.AlignmentPlan{Channels: valid, Start: start, End: start.Add(10*time.Second + time.Millisecond)}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if out, err := r.Aligned(tc.plan); err == nil {
				t.Fatalf("invalid plan succeeded: %+v", out)
			}
		})
	}
}

func TestExportAlignedWritesStereoAndJSONEvidence(t *testing.T) {
	r, start, left, right := prepareAlignedReceiver(t)
	dir := t.TempDir()
	plan := rtpaudio.AlignmentPlan{
		Channels: []rtpaudio.ChannelRef{
			{Source: left, Generation: 1},
			{Source: right, Generation: 1},
		},
		Start: start.Add(90 * time.Millisecond),
		End:   start.Add(130 * time.Millisecond),
	}
	wavPath, evidencePath, err := r.ExportAligned(dir, plan)
	if err != nil {
		t.Fatal(err)
	}
	wav, err := os.ReadFile(wavPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(wav) != 44+320*4 || string(wav[8:12]) != "WAVE" {
		t.Fatalf("bad aligned WAV size/header: %d", len(wav))
	}
	data, err := os.ReadFile(evidencePath)
	if err != nil {
		t.Fatal(err)
	}
	var evidence []rtpaudio.AlignedEvidence
	if err := json.Unmarshal(data, &evidence); err != nil {
		t.Fatal(err)
	}
	if len(evidence) != 4 || evidence[0].Generation != 1 || evidence[2].Generation != 1 {
		t.Fatalf("aligned evidence = %+v", evidence)
	}
}

func TestAlignedErrorsAreSentinels(t *testing.T) {
	r, start, _, _ := prepareAlignedReceiver(t)
	_, err := r.Aligned(rtpaudio.AlignmentPlan{Start: start, End: start.Add(time.Millisecond)})
	if !errors.Is(err, rtpaudio.ErrInvalidAlignment) {
		t.Fatalf("error %v is not ErrInvalidAlignment", err)
	}
}
