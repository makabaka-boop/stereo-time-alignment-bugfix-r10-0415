package rtpaudio

import (
	"encoding/binary"
	"fmt"
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

const sampleDuration = time.Second / ClockRate

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
func (r *Receiver) Aligned(plan AlignmentPlan) (AlignedOutput, error) {
	if len(plan.Channels) != 2 || !plan.End.After(plan.Start) || plan.End.Sub(plan.Start) > 10*time.Second {
		return AlignedOutput{}, fmt.Errorf("invalid plan")
	}
	n := int(plan.End.Sub(plan.Start) / sampleDuration)
	out := AlignedOutput{WAV: alignedHeader(n), Samples: n, Evidence: []AlignedEvidence{}}
	for channel, ref := range plan.Channels {
		frames, err := r.Frames(ref.Source, 0)
		if err != nil {
			return AlignedOutput{}, err
		}
		index := 0
		for _, f := range frames {
			count := min(SamplesPerPacket, n-index)
			if count <= 0 {
				break
			}
			for j := 0; j < count; j++ {
				binary.LittleEndian.PutUint16(out.WAV[44+(index+j)*4+channel*2:], uint16(f.Samples[j]))
			}
			out.Evidence = append(out.Evidence, AlignedEvidence{ref.Source, ref.Generation, f.Index, f.Missing, 0, index, count})
			index += count
		}
	}
	return out, nil
}
