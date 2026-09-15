package answerbench

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"time"
)

const datasetFormatV1 = "demarkus-answer-dataset-v1"

// DatasetManifest pins every input needed to reproduce one independent cohort.
type DatasetManifest struct {
	Format                string `json:"format"`
	ID                    string `json:"id"`
	Source                string `json:"source"`
	ArchiveManifestSHA256 string `json:"archive_manifest_sha256"`
	CorpusSHA256          string `json:"corpus_sha256"`
	TasksSHA256           string `json:"tasks_sha256"`
	RubricSHA256          string `json:"rubric_sha256"`
	ScoringVersion        string `json:"scoring_version"`
	ToolProfile           string `json:"tool_profile"`
	ReaderPolicy          string `json:"reader_policy"`
	ReaderContractSHA256  string `json:"reader_contract_sha256"`
	Authored              string `json:"authored"`
}

func loadDataset(directory string, fixture *Fixture) error {
	datasetPath := filepath.Join(directory, "dataset.json")
	raw, err := readScorerFile(fixture.StoreRoot, datasetPath, 64<<10)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var dataset DatasetManifest
	if err := decodeJSON(raw, &dataset); err != nil {
		return fmt.Errorf("decode dataset: %w", err)
	}
	manifestRaw, err := readScorerFile(fixture.StoreRoot, filepath.Join(directory, "manifest.json"), 64<<10)
	if err != nil {
		return fmt.Errorf("read corpus manifest: %w", err)
	}
	var manifest CorpusManifest
	if err := decodeJSON(manifestRaw, &manifest); err != nil {
		return fmt.Errorf("decode corpus manifest: %w", err)
	}
	if err := validateCorpusManifest(&manifest); err != nil {
		return err
	}
	if err := validateDataset(&dataset, &manifest, fixture, manifestRaw); err != nil {
		return err
	}
	fixture.Dataset = &dataset
	fixture.Hashes["dataset"] = digest(raw)
	fixture.Hashes["archive_manifest"] = digest(manifestRaw)
	return nil
}

func validateDataset(dataset *DatasetManifest, manifest *CorpusManifest, fixture *Fixture, manifestRaw []byte) error {
	origin, err := url.Parse(dataset.Source)
	if err != nil || origin.Scheme != "mark" || origin.Host == "" || origin.Path != "" || origin.RawQuery != "" || origin.Fragment != "" || origin.User != nil {
		return errors.New("dataset requires a root mark:// source")
	}
	if dataset.Format != datasetFormatV1 || dataset.ID == "" || path.Base(dataset.ID) != dataset.ID {
		return errors.New("invalid answer dataset format or id")
	}
	if dataset.ArchiveManifestSHA256 != digest(manifestRaw) || dataset.CorpusSHA256 != fixture.Hashes["corpus"] || dataset.TasksSHA256 != fixture.Hashes["tasks"] || dataset.RubricSHA256 != fixture.Hashes["rubric"] {
		return errors.New("answer dataset input hash mismatch")
	}
	if !validIndependentScoringVersion(dataset.ScoringVersion) || dataset.ToolProfile != "scoped-direct-read-v1" {
		return errors.New("answer dataset scorer or tool profile mismatch")
	}
	contract, err := readerContract(dataset.ReaderPolicy)
	if err != nil || dataset.ReaderContractSHA256 != digest([]byte(contract)) {
		return errors.New("answer dataset reader contract mismatch")
	}
	if _, err := time.Parse(time.DateOnly, dataset.Authored); err != nil {
		return errors.New("answer dataset requires an authoring date")
	}
	if !corpusManifestMatches(dataset, manifest, fixture) {
		return errors.New("answer dataset corpus manifest mismatch")
	}
	return nil
}

func corpusManifestMatches(dataset *DatasetManifest, manifest *CorpusManifest, fixture *Fixture) bool {
	return manifest.ID == dataset.ID && manifest.Source == dataset.Source && manifest.CorpusSHA256 == dataset.CorpusSHA256 &&
		manifest.Documents == len(fixture.storedVersions) && manifest.ActiveDocuments == len(fixture.Documents) && manifest.Versions == fixture.VersionCount
}
