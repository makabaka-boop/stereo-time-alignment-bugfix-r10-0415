package rtpaudio

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
	"time"
)

type ChannelRef struct {
	Source     SourceKey `json:"source"`
	Generation uint64    `json:"generation"`
}
type AlignmentPlan struct {
	Channels []ChannelRef `json:"channels"`
	Start    time.Time    `json:"start"`
	End      time.Time    `json:"end"`
}

// AlignedEvidence describes the source-frame samples used for one contiguous
// piece of one output channel. SourceOffset and OutputOffset are sample
// positions inside the source frame and stereo output, respectively; Count is
// the number of samples copied from that frame.
type AlignedEvidence struct {
	Source       SourceKey `json:"source"`
	Generation   uint64    `json:"generation"`
	FrameIndex   uint64    `json:"frame_index"`
	Missing      bool      `json:"missing"`
	SourceOffset int       `json:"source_offset"`
	OutputOffset int       `json:"output_offset"`
	Count        int       `json:"count"`
}
type AlignedOutput struct {
	WAV      []byte
	Evidence []AlignedEvidence
	Samples  int
}

const (
	sampleDuration     = time.Second / ClockRate
	maxAlignedDuration = 10 * time.Second
)

var (
	ErrInvalidAlignment   = errors.New("invalid aligned export plan")
	ErrOverlappingRecords = errors.New("overlapping output records")
)

type alignedSnapshot struct {
	ref    ChannelRef
	frames []OutputFrame
}

