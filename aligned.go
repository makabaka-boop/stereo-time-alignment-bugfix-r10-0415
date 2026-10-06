package rtpaudio

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// ChannelRef selects one capture generation of one source as one channel of
// an aligned stereo export.
type ChannelRef struct {
	Source     SourceKey `json:"source"`
	Generation uint64    `json:"generation"`
}

// AlignmentPlan describes a two-channel export over the half-open range
// [Start, End). Both channels share the receiver clock's absolute time axis:
// each generation's immutable OutputFrame records are placed by the time they
// were emitted, never packed from the start of the file.
type AlignmentPlan struct {
	Channels []ChannelRef `json:"channels"`
	Start    time.Time    `json:"start"`
	End      time.Time    `json:"end"`
}

// AlignedEvidence describes how one emitted frame contributed to an aligned
// export. Offsets and Count are per-channel 8 kHz sample positions.
type AlignedEvidence struct {
	Source       SourceKey `json:"source"`
	Generation   uint64    `json:"generation"`
	FrameIndex   uint64    `json:"frame_index"`
	Missing      bool      `json:"missing"`
	SourceOffset int       `json:"source_offset"`
	OutputOffset int       `json:"output_offset"`
	Count        int       `json:"count"`
}

// AlignedOutput is one aligned stereo export: interleaved PCM16 WAV bytes
// plus the per-frame evidence in channel order.
type AlignedOutput struct {
	WAV      []byte
	Evidence []AlignedEvidence
	Samples  int
}

const (
	// sampleDuration is the export sample grid at ClockRate.
	sampleDuration = time.Second / ClockRate
	// maxAlignedSpan is the longest range one aligned export may cover.
	maxAlignedSpan = 10 * time.Second
)

