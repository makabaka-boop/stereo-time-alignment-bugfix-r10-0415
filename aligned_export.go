package rtpaudio

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// ExportAligned validates and renders the complete stereo export before
// writing aligned.wav and aligned.json in dir. Invalid plans or record
// histories therefore do not create a partial export.
func (r *Receiver) ExportAligned(dir string, plan AlignmentPlan) (wavPath, evidencePath string, err error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", "", err
	}
	output, err := r.Aligned(plan)
	if err != nil {
		return "", "", err
	}
	evidence, err := json.MarshalIndent(output.Evidence, "", "  ")
	if err != nil {
		return "", "", err
	}
	evidence = append(evidence, '\n')

	wavPath = filepath.Join(dir, "aligned.wav")
	evidencePath = filepath.Join(dir, "aligned.json")

	tempWAV, err := writeTempFile(dir, ".aligned-*.wav", output.WAV)
	if err != nil {
		return "", "", err
	}
	defer os.Remove(tempWAV)

	tempEvidence, err := writeTempFile(dir, ".aligned-*.json", evidence)
	if err != nil {
		return "", "", err
	}
	defer os.Remove(tempEvidence)

	if err := os.Rename(tempWAV, wavPath); err != nil {
		return "", "", err
	}
	if err := os.Rename(tempEvidence, evidencePath); err != nil {
		return "", "", err
	}
	return wavPath, evidencePath, nil
}

func writeTempFile(dir, pattern string, data []byte) (string, error) {
	file, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return "", err
	}
	name := file.Name()
	if _, err := file.Write(data); err != nil {
		file.Close()
		os.Remove(name)
		return "", err
	}
	if err := file.Chmod(0o644); err != nil {
		file.Close()
		os.Remove(name)
		return "", err
	}
	if err := file.Close(); err != nil {
		os.Remove(name)
		return "", err
	}
	return name, nil
}
