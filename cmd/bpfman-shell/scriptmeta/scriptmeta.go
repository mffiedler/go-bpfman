// Package scriptmeta reads bpfman-shell script header metadata.
package scriptmeta

import (
	"bufio"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"strings"

	k8slabels "k8s.io/apimachinery/pkg/labels"
)

const (
	// HeaderLines bounds the script header scan. Metadata belongs at
	// the top of the file, not buried in executable script body.
	HeaderLines = 20

	PragmaMarker = "#pragma"
	LabelsPragma = "labels"
)

type Mode struct {
	Labels k8slabels.Set
}

func Read(path string) (Mode, error) {
	f, err := os.Open(path)
	if err != nil {
		return Mode{}, fmt.Errorf("open script %s for header prescan: %w", path, err)
	}
	defer f.Close()
	return Scan(f, path)
}

func Scan(f *os.File, path string) (Mode, error) {
	mode := Mode{Labels: make(k8slabels.Set)}
	scanner := bufio.NewScanner(f)
	for i := 0; i < HeaderLines && scanner.Scan(); i++ {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, PragmaMarker) {
			if err := ParsePragma(path, line, mode.Labels); err != nil {
				return Mode{}, err
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return Mode{}, fmt.Errorf("read script %s for header prescan: %w", path, err)
	}
	return mode, nil
}

func ParsePragma(path, line string, labels k8slabels.Set) error {
	body := strings.TrimSpace(strings.TrimPrefix(line, PragmaMarker))
	raw, ok := strings.CutPrefix(body, LabelsPragma+"=")
	if !ok {
		return nil
	}
	parsed, err := ParseLabelPragma(path, strings.TrimSpace(raw))
	if err != nil {
		return err
	}
	maps.Copy(labels, parsed)
	return nil
}

func ParseLabelPragma(path, raw string) (k8slabels.Set, error) {
	var labelMap map[string]string
	if err := json.Unmarshal([]byte(raw), &labelMap); err == nil {
		return NormaliseLabelMap(labelMap), nil
	}

	var labels []string
	if err := json.Unmarshal([]byte(raw), &labels); err == nil {
		return LabelsToSet(labels), nil
	}

	var csv string
	if err := json.Unmarshal([]byte(raw), &csv); err != nil {
		return nil, fmt.Errorf("parse %s labels pragma %q: expected JSON string, JSON string array, or JSON object: %w", path, raw, err)
	}
	return LabelsToSet(SplitLabelList(csv)), nil
}

func SplitLabelList(raw string) []string {
	if raw == "" {
		return nil
	}
	return NormaliseLabels(strings.Split(raw, ","))
}

func LabelsToSet(labels []string) k8slabels.Set {
	out := make(k8slabels.Set, len(labels))
	for _, label := range labels {
		out[label] = "true"
	}
	return out
}

func NormaliseLabelMap(labels map[string]string) k8slabels.Set {
	out := make(k8slabels.Set, len(labels))
	for key, value := range labels {
		key = strings.ToLower(strings.TrimSpace(key))
		if key == "" {
			continue
		}
		out[key] = strings.ToLower(strings.TrimSpace(value))
	}
	return out
}

func NormaliseLabels(labels []string) []string {
	out := make([]string, 0, len(labels))
	seen := make(map[string]bool, len(labels))
	for _, label := range labels {
		label = strings.ToLower(strings.TrimSpace(label))
		if label == "" || seen[label] {
			continue
		}
		seen[label] = true
		out = append(out, label)
	}
	return out
}