// frameSegment is one frame's clipped contribution to one export channel.
type frameSegment struct {
	frame        OutputFrame
	sourceOffset int
	outputOffset int
	count        int
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

// Aligned exports exactly two ordered channels over [plan.Start, plan.End).
// Each channel is one explicit {source, generation} pair whose immutable
// output records are aligned by their emitted timestamps: audio before,
// between, and after recorded frames is silence, missing frames stay silent,
// and frames cut by the range are clipped per sample. Late packets never
// modify emitted records, so the export always reflects what was actually
// played. The whole export fails when a generation is unknown, channels are
// duplicated, the range does not fit the 8 kHz sample grid, an intersecting
// frame is off the grid, or records overlap.
func (r *Receiver) Aligned(plan AlignmentPlan) (AlignedOutput, error) {
	n, err := validateAlignmentPlan(plan)
	if err != nil {
		return AlignedOutput{}, err
	}
	snapshots, err := r.snapshotGenerations(plan.Channels)
	if err != nil {
		return AlignedOutput{}, err
	}

	out := AlignedOutput{WAV: alignedHeader(n), Samples: n, Evidence: []AlignedEvidence{}}
	for channel, ref := range plan.Channels {
		segments, err := alignSegments(snapshots[channel], plan.Start, n)
		if err != nil {
			return AlignedOutput{}, fmt.Errorf("source %q generation %d: %w", ref.Source, ref.Generation, err)
		}
		for _, seg := range segments {
			for j := 0; j < seg.count; j++ {
				sample := uint16(seg.frame.Samples[seg.sourceOffset+j])
				binary.LittleEndian.PutUint16(out.WAV[44+(seg.outputOffset+j)*4+channel*2:], sample)
			}
			out.Evidence = append(out.Evidence, AlignedEvidence{
				Source:       ref.Source,
				Generation:   ref.Generation,
				FrameIndex:   seg.frame.Index,
				Missing:      seg.frame.Missing,
				SourceOffset: seg.sourceOffset,
				OutputOffset: seg.outputOffset,
				Count:        seg.count,
			})
		}
	}
	return out, nil
}

func validateAlignmentPlan(plan AlignmentPlan) (int, error) {
	if len(plan.Channels) != 2 {
		return 0, fmt.Errorf("aligned export needs exactly 2 channels, got %d", len(plan.Channels))
	}
	for _, ref := range plan.Channels {
		if ref.Generation == 0 {
			return 0, fmt.Errorf("source %q: aligned export requires an explicit positive generation ID", ref.Source)
		}
	}
	if plan.Channels[0] == plan.Channels[1] {
		return 0, fmt.Errorf("aligned export channels must be distinct, got %q generation %d twice",
			plan.Channels[0].Source, plan.Channels[0].Generation)
	}
	span := plan.End.Sub(plan.Start)
	if span <= 0 {
		return 0, fmt.Errorf("aligned export range must be positive, got %v", span)
	}
	if span > maxAlignedSpan {
		return 0, fmt.Errorf("aligned export range %v exceeds the %v limit", span, maxAlignedSpan)
	}
	if span%sampleDuration != 0 {
		return 0, fmt.Errorf("aligned export range %v is not an integer number of %v samples", span, sampleDuration)
	}
	return int(span / sampleDuration), nil
}

// snapshotGenerations copies the emitted records of every planned channel
// under one lock so the whole export reflects a single receiver state.
func (r *Receiver) snapshotGenerations(refs []ChannelRef) ([][]OutputFrame, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([][]OutputFrame, len(refs))
	for i, ref := range refs {
		g, err := r.findGeneration(ref.Source, ref.Generation)
		if err != nil {
			return nil, err
		}
		frames := make([]OutputFrame, len(g.frames))
		copy(frames, g.frames)
		out[i] = frames
	}
	return out, nil
}

// alignSegments clips emitted frames onto the start-relative sample grid of
// length n. Frames outside [0, n) contribute nothing; every intersecting
// frame must start on the grid, and no two records may overlap.
func alignSegments(frames []OutputFrame, start time.Time, n int) ([]frameSegment, error) {
	order := make([]int, len(frames))
	for i := range order {
		order[i] = i
	}
	sort.Slice(order, func(a, b int) bool { return frames[order[a]].At.Before(frames[order[b]].At) })

	var segments []frameSegment
	var prevEnd time.Time
	for i, idx := range order {
		f := frames[idx]
		if i > 0 && f.At.Before(prevEnd) {
			return nil, fmt.Errorf("output records overlap: frame %d at %v starts before %v", f.Index, f.At, prevEnd)
		}
		prevEnd = f.At.Add(FrameDuration)

		delta := f.At.Sub(start)
		first := int64(delta / sampleDuration)
		if first >= int64(n) || first+SamplesPerPacket <= 0 {
			continue // entirely outside the export range
		}
		if delta%sampleDuration != 0 {
			return nil, fmt.Errorf("frame %d emitted at %v does not align to the export sample grid", f.Index, f.At)
		}
		sourceOffset := 0
		outputOffset := int(first)
		if outputOffset < 0 {
			sourceOffset = -outputOffset
			outputOffset = 0
		}
		count := SamplesPerPacket - sourceOffset
		if outputOffset+count > n {
			count = n - outputOffset
		}
		segments = append(segments, frameSegment{
			frame:        f,
			sourceOffset: sourceOffset,
			outputOffset: outputOffset,
			count:        count,
		})
	}
	return segments, nil
}

// ExportAligned writes the aligned stereo WAV and its evidence JSON into
// dir, mirroring ExportGeneration for the mono per-generation exports.
func (r *Receiver) ExportAligned(dir string, plan AlignmentPlan) (wavPath, evidencePath string, err error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", "", err
	}
	out, err := r.Aligned(plan)
	if err != nil {
		return "", "", err
	}
	wavPath = filepath.Join(dir, "aligned.wav")
	evidencePath = filepath.Join(dir, "aligned.json")
	if err := os.WriteFile(wavPath, out.WAV, 0o644); err != nil {
		return "", "", err
	}
	data, err := json.MarshalIndent(out.Evidence, "", "  ")
	if err != nil {
		return "", "", err
	}
	data = append(data, '\n')
	if err := os.WriteFile(evidencePath, data, 0o644); err != nil {
		return "", "", err
	}
	return wavPath, evidencePath, nil
}