func alignedHeader(samples int) []byte {
	b := make([]byte, 44+samples*4)
	copy(b, "RIFF")
	binary.LittleEndian.PutUint32(b[4:], uint32(len(b)-8))
	copy(b[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(b[16:], 16)
	binary.LittleEndian.PutUint16(b[20:], 1)
	binary.LittleEndian.PutUint16(b[22:], 2)
	binary.LittleEndian.PutUint32(b[24:], ClockRate)
	binary.LittleEndian.PutUint32(b[28:], ClockRate*4)
	binary.LittleEndian.PutUint16(b[32:], 4)
	binary.LittleEndian.PutUint16(b[34:], 16)
	copy(b[36:], "data")
	binary.LittleEndian.PutUint32(b[40:], uint32(samples*4))
	return b
}

// Aligned renders exactly two channels from immutable output records. Both
// channels are snapshotted under the receiver lock before any output byte is
// produced, so an invalid plan or malformed record history rejects the entire
// export and late packets cannot alter selected generations.
func (r *Receiver) Aligned(plan AlignmentPlan) (AlignedOutput, error) {
	_, samples, err := validateAlignmentPlan(plan)
	if err != nil {
		return AlignedOutput{}, err
	}

	snapshots, err := r.alignedSnapshots(plan.Channels)
	if err != nil {
		return AlignedOutput{}, err
	}

	out := AlignedOutput{WAV: alignedHeader(samples), Samples: samples, Evidence: []AlignedEvidence{}}
	for channel, snapshot := range snapshots {
		records, err := alignedFrameIntervals(plan.Start, snapshot.frames)
		if err != nil {
			return AlignedOutput{}, fmt.Errorf("source %q generation %d: %w",
				snapshot.ref.Source, snapshot.ref.Generation, err)
		}

		for _, interval := range records {
			sourceOffset, outputOffset, count, ok := interval.slice(samples)
			if !ok {
				continue
			}
			frame := interval.frame
			if !frame.Missing {
				for j := 0; j < count; j++ {
					pos := 44 + (outputOffset+j)*4 + channel*2
					binary.LittleEndian.PutUint16(out.WAV[pos:], uint16(frame.Samples[sourceOffset+j]))
				}
			}
			out.Evidence = append(out.Evidence, AlignedEvidence{
				Source:       snapshot.ref.Source,
				Generation:   snapshot.ref.Generation,
				FrameIndex:   frame.Index,
				Missing:      frame.Missing,
				SourceOffset: sourceOffset,
				OutputOffset: outputOffset,
				Count:        count,
			})
		}
	}
	return out, nil
}

func validateAlignmentPlan(plan AlignmentPlan) (time.Duration, int, error) {
	if len(plan.Channels) != 2 {
		return 0, 0, fmt.Errorf("%w: exactly two channels are required", ErrInvalidAlignment)
	}
	if plan.Channels[0].Source == plan.Channels[1].Source {
		return 0, 0, fmt.Errorf("%w: both channels select source %q",
			ErrInvalidAlignment, plan.Channels[0].Source)
	}
	for _, channel := range plan.Channels {
		if channel.Generation == 0 {
			return 0, 0, fmt.Errorf("%w: source %q has generation 0", ErrInvalidAlignment, channel.Source)
		}
	}

	duration := plan.End.Sub(plan.Start)
	if duration <= 0 {
		return 0, 0, fmt.Errorf("%w: export range must be positive", ErrInvalidAlignment)
	}
	if duration > maxAlignedDuration {
		return 0, 0, fmt.Errorf("%w: range %s exceeds %s", ErrInvalidAlignment, duration, maxAlignedDuration)
	}
	if duration%sampleDuration != 0 {
		return 0, 0, fmt.Errorf("%w: range %s is not an integer number of %d Hz samples",
			ErrInvalidAlignment, duration, ClockRate)
	}
	return duration, int(duration / sampleDuration), nil
}

func (r *Receiver) alignedSnapshots(refs []ChannelRef) ([]alignedSnapshot, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	snapshots := make([]alignedSnapshot, len(refs))
	for i, ref := range refs {
		s, err := r.requireSource(ref.Source)
		if err != nil {
			return nil, err
		}
		g, err := generationOf(s, ref.Generation)
		if err != nil {
			return nil, err
		}
		frames := make([]OutputFrame, len(g.frames))
		copy(frames, g.frames)
		snapshots[i] = alignedSnapshot{ref: ref, frames: frames}
	}
	return snapshots, nil
}

type alignedFrameInterval struct {
	frame       OutputFrame
	startSample int
}

func alignedFrameIntervals(start time.Time, frames []OutputFrame) ([]alignedFrameInterval, error) {
	intervals := make([]alignedFrameInterval, len(frames))
	seenIndexes := make(map[uint64]struct{}, len(frames))
	for i, frame := range frames {
		offset, aligned := alignedSampleOffset(start, frame.At)
		if !aligned {
			return nil, fmt.Errorf("%w: frame %d output time %s is not on the sample grid",
				ErrInvalidAlignment, frame.Index, frame.At)
		}
		if _, exists := seenIndexes[frame.Index]; exists {
			return nil, fmt.Errorf("%w: frame index %d appears more than once",
				ErrOverlappingRecords, frame.Index)
		}
		seenIndexes[frame.Index] = struct{}{}
		intervals[i] = alignedFrameInterval{frame: frame, startSample: offset}
	}

	sort.Slice(intervals, func(i, j int) bool {
		if intervals[i].frame.At.Equal(intervals[j].frame.At) {
			return intervals[i].frame.Index < intervals[j].frame.Index
		}
		return intervals[i].frame.At.Before(intervals[j].frame.At)
	})
	for i := 1; i < len(intervals); i++ {
		previousEnd := intervals[i-1].frame.At.Add(FrameDuration)
		if intervals[i].frame.At.Before(previousEnd) {
			return nil, fmt.Errorf("%w: frames %d and %d overlap",
				ErrOverlappingRecords, intervals[i-1].frame.Index, intervals[i].frame.Index)
		}
	}
	return intervals, nil
}

// alignedSampleOffset uses wall-clock nanoseconds so a monotonic clock
// reading on one value cannot alter the exported time grid.
func alignedSampleOffset(start, at time.Time) (int, bool) {
	delta := at.UnixNano() - start.UnixNano()
	if delta%int64(sampleDuration) != 0 {
		return 0, false
	}
	return int(delta / int64(sampleDuration)), true
}

func (i alignedFrameInterval) slice(outputSamples int) (sourceOffset, outputOffset, count int, ok bool) {
	frameStart := i.startSample
	frameEnd := frameStart + SamplesPerPacket
	if frameEnd <= 0 || frameStart >= outputSamples {
		return 0, 0, 0, false
	}

	if frameStart < 0 {
		sourceOffset = -frameStart
	} else {
		outputOffset = frameStart
	}
	end := frameEnd
	if end > outputSamples {
		end = outputSamples
	}
	count = end - outputOffset
	return sourceOffset, outputOffset, count, count > 0
}
