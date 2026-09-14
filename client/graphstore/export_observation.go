package graphstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"

	"github.com/latebit-io/demarkus/client/graph"
)

const observationSection = "\n## Source observations\n\n```json\n"

type exportObservation struct {
	URL         string            `json:"url"`
	Observation graph.Observation `json:"observation"`
}

func writeExportObservations(b *strings.Builder, nodes []StoredNode) {
	var observations []exportObservation
	for i := range nodes {
		node := &nodes[i]
		if node.Observation != (graph.Observation{}) {
			observations = append(observations, exportObservation{node.URL, node.Observation})
		}
	}
	if len(observations) == 0 {
		return
	}
	encoded, err := json.Marshal(observations)
	if err != nil {
		log.Printf("warning: marshal graph source observations: %v", err)
		b.WriteString("\nSource observations unavailable: invalid observation time.\n")
		return
	}
	b.WriteString(observationSection)
	b.Write(encoded)
	b.WriteString("\n```\n")
}

func readExportObservations(body string, nodes []StoredNode) error {
	hasSection := false
	for line := range strings.SplitSeq(body, "\n") {
		if strings.TrimSpace(line) == "## Source observations" {
			hasSection = true
			break
		}
	}
	if !hasSection {
		return nil
	}
	_, payload, found := strings.Cut(body, observationSection)
	if !found || strings.Count(body, observationSection) != 1 || !strings.HasSuffix(payload, "\n```\n") {
		return errors.New("invalid graph source observations section")
	}
	payload = strings.TrimSuffix(payload, "\n```\n")
	if err := validateSnapshotJSON(payload); err != nil {
		return err
	}
	var observations []exportObservation
	decoder := json.NewDecoder(strings.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&observations); err != nil {
		return fmt.Errorf("graph observations: %w", err)
	}
	byURL := make(map[string]int, len(nodes))
	for i := range nodes {
		byURL[nodes[i].URL] = i
	}
	seen := make(map[string]bool, len(observations))
	for rowIndex := range observations {
		row := &observations[rowIndex]
		i, ok := byURL[row.URL]
		if !ok || seen[row.URL] {
			return fmt.Errorf("invalid graph observation source row %q", row.URL)
		}
		if err := validateObservation(&row.Observation); err != nil {
			return err
		}
		seen[row.URL] = true
		nodes[i].Observation = row.Observation
		nodes[i].Etag = row.Observation.Etag
	}
	return nil
}

func validateObservation(o *graph.Observation) error {
	if *o == (graph.Observation{}) {
		return nil
	}
	if o.View != "" && !o.KnownView() {
		return errors.New("unknown graph extraction view")
	}
	if o.Source != "" {
		if err := validateSnapshotURL(o.Source); err != nil {
			return err
		}
		if !strings.HasPrefix(o.Source, "mark://") {
			return errors.New("graph observation source must be mark://")
		}
	}
	if o.Revision < 0 || o.HighestRevision < 0 || (o.HighestRevision > 0 && (o.Source == "" || o.HighestRevision < o.Revision)) || (o.Revision > 0 && o.Source == "") || (o.Complete && (o.Source == "" || o.ObservedAt.IsZero())) {
		return errors.New("invalid graph source revision or completeness")
	}
	if len(o.Etag) > 256 || len(o.Problem) > 64 || strings.ContainsAny(o.Problem, "\r\n[]") {
		return errors.New("invalid graph observation etag or problem")
	}
	return nil
}
